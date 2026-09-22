package random

import (
	"sync"
)

const (
	// MT19937-64 算法参数
	N        = 312                 // 状态字数量
	M        = 156                 // 中间词，递推关系的偏移
	R        = 31                  // 单字的分割点，即低位掩码的位数
	W        = 64                  // 字长（位数）
	MATRIX_A = 0xb5026f5aa96619e9  // 常量矩阵 a
	MASK     = 6364136223846793005 // 初始化乘数
	U        = 29                  // 右移扰动量，取高位 w-r 位
	S        = 17                  // 升温（tempering）左移位量，作用于中间位
	B        = 0x71d67fffeda60000  // 升温掩码，作用于中间位
	T        = 37                  // 升温左移位量，作用于中间位
	C        = 0xfff7eee000000000  // 升温掩码，作用于中间位
	L        = 43                  // 右移扰动量，取低位

	// 高位与低位掩码
	UPPER_MASK = 0xFFFFFFFF80000000 // 高 R 位
	LOWER_MASK = 0x7FFFFFFF         // 低 W-R 位
)

// MersenneTwister 是梅森旋转（Mersenne Twister）MT19937-64 伪随机数发生器，
// 周期为 2^19937-1。
//
// 靠互斥锁保护，可并发使用。
//
// 示例：
//
//	seed := int64(42)
//	mt := NewMersenneTwister(&seed)
//	rand := mt.Int63()
type MersenneTwister struct {
	mu    sync.Mutex
	mt    [N]uint64
	index int
	seed  *int64
}

// NewMersenneTwister 以给定指针的值作种子创建发生器。
// 种子指针本身被保存下来，因此外部改写的动作对后续 Seed 仍然有效。
// *seed 为 0 时改用非零值，以避开全零的退化状态。
func NewMersenneTwister(seed *int64) *MersenneTwister {
	if seed == nil || *seed == 0 {
		// 指针为空或值为零时换用一个非零种子
		if seed == nil {
			defaultSeed := int64(1)
			seed = &defaultSeed
		} else {
			*seed = 1
		}
	}
	mt := &MersenneTwister{
		seed: seed,
	}
	mt.init(*seed)
	return mt
}

// init 用给定种子初始化状态数组，是初始化算法的内部实现。
func (mt *MersenneTwister) init(seed int64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	mt.mt[0] = uint64(seed)
	for i := 1; i < N; i++ {
		// 初始化递推式：mt[i] = (MASK * (mt[i-1] ^ (mt[i-1] >> (W-2))) + i)
		mt.mt[i] = MASK*(mt.mt[i-1]^(mt.mt[i-1]>>(W-2))) + uint64(i)
	}
	mt.index = N // 置成 N，首次取值即触发生成
}

// Seed 用新种子重新初始化发生器。
// 可并发调用；若需要频繁换种子，更建议直接新建一个发生器而不是反复重播。
//
// seed 指向的值为 0 时改用非零值，以避开全零的退化状态。
func (mt *MersenneTwister) Seed(seed *int64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	if *seed == 0 {
		*seed = 1
	}
	if mt.seed != nil {
		mt.seed = seed
	}

	mt.mt[0] = uint64(*seed)
	for i := 1; i < N; i++ {
		mt.mt[i] = MASK*(mt.mt[i-1]^(mt.mt[i-1]>>(W-2))) + uint64(i)
	}
	mt.index = N
}

// twist 执行梅森旋转的核心变换，一次性把状态数组里的 N 个数全部刷新。
// 调用前必须已持有写锁。
func (mt *MersenneTwister) twist() {
	for i := 0; i < N; i++ {
		// 用高/低位掩码把相邻两字拼成 x
		x := (mt.mt[i] & UPPER_MASK) | (mt.mt[(i+1)%N] & LOWER_MASK)

		// 计算 xA
		xA := x >> 1
		if x&1 != 0 {
			xA ^= MATRIX_A
		}

		// 生成新的状态值
		mt.mt[i] = mt.mt[(i+M)%N] ^ xA
	}
	mt.index = 0
}

// nextUint64 产出下一个 uint64 随机数，返回未再做位掩码的原始值。
// 调用前必须已持有锁。
func (mt *MersenneTwister) nextUint64() uint64 {
	// 状态数组取空则补一轮
	if mt.index >= N {
		mt.twist()
	}

	y := mt.mt[mt.index]
	mt.index++

	// 升温变换，改善分布的等均匀性
	y ^= y >> U
	y ^= (y << S) & B
	y ^= (y << T) & C
	y ^= y >> L

	return y
}

// Uint64 返回一个伪随机的 uint64，是其余取值方法的公共核心。
func (mt *MersenneTwister) Uint64() uint64 {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	result := mt.nextUint64()

	// 把出值累加进种子，沿用原实现的种子跟踪语义
	*mt.seed += int64(result & 0xffff)

	return result
}

// Int63 返回 [0, 1<<63-1] 区间内的非负伪随机 int64。
func (mt *MersenneTwister) Int63() int64 {
	return int64(mt.Uint64() >> 1)
}

// Uint32 返回一个伪随机的 uint32。
func (mt *MersenneTwister) Uint32() uint32 {
	return uint32(mt.Uint64() >> 32)
}

// Int31 返回 [0, 1<<31-1] 区间内的非负伪随机 int32。
func (mt *MersenneTwister) Int31() int32 {
	return int32(mt.Uint64() >> 33)
}

// Intn 返回 [0,n) 区间内的非负伪随机 int；n <= 0 时 panic。
func (mt *MersenneTwister) Intn(n int) int {
	if n <= 0 {
		panic("Intn: n must be positive")
	}
	return int(mt.Uint64() % uint64(n))
}

// Int63n 返回 [0,n) 区间内的非负伪随机 int64；n <= 0 时 panic。
func (mt *MersenneTwister) Int63n(n int64) int64 {
	if n <= 0 {
		panic("Int63n: n must be positive")
	}
	return mt.Int63() % n
}

// Uint64n 返回 [0,n) 区间内的伪随机 uint64；n 为 0 时 panic。
// 取模分布，n 不是 2 的幂时高位偏斜，需要严格均匀请自行用 rejection 采样。
func (mt *MersenneTwister) Uint64n(n uint64) uint64 {
	if n <= 0 {
		panic("Uint64n: n must be positive")
	}
	return mt.Uint64() % n
}

// Float64 返回 [0.0,1.0) 区间内的伪随机 float64。
func (mt *MersenneTwister) Float64() float64 {
	return float64(mt.Int63()) / (1 << 63)
}

// Float32 返回 [0.0,1.0) 区间内的伪随机 float32。
func (mt *MersenneTwister) Float32() float32 {
	return float32(mt.Uint64()>>32) / (1 << 32)
}

// Float32Range 返回 [min,max) 区间内的伪随机 float32；max <= min 时 panic。
func (mt *MersenneTwister) Float32Range(min, max float32) float32 {
	if max <= min {
		panic("Float32Range: max must be greater than min")
	}
	return min + mt.Float32()*(max-min)
}

// Float64Range 返回 [min,max) 区间内的伪随机 float64；max <= min 时 panic。
func (mt *MersenneTwister) Float64Range(min, max float64) float64 {
	if max <= min {
		panic("Float64Range: max must be greater than min")
	}
	return min + mt.Float64()*(max-min)
}

// resetMersenneTwister 用当前种子重置发生器，仅为向后兼容保留。
func (mt *MersenneTwister) resetMersenneTwister() {
	mt.init(*mt.seed)
}

// nextInt64 供 IRandom 接口向后兼容使用：返回非负 63 位整数，并推进内部种子。
func (mt *MersenneTwister) nextInt64() int64 {
	return mt.Int63()
}
