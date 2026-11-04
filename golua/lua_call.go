package lua

import (
	"context"
	"fmt"
	"time"

	"touchgocore/metrics"

	rt "github.com/arnodel/golua/runtime"
)

// Call 调用 Lua 函数
func (ls *LuaScript) Call(funcname string, list ...interface{}) ([]interface{}, error) {
	return ls.CallWithContext(context.Background(), funcname, list...)
}

// CallWithContext 使用上下文调用 Lua 函数。
//
// 并发约束：rtMu 读锁只保证本调用与热重载（写锁）互斥，不保证多个 Call 之间互斥。
// golua runtime 与 ls.thread 非线程安全，ls.returnValues 为共享字段；
// 同一实例的并发调用方必须自行串行化（使用 SafeLuaScript 或外层加锁）。
//
// 超时限制：超时只中止等待，后台协程不会被强杀——golua runtime 非线程安全，
// 无法从外部安全终止正在执行的 Lua 代码。超时后本实例被标记为 poisoned，
// 后续调用将直接返回错误；需要恢复请调用 Init() 或创建新实例。
func (ls *LuaScript) CallWithContext(ctx context.Context, funcname string, list ...interface{}) ([]interface{}, error) {
	// 快速路径：已中毒直接拒绝，无需竞争锁
	if ls.poisoned.Load() {
		return nil, errRuntimePoisoned
	}

	ls.rtMu.RLock()
	defer ls.rtMu.RUnlock()

	// 取锁后二次检查：可能在等锁期间被 poison 或 close
	if ls.poisoned.Load() {
		return nil, errRuntimePoisoned
	}
	if ls.runtime == nil || ls.env == nil {
		return nil, errRuntimeGone
	}

	select {
	case <-ls.ctx.Done():
		return nil, errScriptClosed
	default:
	}

	// 从全局环境获取函数
	funcVal := ls.env.Get(rt.StringValue(funcname))
	if funcVal == rt.NilValue {
		return nil, errFunctionNotFound(funcname)
	}

	// 检查是否是函数类型
	if _, ok := funcVal.TryCallable(); !ok {
		return nil, errNotCallable(funcname)
	}

	// 转换参数为 rt.Value
	args := make([]rt.Value, 0, len(list))
	for _, val := range list {
		args = append(args, GoToLuaValueWithContext(ctx, val))
	}

	// 捕获 thread 局部引用：即使超时后 ls.thread 被置 nil，协程仍持有有效引用
	thread := ls.thread

	// 调用 Lua 函数（带超时保护）
	resultChan := make(chan rt.Value, 1)
	errChan := make(chan error, 1)

	go func() {
		// 防护性 recover：超时后本协程成为泄漏协程，若内部 panic 会崩溃整个进程。
		// 由于 runtime 已被标记为 poisoned，此处 panic 不影响业务正确性。
		defer func() {
			if r := recover(); r != nil {
				errChan <- fmt.Errorf("lua goroutine panic (poisoned runtime): %v", r)
			}
		}()
		result, err := rt.Call1(thread, funcVal, args...)
		if err != nil {
			errChan <- err
		} else {
			resultChan <- result
		}
	}()

	metrics.Lua.IncCalls(funcname)
	started := time.Now()
	select {
	case <-ctx.Done():
		// 超时：标记 runtime 为中毒状态，后续调用不再使用该 runtime。
		// 注意：上面的 goroutine 仍在执行，无法安全终止（golua 非线程安全），
		// 但由于 poisoned 标记，不会有其他调用者并发访问同一 runtime。
		ls.poisoned.Store(true)
		metrics.Lua.IncCalls("timeout:" + funcname)
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		return nil, fmt.Errorf("call timeout: %w", ctx.Err())
	case result := <-resultChan:
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		ls.returnValues = []interface{}{LuaToGoValueWithContext(ctx, result)}
		return ls.returnValues, nil
	case err := <-errChan:
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		return nil, fmt.Errorf("Lua call failed: %w", err)
	}
}
