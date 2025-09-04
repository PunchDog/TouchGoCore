package random

import (
	"time"
)

type IRandom interface {
	nextInt64() int64
}

type Random struct {
	r    []IRandom
	seed int64
}

var forcnt int = 2

func (r *Random) NextInt64() int64 {
	rr := int64(0)
	for i := 0; i < forcnt; i++ {
		for _, r1 := range r.r {
			rr += r1.nextInt64()
			rr = rr & 0x7fffffffffffffff
			r.seed = r.seed & 0x7fffffffffffffff
		}
	}
	// r1 := rr << 32
	// r1 = r1 & 0x7fffffffffffffff
	// r2 := rr >> 32
	// rr = r1 | r2
	return rr / (int64(len(r.r) * forcnt))
}

func (r *Random) New() {
	newRandom(r, r.seed)
}

// New 创建并返回一个新的随机数生成器实例。
// 该函数旨在根据给定的种子和必须生成新实例的标志来创建随机数生成器。
// 参数:
//   - seed: 用于初始化随机数生成器的种子。
//
// 返回值:
//   - Random: 随机数生成器接口的实例。
func New(seed int64) *Random {
	r := new(Random)
	newRandom(r, seed)
	return r
}

func newRandom(r *Random, seed int64) {
	r.seed = seed
	r.r = []IRandom{NewMersenneTwister(&r.seed), NewMonteCarlo(&r.seed)}
	// //判断系统是windows
	// if "windows" != runtime.GOOS {
	// 	r.r = append(r.r, NewSystemRandom(&r.seed))
	// 	r.r = append(r.r, NewSystemRandom(&r.seed))
	// } else {
	// 	r.r = append(r.r, NewMersenneTwister(&r.seed))
	// 	r.r = append(r.r, NewMonteCarlo(&r.seed))
	// }
}

var _defautlRandom *Random = nil

func NextInt64() int64 {
	if _defautlRandom == nil {
		_defautlRandom = New(time.Now().UnixNano())
	}
	return _defautlRandom.NextInt64()
}
