package lua

import (
	"sync"
	rt "github.com/arnodel/golua/runtime"
)

// LuaScript 并发安全封装
type SafeLuaScript struct {
	*LuaScript
	mu sync.RWMutex
}

// NewSafeLuaScript 创建一个并发安全的 Lua 脚本实例
func NewSafeLuaScript(initScriptPath string) (*SafeLuaScript, error) {
	ls, err := NewLuaScript(initScriptPath)
	if err != nil {
		return nil, err
	}
	return &SafeLuaScript{LuaScript: ls}, nil
}

// Call 并发安全地调用 Lua 函数
func (s *SafeLuaScript) Call(funcName string, args ...interface{}) ([]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LuaScript.Call(funcName, args...)
}

// GetUID 获取脚本 ID
func (s *SafeLuaScript) GetUID() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LuaScript.UID
}

// GetRuntime 获取运行时（谨慎使用，可能导致并发问题）
func (s *SafeLuaScript) GetRuntime() *rt.Runtime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LuaScript.runtime
}

// GetReturnValues 获取最近的返回值
func (s *SafeLuaScript) GetReturnValues() []interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LuaScript.returnValues
}

// Close 安全关闭
func (s *SafeLuaScript) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LuaScript.Close()
}
