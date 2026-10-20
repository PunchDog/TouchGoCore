package zalo

// 本文件是入站消息面：zcago listener 的启停、三个消费协程（消息/错误/关闭），
// 以及 model.Message → InboundMessage 的纯映射。
//
// 并发口径（已读 zcago v0.3.0 listener/listener.go）：
//   - listener.Start(ctx, retryOnClose=true) 只在首次拨号时阻塞，重连在库内部完成；
//   - listener.Stop() 可重复调用，内部 cancel + 关连接 + 等自己的 run 协程；
//   - 消息/错误/关闭三个 channel 由 api.New 一次建好，listener 停止时不会关闭，
//     所以消费协程必须靠本文件的 lctx.Done() 退出，不能等 channel 关闭。
//
// 回调纪律：onMessage 在 zcago 的投递泵上同步执行，必须快；慢回调会阻塞整个
// listener（zcago 的 Message channel 满后泵就卡住）。广播/落库由 manager 侧异步化，
// 本层不做缓冲队列，避免在 seam 里再造一套背压。

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/amrakk/zcago/model"
	"touchgocore/vars"
)

// errZANilListener 表示登录后的 API 句柄没有 listener。
// api.New 正常登录后一定有，nil 说明上游行为变了或测试假件构造不全。
var errZANilListener = errors.New("zalo listener 不可用")

// StartListener 启动 zcago listener 并挂上三个消费协程。
//
// 幂等：重复调用返回 nil，不会起第二个 listener（zcago 自身也会拒绝二次 Start）。
// 失败路径把状态清回未启动，允许调用方重试。onMessage/onTerminal 传 nil 时替换为
// 空函数，避免消费协程 panic。
func (c *zaRealConn) StartListener(ctx context.Context, onMessage func(InboundMessage), onTerminal func(code int, reason string)) error {
	if c.api == nil {
		return errZANilAPI
	}
	ln := c.api.Listener()
	if ln == nil {
		return errZANilListener
	}
	if onMessage == nil {
		onMessage = func(InboundMessage) {}
	}
	if onTerminal == nil {
		onTerminal = func(int, string) {}
	}

	c.mu.Lock()
	if c.listenerStarted {
		c.mu.Unlock()
		return nil
	}
	c.listenerStarted = true
	lctx, cancel := context.WithCancel(ctx)
	c.listenerCancel = cancel
	c.mu.Unlock()

	if err := ln.Start(lctx, true); err != nil {
		cancel()
		c.mu.Lock()
		c.listenerStarted = false
		c.listenerCancel = nil
		c.mu.Unlock()
		return zaSafeWrap("zalo listener 启动失败", err)
	}

	// 与 StopListener 竞争时以「cancel 是否已被 Stop 摘走」为准：停过就不再起
	// 消费协程，避免在 Wait 之后 Add 触发 WaitGroup 误用。
	c.mu.Lock()
	if c.listenerCancel == nil {
		c.mu.Unlock()
		ln.Stop()
		return zaSafeWrap("zalo listener 启动被取消", context.Canceled)
	}
	c.listenerWG.Add(3)
	c.mu.Unlock()

	go func() {
		defer c.listenerWG.Done()
		pumpZAMessages(lctx, ln.Message(), onMessage)
	}()
	go func() {
		defer c.listenerWG.Done()
		pumpZAErrors(lctx, ln.Error())
	}()
	// Closed() 的元素类型来自 zcago 的 internal 包，无法在签名里写出，
	// 所以在闭包里消费；只认第一条关闭事件，交给 manager 判 3003/3000。
	go func() {
		defer c.listenerWG.Done()
		for {
			select {
			case <-lctx.Done():
				return
			case ci, ok := <-ln.Closed():
				if !ok {
					return
				}
				onTerminal(ci.Code, ci.Reason)
				return
			}
		}
	}()

	return nil
}

// StopListener 停止 listener 并等三个消费协程退出，可重复调用。
//
// 关键次序：先摘状态并释放 c.mu，再 cancel 与 ln.Stop()。ln.Stop() 会等待 zcago
// 的 run 协程，若持 c.mu 调用，任何正在 StartListener 的调用方都会被拖住，
// 而 Stop 本身也可能因回调重入 c.mu 而死锁。
func (c *zaRealConn) StopListener() {
	c.mu.Lock()
	cancel := c.listenerCancel
	started := c.listenerStarted
	c.listenerCancel = nil
	c.listenerStarted = false
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if !started {
		return
	}
	if c.api != nil {
		if ln := c.api.Listener(); ln != nil {
			ln.Stop()
		}
	}
	c.listenerWG.Wait()
}

// pumpZAMessages 消费入站消息：非文本/空文本由 messageToInbound 过滤。
func pumpZAMessages(ctx context.Context, ch <-chan model.Message, onMessage func(InboundMessage)) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if in, ok := messageToInbound(msg); ok {
				onMessage(in)
			}
		}
	}
}

// pumpZAErrors 消费 listener 错误。只打错误本身：zcago 的错误可能带 URL/参数，
// 但不会带 cookie/IMEI；本层不再额外拼接任何会话材料。
func pumpZAErrors(ctx context.Context, ch <-chan error) {
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-ch:
			if !ok {
				return
			}
			if err == nil {
				continue
			}
			// zcago 的 error 可能带上游响应体；只记类型，不把正文带进日志。
			vars.Warning("Zalo listener 错误类型=%T（详情已隐藏）", err)
		}
	}
}

// messageToInbound 把 zcago 的消息对象映射成本包 InboundMessage。
//
// 只接受 UserMessage/GroupMessage（含指针）：附件/表情/撤回等没有文本的消息一律
// 返回 false。文本取 Data.Content.String，nil 或 TrimSpace 后为空都跳过；
// AccountID 留空，由 manager 填（本层拿不到账号 id 的所有权）。
func messageToInbound(msg model.Message) (InboundMessage, bool) {
	if msg == nil {
		return InboundMessage{}, false
	}

	var data model.TMessage
	var threadType string
	switch m := msg.(type) {
	case model.UserMessage:
		data = m.Data
		threadType = "user"
	case *model.UserMessage:
		if m == nil {
			return InboundMessage{}, false
		}
		data = m.Data
		threadType = "user"
	case model.GroupMessage:
		data = m.Data.TMessage
		threadType = "group"
	case *model.GroupMessage:
		if m == nil {
			return InboundMessage{}, false
		}
		data = m.Data.TMessage
		threadType = "group"
	default:
		return InboundMessage{}, false
	}

	if data.Content.String == nil {
		return InboundMessage{}, false
	}
	text := *data.Content.String
	if strings.TrimSpace(text) == "" {
		return InboundMessage{}, false
	}

	// ts 是十进制毫秒字符串；解析失败按 0 处理（契约），不编造时间。
	ts, err := strconv.ParseInt(strings.TrimSpace(data.TS), 10, 64)
	if err != nil {
		ts = 0
	}

	return InboundMessage{
		ThreadID:    msg.ThreadID(),
		ThreadType:  threadType,
		IsSelf:      msg.IsSelf(),
		MsgID:       data.MsgID,
		FromID:      data.UIDFrom,
		ToID:        data.IDTo,
		SenderName:  data.DName,
		Text:        text,
		TimestampMS: ts,
	}, true
}
