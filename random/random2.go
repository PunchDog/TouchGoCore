package random

import (
	"math"
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

// findNextPrime 返回与 mc.q 互质的最小质数
func findNextPrime(mc int64) int64 {
	for i := int64(0); ; i++ {
		a := int64(math.Pow(5, float64(i)))*4 + 1 //5的幂次方
		if isPrime(a) && areCoprime(a, mc) {
			return int64(math.Pow(5, float64(i)))
		}
	}
}

func NewMonteCarlo(Seed *int64) *MonteCarlo {
	mc := &MonteCarlo{}
	mc.seed = Seed
	mc.init()
	return mc
}

type MonteCarlo struct {
	q        int64
	p        int64
	M        int64
	seed     *int64
	nextTime int64
	tick     int64
}

func (mc *MonteCarlo) init() {
	mc.nextTime = *mc.seed
	mc.M = 1 << ((*mc.seed>>2)&0x7 + 17)
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
	s.tick++
	if s.tick%500 == 0 {
		s.init()
	}
	s.nextTime = (s.nextTime*s.p + s.q) % s.M
	*s.seed += s.nextTime & 0xffff
	return int64(s.nextTime & 0x7fffffffffffffff)
}
