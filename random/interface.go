package random

import (
	"sync"
	"sync/atomic"
	"time"
)

type IRandom interface {
	nextInt64() int64
}

type Random struct {
	r    []IRandom
	seed atomic.Int64  // 使用 atomic.Int64 保证并发安全
	mu   sync.RWMutex  // 保护 r 数组的访问（如果需要动态修改）
}

func (r *Random) NextInt64() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	if len(r.r) == 0 {
		return 0
	}
	
	rr := int64(0)
	for _, r1 := range r.r {
		rr += r1.nextInt64()
		rr = rr & 0x7fffffffffffffff
	}
	
	// 原子地更新 seed（可选，保持向后兼容）
	currentSeed := r.seed.Load()
	r.seed.Store(currentSeed & 0x7fffffffffffffff)
	
	return rr / int64(len(r.r))
}

func (r *Random) New() {
	newRandom(r, r.seed.Load())
}

// New 创建并返回一个新的随机数生成器实例。
// 该函数旨在根据给定的种子和必须生成新实例的标志来创建随机数生成器。
// 参数:
//   - seed: 用于初始化随机数生成器的种子。
//
// 返回值:
//   - Random: 随机数生成器接口的实例。
func New(seed int64) *Random {
	r := &Random{}
	newRandom(r, seed)
	return r
}

func newRandom(r *Random, seed int64) {
	r.seed.Store(seed)
	
	// 为每个生成器创建独立的 seed 副本，避免共享指针导致的并发问题
	// 注意：现在 NewMersenneTwister 和 NewMonteCarlo 已经使用原子类型，
	// 不再需要传递指针，但为了保持接口兼容性，我们仍然传递 nil
	r.mu.Lock()
	defer r.mu.Unlock()
	
	// 创建两个独立的随机数生成器
	mt := NewMersenneTwister(nil)
	mtSeed := seed
	mt.seed.Store(mtSeed)
	mt.resetMersenneTwister()
	
	mc := NewMonteCarlo(nil)
	mcSeed := seed
	mc.seed.Store(mcSeed)
	mc.init()
	
	r.r = []IRandom{mt, mc}
}

var (
	_defautlRandom *Random = nil
	once           sync.Once  // 用于安全地初始化默认随机数生成器
)

func NextInt64() int64 {
	once.Do(func() {
		_defautlRandom = New(time.Now().UnixNano())
	})
	return _defautlRandom.NextInt64()
}

// RandInt 返回 [0, max) 范围的随机 int64
func RandInt(max int64) int64 {
	if max == 0 {
		return 0
	}
	return NextInt64()
}

// RandRange 返回 [min, max) 或 [max, min) 范围的随机 int64
func RandRange(max int64, min int64) (ret int64) {
	if max-min == 0 {
		ret = min
	} else if max-min > 0 {
		ret = int64(RandInt(int64(max-min))) + min
	} else {
		// max < min，交换边界
		min = min + 1
		ret = int64(RandInt(int64(min-max))) + max
	}
	return
}
