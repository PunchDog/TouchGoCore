package lua

import (
	"context"
	rt "github.com/arnodel/golua/runtime"
	"sync"
)

// SafeLuaScript LuaScript 并发安全封装
type SafeLuaScript struct {
	*LuaScript
	mu sync.RWMutex
}

// NewSafeLuaScript 创建一个并发安全的 Lua 脚本实例（向后兼容）
func NewSafeLuaScript(initScriptPath string) (*SafeLuaScript, error) {
	return NewSafeLuaScriptWithContext(context.Background(), initScriptPath)
}

// NewSafeLuaScriptWithContext 使用上下文创建一个并发安全的 Lua 脚本实例
func NewSafeLuaScriptWithContext(ctx context.Context, initScriptPath string) (*SafeLuaScript, error) {
	ls, err := NewLuaScriptWithContext(ctx, initScriptPath)
	if err != nil {
		return nil, err
	}
	return &SafeLuaScript{LuaScript: ls}, nil
}

// Call 并发安全地调用 Lua 函数（向后兼容）
func (sls *SafeLuaScript) Call(funcName string, args ...interface{}) ([]interface{}, error) {
	return sls.CallWithContext(context.Background(), funcName, args...)
}

// CallWithContext 使用上下文并发安全地调用 Lua 函数
func (sls *SafeLuaScript) CallWithContext(ctx context.Context, funcName string, args ...interface{}) ([]interface{}, error) {
	sls.mu.Lock()
	defer sls.mu.Unlock()
	return sls.LuaScript.CallWithContext(ctx, funcName, args...)
}

// GetUID 获取脚本 ID
func (sls *SafeLuaScript) GetUID() int64 {
	sls.mu.RLock()
	defer sls.mu.RUnlock()
	return sls.LuaScript.UID
}

// GetRuntime 获取运行时（谨慎使用，可能导致并发问题）
func (sls *SafeLuaScript) GetRuntime() *rt.Runtime {
	sls.mu.RLock()
	defer sls.mu.RUnlock()
	return sls.LuaScript.runtime
}

// GetReturnValues 获取最近的返回值
func (sls *SafeLuaScript) GetReturnValues() []interface{} {
	sls.mu.RLock()
	defer sls.mu.RUnlock()
	// 返回副本避免外部修改
	if sls.LuaScript.returnValues == nil {
		return nil
	}
	result := make([]interface{}, len(sls.LuaScript.returnValues))
	copy(result, sls.LuaScript.returnValues)
	return result
}

// Close 安全关闭
func (sls *SafeLuaScript) Close() {
	sls.mu.Lock()
	defer sls.mu.Unlock()
	sls.LuaScript.Close()
}
