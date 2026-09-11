package random

import "testing"

func TestNewMersenneTwister_NilSeed(t *testing.T) {
	mt := NewMersenneTwister(nil)
	if mt == nil {
		t.Fatal("nil 种子应使用回退非零值，不应返回 nil")
	}
	// 第一次取值应成功
	if v := mt.Int63(); v < 0 {
		t.Fatalf("Int63 应非负，实际: %d", v)
	}
}

func TestNewMersenneTwister_ZeroSeed(t *testing.T) {
	zero := int64(0)
	mt := NewMersenneTwister(&zero)
	if mt == nil {
		t.Fatal("0 种子应回退非零值，不应返回 nil")
	}
	if zero == 0 {
		t.Fatal("零种子应被改为非零值")
	}
}

func TestMersenneTwister_Intn(t *testing.T) {
	mt := NewMersenneTwister(nil)
	for i := 0; i < 100; i++ {
		v := mt.Intn(10)
		if v < 0 || v >= 10 {
			t.Fatalf("Intn(10) 越界: %d", v)
		}
	}
}

func TestMersenneTwister_FloatRange(t *testing.T) {
	mt := NewMersenneTwister(nil)
	if v := mt.Float64Range(1.0, 5.0); v < 1.0 || v >= 5.0 {
		t.Fatalf("Float64Range 越界: %f", v)
	}
	if v := mt.Float32Range(1.0, 5.0); v < 1.0 || v >= 5.0 {
		t.Fatalf("Float32Range 越界: %f", v)
	}
}

func TestIsPrime(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{3, true},
		{4, false},
		{5, true},
		{9, false},
		{11, true},
		{17, true},
		{25, false},
		{97, true},
	}
	for _, c := range cases {
		if got := isPrime(c.n); got != c.want {
			t.Fatalf("isPrime(%d)=%v want=%v", c.n, got, c.want)
		}
	}
}

func TestGcd(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{12, 8, 4},
		{17, 5, 1},
		{100, 0, 100},
		{0, 0, 0}, // 退化情况
	}
	for _, c := range cases {
		if got := gcd(c.a, c.b); got != c.want {
			t.Fatalf("gcd(%d,%d)=%d want=%d", c.a, c.b, got, c.want)
		}
	}
}

func TestNextInt64(t *testing.T) {
	v := NextInt64()
	if v < 0 {
		t.Fatalf("NextInt64 应非负，实际: %d", v)
	}
}

func TestMersenneTwister_Determinism(t *testing.T) {
	seed := int64(123)
	mt1 := NewMersenneTwister(&seed)
	seed2 := int64(123)
	mt2 := NewMersenneTwister(&seed2)
	for i := 0; i < 10; i++ {
		if mt1.Int63() != mt2.Int63() {
			t.Fatalf("相同种子第 %d 次序列不一致", i)
		}
	}
}
