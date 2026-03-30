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
	index int32
	seed  *int64
}

func NewMersenneTwister(seed *int64) *MersenneTwister {
	ret := &MersenneTwister{
		seed: seed,
	}
	ret.resetMersenneTwister()
	return ret
}

// 重置随机数
func (mt *MersenneTwister) resetMersenneTwister() {
	mt.mt = [N]uint64{uint64(*mt.seed)}
	for i := 1; i < N; i++ {
		mt.mt[i] = MASK * (mt.mt[i-1] ^ (mt.mt[i-1] >> 62) + uint64(i))
	}

	mt.index = 0
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
	atomic.StoreInt32(&mt.index, 0)
}

func (mt *MersenneTwister) nextInt64() int64 {
	idx := atomic.AddInt32(&mt.index, 1) - 1
	if idx >= N {
		mt.twist()
		idx = 0
	}

	y := &mt.mt[idx]
	*y ^= *y >> U
	*y ^= (*y << S) & B
	*y ^= (*y << T) & C
	*y ^= *y >> L

	*mt.seed += int64(*y & 0xffff)
	return int64(*y & 0x7fffffffffffffff)
}
