package random

import (
	"sync"
	"time"
)

// IRandom 是包内发生器的取值契约。
type IRandom interface {
	nextInt64() int64
}

var (
	_ IRandom = (*MersenneTwister)(nil)
	_ IRandom = (*MonteCarlo)(nil)
)

// Random 把两路发生器（MT19937-64 与 xoshiro256**）各取一次值再异或，
// 用以削弱任一路在低位上的周期性，值域为 [0, 1<<63)，幅度均匀。
//
// 零值可直接使用：首次取值会就地补齐引擎，不会 panic。
// 靠自身互斥锁保护，可并发使用。
//
// 锁纪律：mt 与 mc 是本实例的私产，构造后指针不再改写、也不外泄给任何调用方，
// 因此取值路径只拿 r.mu 一把锁，直接调引擎要求持锁的内部方法；
// 引擎自身的对外方法（如 MersenneTwister.Uint64）仍各自加自己的锁，二者互不干扰。
// 唯一的锁序是 r.mu → 引擎锁（建实例时），反向路径不存在，故不会死锁。
type Random struct {
	mu   sync.Mutex
	mt   *MersenneTwister
	mc   *MonteCarlo
	seed int64
}

// NextInt64 返回 [0,1<<63) 区间内的非负伪随机 int64，幅度在该值域上均匀。
// 可并发调用；对 nil 接收者或零值实例同样安全。
//
// 两路各出一个非负 63 位值再异或：任一路在某比特位上有周期结构，另一路同位都会把它打散，
// 这是组合两路的唯一目的；而两个独立量异或后的边际分布跟随其中均匀的那一路，
// 所以幅度仍是 [0,1<<63) 上的均匀分布（实测 64 等宽桶卡方见 random_dist_test.go）。
// 早先的组合是 (a+b)>>1 求平均，它把幅度压成以 2^62 为峰的三角分布（中间桶约为边缘桶的 63 倍），
// 只适合「按低位取模」的用法，现已换掉；异或也保住了全部 63 位，右移丢的那一位不必再丢。
//
// 对小的 n 取模时，残余分布的偏斜量级 ≤ 2n/2^63；n 逼近 2^40 以上才需要 rejection 采样。
func (r *Random) NextInt64() int64 {
	if r == nil {
		return NextInt64()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLocked()

	return r.mt.int63Locked() ^ r.mc.int63Locked()
}

// New 用本实例的构造种子原地重播两路引擎。
//
// 旧实现是重建引擎指针并按「已被两路累加漂移过的种子」重播，因此重播结果取决于
// 此前取过多少次值；现在换成原地重播构造种子，同一实例反复 New 得到同一条序列，
// 也不再让并发取值撞到被替换的旧指针。
func (r *Random) New() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.mt == nil {
		r.buildLocked(r.seed)
		return
	}

	r.mt.mu.Lock()
	r.mt.reseedLocked(r.seed)
	r.mt.mu.Unlock()
	r.mc.mu.Lock()
	r.mc.reseedLocked(r.seed)
	r.mc.mu.Unlock()
}

// ensureLocked 在引擎缺失时就地补齐，供零值实例使用。调用前必须已持有 r.mu。
//
// 缺了这一步就是旧实现的宕机点之一：两路都空时 len(r.r) 为 0，取值末尾要除以它，
// 直接抛不可 recover 的 integer divide by zero。
func (r *Random) ensureLocked() {
	if r.mt != nil {
		return
	}
	seed := r.seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	r.buildLocked(seed)
}

// buildLocked 按给定种子建起两路引擎。调用前必须已持有 r.mu。
//
// 种子以私有副本交给每个引擎：两路各自累加出值时写的是自己的盒子，
// 不再共用 r.seed 这一个 int64。
func (r *Random) buildLocked(seed int64) {
	if seed == 0 {
		seed = 1
	}
	r.seed = seed

	mtSeed := seed
	mcSeed := seed
	r.mt = NewMersenneTwister(&mtSeed)
	r.mc = NewMonteCarlo(&mcSeed)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buildLocked(seed)
}

var defaultRandom = sync.OnceValue(func() *Random {
	return New(time.Now().UnixNano())
})

// NextInt64 返回包级默认实例的一个非负伪随机 int64，可并发调用。
// 默认实例按启动时刻播种，只保证不重复，不保证可复现；需要复现请自行 New(seed)。
// 幅度和取模后残余分布的说明见 Random.NextInt64。
func NextInt64() int64 {
	return defaultRandom().NextInt64()
}
