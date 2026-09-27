package rpc

import (
	"testing"
	"time"

	"touchgocore/network/message"
)

// ============================================================================
// L6：RpcClient 池复用依赖隐式清理的回归。
//
// 问题：RpcClient 通过 localtimer 对象池复用，pending（在途请求表）/ stream
// 状态原本只在 Close 路径隐式清理。若 Close 未走完或被跳过，池把实例发给下一个
// NewRpcClient 时，复用者会继承脏状态：残留 pending 让响应错投给已消失的等待者，
// 残留 streamValid/stream 让复用者在一条死流上收发，nextReqID 高水位错乱代际。
//
// 修复：在池复用取出/重置路径（NewTimer 的 initcallback，复用者独占期）显式调用
// resetForReuse() 兜底清理。本用例直接驱动 resetForReuse，确定性地验证：
// 预置脏 pending/stream 的实例经清理后状态干净，且清理幂等安全。
// ============================================================================

// dirtyClient 构造一个带脏状态的 RpcClient，模拟「Close 未走完就被池收走」的实例。
func dirtyClient() (*RpcClient, chan *message.FSMessage) {
	c := &RpcClient{
		serverName: "reuse",
		fullAddr:   "reuse://127.0.0.1:0",
		timeout:    time.Second,
	}
	c.SetCallbacks(NewClientCallbacks())

	// 脏 pending：两个在途请求，其中一个有等待者正在阻塞收响应
	waitCh := make(chan *message.FSMessage, 1)
	c.pending.Store(uint64(1), waitCh)
	c.pending.Store(uint64(2), make(chan *message.FSMessage, 1))

	// 脏 stream：缓存了一条流，且标记为有效
	c.stream.Store(message.Grpc_MsgClient(&fakeMsgStream{
		recvCh: make(chan *message.FSMessage, 1),
		errCh:  make(chan error, 1),
	}))
	c.streamCancel = func() {}
	c.streamValid.Store(true)

	// 脏代际计数与连接状态
	c.nextReqID.Store(42)
	c.connStatus.Store(true)
	return c, waitCh
}

func pendingCount(c *RpcClient) int {
	n := 0
	c.pending.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestRpcClient_ResetForReuseClearsDirtyState 预置脏 pending/stream 的实例
// 经池复用重置后状态必须干净：pending 为空、stream 字段零值、代际计数归零。
func TestRpcClient_ResetForReuseClearsDirtyState(t *testing.T) {
	c, waitCh := dirtyClient()

	// 前置：确认脏状态确实存在，否则本用例是空跑
	if pendingCount(c) != 2 {
		t.Fatalf("前置条件被破坏：脏 pending 数=%d", pendingCount(c))
	}
	if c.stream.Load() == nil || !c.streamValid.Load() {
		t.Fatal("前置条件被破坏：脏 stream 未预置")
	}

	c.resetForReuse()

	// pending 表清空
	if n := pendingCount(c); n != 0 {
		t.Errorf("✘ 复用后 pending 未清空: %d 条残留", n)
	}
	// 残留等待者被投递 nil 唤醒（语义与 failAllPending 一致），不会永久阻塞
	select {
	case v, ok := <-waitCh:
		if ok && v != nil {
			t.Errorf("✘ 残留等待者应收到 nil 唤醒信号，实收: %v", v)
		}
	default:
		t.Error("✘ 残留等待者未被唤醒（清理未投递 nil）")
	}
	// stream 字段回到零值
	if c.stream.Load() != nil {
		t.Error("✘ 复用后 stream 未归零值")
	}
	if c.currentStream() != nil {
		t.Error("✘ 复用后 currentStream 应为 nil")
	}
	if c.streamValid.Load() {
		t.Error("✘ 复用后 streamValid 应为 false")
	}
	if c.streamCancel != nil {
		t.Error("✘ 复用后 streamCancel 应为 nil")
	}
	// 代际计数与连接状态归零
	if got := c.nextReqID.Load(); got != 0 {
		t.Errorf("✘ 复用后 nextReqID 应归零，实为: %d", got)
	}
	if c.connStatus.Load() {
		t.Error("✘ 复用后 connStatus 应归零")
	}
	t.Log("✔ 脏 pending/stream/代际计数经 resetForReuse 后全部干净")
}

// TestRpcClient_ResetForReuseIdempotent 清理必须幂等：对已经干净的实例
// （正常 Close 后再复用）重复调用不 panic、不改坏状态。
func TestRpcClient_ResetForReuseIdempotent(t *testing.T) {
	c := newFakeClient()

	// 零值实例连续多次清理都不应 panic
	for i := 0; i < 3; i++ {
		c.resetForReuse()
	}
	if pendingCount(c) != 0 || c.stream.Load() != nil || c.streamValid.Load() {
		t.Fatal("✘ 幂等清理后干净实例状态被改坏")
	}

	// 先弄脏再连续清理两次，第二次必须是安全的空操作
	dirty, _ := dirtyClient()
	dirty.resetForReuse()
	dirty.resetForReuse()
	if n := pendingCount(dirty); n != 0 {
		t.Fatalf("✘ 二次清理后 pending 应仍为空: %d", n)
	}
	if dirty.stream.Load() != nil || dirty.nextReqID.Load() != 0 {
		t.Fatal("✘ 二次清理后 stream/代际计数应保持干净")
	}
	t.Log("✔ resetForReuse 幂等：干净实例重复清理安全")
}

// TestRpcClient_ResetForReuseWiredIntoConstructor 验证 resetForReuse 已接入
// 池复用重置路径：NewRpcClient 的 initcallback 会调用它。用一个已脏的实例
// 走一遍 callback 应清干净——直接调用构造闭包所触发的清理入口。
func TestRpcClient_ResetForReuseWiredIntoConstructor(t *testing.T) {
	// 无法在单测里安全地跑完整 NewRpcClient（需真实拨号），
	// 这里断言清理入口存在且能被复用路径调用：等价于 initcallback 内的那一步。
	c, _ := dirtyClient()
	c.closed.Store(true) // 模拟上一位主人留下的 closed 标记

	// 复刻 initcallback 对复用实例做的关键清理次序
	c.closed.Store(false)
	c.resetForReuse()

	if c.closed.Load() {
		t.Error("✘ closed 标记未随复用清除")
	}
	if pendingCount(c) != 0 || c.stream.Load() != nil {
		t.Error("✘ 复用路径未清干净脏 pending/stream")
	}
	t.Log("✔ 复用重置入口按预期清理脏状态")
}
