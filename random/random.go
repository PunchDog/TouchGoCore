package random

import (
	"sync"
	"touchgocore/vars"
)

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
	index int64
	seed  *int64
	lock  sync.Mutex
}

func NewMersenneTwister(seed *int64) *MersenneTwister {
	ret := &MersenneTwister{seed: seed}
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
	cnt := mt.nextInt64()&0x7 + 2
	for i := 0; i < int(cnt)*N; i++ {
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
	mt.index = 0
}

func (mt *MersenneTwister) nextInt64() int64 {
	mt.lock.Lock()
	defer func() {
		mt.lock.Unlock()
		if err := recover(); err != nil {
			vars.Error("随机数生成失败:%s", err.(error))
		}
	}()
	if mt.index >= N {
		mt.twist()
	}

	y := &mt.mt[mt.index]
	*y ^= *y >> U
	*y ^= (*y << S) & B
	*y ^= (*y << T) & C
	*y ^= *y >> L

	mt.index++
	*mt.seed += int64(*y & 0xffff)
	return int64(*y & 0x7fffffffffffffff)
}
