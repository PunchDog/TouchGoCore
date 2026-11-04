package lua

import (
	"errors"
	"fmt"
)

// ==================== 桥接层错误 ====================
//
// 约定：中文正文 + 括号内保留英文技术 token。
// 保留 token 不是凑字数——lifecycle 系列并发用例靠 "poisoned" / "not available" /
// "closed" / "reloading" 这些子串判定「这是预期的并发错误」，纯中文会迫使改动
// 那些并发断言（等于削弱既有闸）。

var (
	// errRuntimePoisoned 表示上一次调用超时、协程仍卡在 runtime 上，本实例不可再用。
	errRuntimePoisoned = errors.New("lua 运行时已中毒 (poisoned)：上一次调用超时，runtime 不可安全复用；请 Init() 或新建实例")
	// errRuntimeGone 表示 runtime 已被关闭或正在重载。
	errRuntimeGone = errors.New("lua 运行时不可用 (not available / reloading)")
	// errScriptClosed 表示脚本实例的上下文已取消。
	errScriptClosed = errors.New("lua 脚本已关闭 (closed)")
)

// errFunctionNotFound 表示全局环境里没有这个函数名。
func errFunctionNotFound(name string) error {
	return fmt.Errorf("函数未找到 (function not found): '%s'", name)
}

// errNotCallable 表示同名值不是可调用对象。
func errNotCallable(name string) error {
	return fmt.Errorf("'%s' 不是可调用函数", name)
}

// errMethodArity 表示实参个数与 Go 方法签名不匹配。
func errMethodArity(methodName string, want, got int) error {
	return fmt.Errorf("方法 '%s' 需要 %d 个参数（含接收者），实际 %d", methodName, want, got)
}
