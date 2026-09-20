package util

import (
	"sync"
	"sync/atomic"
	"testing"
	"touchgocore/syncmap"
)

func newTestCallFunc() *CallFunction {
	return &CallFunction{fn: syncmap.NewAny()}
}

func TestCallback_UnregisterByIDRemovesExactlyOne(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	hits := map[string]int{}

	id1 := c.Register("k", func() { mu.Lock(); hits["a"]++; mu.Unlock() })
	c.Register("k", func() { mu.Lock(); hits["b"]++; mu.Unlock() })

	if !c.Unregister("k", id1) {
		t.Fatal("按 ID 注销应成功")
	}
	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if hits["a"] != 0 || hits["b"] != 1 {
		t.Fatalf("注销后应只剩一个回调: %v", hits)
	}
}

func TestCallback_UnregisterByFuncValue(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	hit := 0
	fn := func() { mu.Lock(); hit++; mu.Unlock() }

	c.Register("k", fn)
	if !c.Unregister("k", fn) {
		t.Fatal("按函数值注销应成功（旧实现 DeepEqual 对 func 恒 false）")
	}
	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if hit != 0 {
		t.Fatal("注销后回调不应再执行")
	}
}

func TestCallback_UnregisterConcurrentWithDo(t *testing.T) {
	c := newTestCallFunc()
	var wg sync.WaitGroup
	var counter atomic.Int64

	id := c.Register("k", func() { counter.Add(1) })
	ids := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		ids = append(ids, c.Register("k", func() { counter.Add(1) }))
	}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); for j := 0; j < 200; j++ { c.Do("k") } }()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Unregister("k", id)
		for _, one := range ids {
			c.Unregister("k", one)
		}
	}()
	wg.Wait()

	if _, has := c.fn.Load("k"); has {
		t.Fatal("全部注销后 key 应被删除")
	}
}

// panic 隔离：一个回调 panic 不应阻止其余回调执行
func TestCallback_PanicIsolation(t *testing.T) {
	c := newTestCallFunc()
	var mu sync.Mutex
	after := 0
	c.Register("k", func() { panic("boom") })
	c.Register("k", func() { mu.Lock(); after++; mu.Unlock() })

	c.Do("k")
	mu.Lock()
	defer mu.Unlock()
	if after != 1 {
		t.Fatal("前一回调 panic 后，后续回调仍应执行")
	}
}
