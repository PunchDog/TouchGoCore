package random

import (
	"math"
	"sync/atomic"
	"touchgocore/vars"
)

// isPrime 判断 n 是否为质数
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
func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// areCoprime 判断两个数是否互质
func areCoprime(a, b int64) bool {
	return gcd(a, b) == 1
}

// findNextPrime 返回与 mc 互质的最小质数
func findNextPrime(mc int64) int64 {
	// 设置最大迭代次数，避免无限循环
	const maxIterations = 1000
	
	for i := int64(0); i < maxIterations; i++ {
		base := int64(math.Pow(5, float64(i)))
		a := base*4 + 1 // 5的幂次方 * 4 + 1
		
		if isPrime(a) && areCoprime(a, mc) {
			// 根据注释，应该返回 p = base
			return base
		}
	}
	
	// 如果没有找到符合条件的质数，返回一个安全的默认值
	// 使用 5^4 = 625 作为默认值
	return 625
}

func NewMonteCarlo(Seed *int64) *MonteCarlo {
	mc := &MonteCarlo{}
	if Seed != nil {
		mc.seed.Store(*Seed)
	}
	mc.init()
	return mc
}

type MonteCarlo struct {
	q        int64
	p        int64
	M        int64
	seed     atomic.Int64  // 使用 atomic.Int64 替代指针
	nextTime atomic.Int64  // 使用 atomic.Int64 保证并发安全
	tick     atomic.Int64  // 使用 atomic.Int64 保证并发安全
}

func (mc *MonteCarlo) init() {
	seed := mc.seed.Load()
	mc.nextTime.Store(seed)
	mc.M = 1 << ((seed>>2)&0x7 + 17)
	p := findNextPrime(mc.M)
	mc.q = (p+1)*2 + 1
	mc.p = p*4 + 1
	// initforcnt := 1 << ((mc.p+mc.q)&0xf + 5)
	for i := 0; i < 20; i++ {
		mc.nextInt64()
	}
}

func (s *MonteCarlo) nextInt64() int64 {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("随机数生成失败:%s", err.(error))
		}
	}()
	
	// 原子地增加 tick 计数器
	s.tick.Add(1)
	// 每 500 次调用重新初始化（可选）
	// if s.tick.Load()%500 == 0 {
	// 	s.init()
	// }
	
	// 原子操作获取和更新 nextTime
	next := s.nextTime.Load()
	next = (next*s.p + s.q) % s.M
	s.nextTime.Store(next)
	
	// 原子地更新 seed
	s.seed.Add(next & 0xffff)
	return int64(next & 0x7fffffffffffffff)
}
