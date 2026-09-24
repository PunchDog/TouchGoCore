// Package singleflight 是最小实现的同键请求合并（零外部依赖）。
// 语义与 golang.org/x/sync/singleflight 一致：同键并发只执行一次，其余等待共享结果。
package singleflight

import (
	"fmt"
	"sync"
)

type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Group 按 key 合并并发调用
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do 执行 fn；同 key 在途调用会被合并。shared 表示结果是否与他人共享。
// fn panic 会被转为 error，避免拖死全部等待者。
func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				c.err = fmt.Errorf("singleflight: panic recovered: %v", r)
			}
		}()
		c.val, c.err = fn()
	}()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err, false
}
