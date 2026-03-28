package random

import "sync/atomic"

const (
	N               = 312
	M               = 156
	R               = 31
	W               = 64
	MATRIX_A        = 0xb5026f5aa96619e9
	MASK     uint64 = 6364136223846793005
	U               = 29
	S               = 17
	B               = 0x71d67fffeda60000
	T               = 37
	C               = 0xfff7eee000000000
	L               = 43

	// UPPER_MASK = 1 << R
	// LOWER_MASK = 1<<R - 1
	UPPER_MASK = 0xFFFFFFFFFFFFFFFF & (1<<R - 1)
	LOWER_MASK = 1<<R - 1
)

type MersenneTwister struct {
	mt    [N]uint64
	index atomic.Int32  // 使用 atomic.Int32 替代普通的 int32
	seed  atomic.Int64  // 使用 atomic.Int64 替代指针，避免共享问题
}

func NewMersenneTwister(seed *int64) *MersenneTwister {
	ret := &MersenneTwister{}
	if seed != nil {
		ret.seed.Store(*seed)
	}
	ret.resetMersenneTwister()
	return ret
}

// 重置随机数
func (mt *MersenneTwister) resetMersenneTwister() {
	seed := mt.seed.Load()
	mt.mt = [N]uint64{uint64(seed)}
	for i := 1; i < N; i++ {
		mt.mt[i] = MASK * (mt.mt[i-1] ^ (mt.mt[i-1] >> 62) + uint64(i))
	}

	mt.index.Store(0)
	for i := 0; i < N; i++ {
		mt.nextInt64()
	}
}

func (mt *MersenneTwister) twist() {
	for i := int64(0); i < N; i++ {
		x := (mt.mt[i] & UPPER_MASK) + (mt.mt[(i+1)%N] & LOWER_MASK)
		xA := x >> 1
		if x&0x1 != 0 {
			xA ^= MATRIX_A
		}
		mt.mt[i] = mt.mt[(i+M)%N] ^ xA
	}
	mt.index.Store(0)
}

func (mt *MersenneTwister) nextInt64() int64 {
	// 使用原子操作获取并递增 index
	idx := mt.index.Add(1) - 1
	if idx >= N {
		// 需要重新 twist，但这里可能有竞态条件
		// 多个 goroutine 可能同时进入这个分支
		mt.twist()
		// 重置后重新获取 index
		idx = 0
	}

	y := &mt.mt[idx]
	*y ^= *y >> U
	*y ^= (*y << S) & B
	*y ^= (*y << T) & C
	*y ^= *y >> L

	// 原子地更新 seed
	yVal := *y
	mt.seed.Add(int64(yVal & 0xffff))
	return int64(yVal & 0x7fffffffffffffff)
}
