package util

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"touchgocore/syncmap"
)

// TestConcurrentDoWithRetPerCallIsolation 并发触发同一 key 时，每次调用只能拿到自己的返回值。
// 旧的 Do+SetDoRet 机制把返回值写进接收者级共享切片，跨调用串味；
// 该通道已删除，返回值一律走调用栈。
func TestConcurrentDoWithRetPerCallIsolation(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("echo-tag", func(s string) string { return "ret:" + s })

	const n = 64
	var wg sync.WaitGroup
	mismatch := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := strconv.Itoa(i)
			rets, ok := c.DoWithRet("echo-tag", tag)
			if !ok || len(rets) != 1 {
				mismatch <- fmt.Sprintf("调用 %s 没有拿到返回值", tag)
				return
			}
			v, _ := rets[0].Interface().(string)
			if want := "ret:" + tag; v != want {
				mismatch <- fmt.Sprintf("返回值串味: got %q want %q", v, want)
			}
		}(i)
	}
	wg.Wait()
	close(mismatch)
	for msg := range mismatch {
		t.Error(msg)
	}
}

// TestDoAndDoWithRetInterleaveNoCrossTalk Do 不再影响带返回值路径的结果。
func TestDoAndDoWithRetInterleaveNoCrossTalk(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("shared", func(s string) string { return s })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if !c.Do("shared", strconv.Itoa(i)) {
				t.Errorf("Do 应返回 ok")
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			rets, ok := c.DoWithRet("shared", "v"+strconv.Itoa(i))
			if !ok || len(rets) != 1 {
				t.Errorf("DoWithRet 应返回 1 个值")
				return
			}
			if got, _ := rets[0].Interface().(string); got != "v"+strconv.Itoa(i) {
				t.Errorf("DoWithRet 结果被 Do 污染: %q", got)
			}
		}(i)
	}
	wg.Wait()
}

// TestDoOnlyReportsSuccess Do 只表达调用是否成功：回调 panic 时 ok=false，且不保留返回值。
func TestDoOnlyReportsSuccess(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("boom", func() string { panic("业务回调崩溃") })
	if ok := c.Do("boom"); ok {
		t.Fatal("回调 panic 时 Do 应返回 false")
	}

	c2 := &CallFunction{fn: syncmap.NewAny()}
	c2.Register("fine", func() string { return "x" })
	if ok := c2.Do("fine"); !ok {
		t.Fatal("回调正常时 Do 应返回 true")
	}
}

// TestDoWithRetKeepsGroupSemantics 同一 key 多个回调的返回值仍按注册顺序累积（既有契约，不得因删除全局通道而退化）。
func TestDoWithRetKeepsGroupSemantics(t *testing.T) {
	// 局部实例，避免污染 DefaultCallFunc 的注册表
	c := &CallFunction{fn: syncmap.NewAny()}
	c.Register("group", func(i int) int { return i + 1 })
	c.Register("group", func(i int) int { return i + 2 })
	rets, ok := c.DoWithRet("group", 10)
	if !ok || len(rets) != 2 {
		t.Fatalf("应累积 2 个返回值, got %d ok=%v", len(rets), ok)
	}
	if rets[0].Interface() != reflect.ValueOf(11).Interface() {
		t.Fatalf("第一个回调结果错误: %v", rets[0])
	}
	if rets[1].Interface() != reflect.ValueOf(12).Interface() {
		t.Fatalf("第二个回调结果错误: %v", rets[1])
	}
}
