package random

import (
	"math"
	"math/bits"
	"sync"
)

// MonteCarlo 是包内的第二路伪随机发生器：256 位状态的 xoshiro256**，
// 种子由 splitmix64 展开成状态。名字沿用历史，它与蒙特卡洛方法无关。
//
// 它替代了原先的自写线性同余实现：那套参数里 findNextPrime 对任何 2 的幂模数恒在
// 第一次尝试命中，于是乘数与增量恒为 5、模数只有 17~24 位，低位几乎是噪声，
// 而随机数的常见用法恰恰是按低位取模。
//
// 零值可用（首次取值自行播种，只会恒返回 0 而不 panic）；靠互斥锁保护，可并发使用。
type MonteCarlo struct {
	// M 是输出值域上界（含），恒为 math.MaxInt64：取值一律折进非负 63 位。
	// 旧实现里它是同余模数（1<<k，k∈[17,24]）；换成 64 位发生器后模数为 2^64，
	// 无法用正的 int64 表示，故这里改记输出上界。
	M int64

	mu    sync.Mutex
	seed  *int64
	st    [4]uint64
	ready bool
}

// NewMonteCarlo 以给定指针的值作种子创建发生器。
// 指针为空时改用实例私有的种子盒子，不解引用空指针。
func NewMonteCarlo(Seed *int64) *MonteCarlo {
	mc := &MonteCarlo{}
	if Seed == nil {
		own := int64(0)
		Seed = &own
	}
	mc.seed = Seed
	mc.init(*Seed)
	return mc
}

// init 用给定种子展开并装入状态，是播种的内部实现。
func (mc *MonteCarlo) init(seed int64) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	mc.reseedLocked(seed)
}

// reseedLocked 按给定种子重播状态。调用前必须已持有 mc.mu。
//
// 全程只有加法、移位与乘法，无除法、无取模、无解引用，故取值路径可证明不会 panic，
// 也就不再需要旧实现那层 defer/recover（它会把真缺陷连同日志一起吞掉）。
func (mc *MonteCarlo) reseedLocked(seed int64) {
	if mc.seed == nil {
		mc.seed = new(int64)
	}
	*mc.seed = seed

	x := uint64(seed)
	for i := range mc.st {
		mc.st[i] = splitMix64(&x)
	}
	if mc.st[0]|mc.st[1]|mc.st[2]|mc.st[3] == 0 {
		mc.st[0] = 1 // 全零状态会自我复制，兜一个非零位
	}
	mc.M = math.MaxInt64
	mc.ready = true
}

// splitMix64 按 splitmix64 递推产出一个状态字，连续调用即可填满 xoshiro 的四字状态。
func splitMix64(x *uint64) uint64 {
	*x += 0x9e3779b97f4a7c15
	z := *x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// nextUint64Locked 推进状态并返回一个 64 位原字。调用前必须已持有 mc.mu。
//
// 取 xoshiro256** 而非 ++：结果还要与非负化的 MT 取值异或后由调用方按低位取模，
// 而 ++ 的最低位存在短周期线性关系。
func (mc *MonteCarlo) nextUint64Locked() uint64 {
	result := bits.RotateLeft64(mc.st[1]*5, 7) * 9

	t := mc.st[1] << 17
	mc.st[2] ^= mc.st[0]
	mc.st[3] ^= mc.st[1]
	mc.st[1] ^= mc.st[2]
	mc.st[0] ^= mc.st[3]
	mc.st[2] ^= t
	mc.st[3] = bits.RotateLeft64(mc.st[3], 45)

	return result
}

// int63Locked 返回非负 63 位取值。调用前必须已持有 mc.mu。
func (mc *MonteCarlo) int63Locked() int64 {
	return int64(mc.nextUint64Locked() >> 1)
}

// nextInt64 供 IRandom 契约使用：返回 [0,M] 区间内的非负伪随机 int64，并推进种子。
func (mc *MonteCarlo) nextInt64() int64 {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if !mc.ready {
		mc.reseedLocked(0)
	}
	result := mc.nextUint64Locked()
	*mc.seed += int64(result & 0xffff)

	return int64(result >> 1)
}

// isPrime 判断 n 是否为质数
//
// 现仅由本包测试引用：换掉同余参数后，发生器不再需要搜索乘数。
func isPrime(n int64) bool {
	if n <= 1 {
		return false
	}
	for i := int64(2); i*i <= n; i++ {
		if n%i == 0 {
			return false
		}
	}
	return true
}

// gcd 计算两个数的最大公约数
//
// 现仅由本包测试引用。
func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// areCoprime 判断两个数是否互质
//
// 现仅由本包测试引用。
func areCoprime(a, b int64) bool {
	return gcd(a, b) == 1
}
