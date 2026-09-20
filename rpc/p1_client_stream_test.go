package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ============================================================================
// S16：RpcClient 流与连接生命周期回归。
//
// 修复前：
//  1. SendMsg 用「本次调用的 timeout ctx」建立并缓存长流，函数返回时 defer cancel()
//     立刻作废整条流 → 第二次调用复用到的是一条已取消的流，收发全废；
//  2. Tick() 重连直接 c.conn.Store(new)，旧 *grpc.ClientConn 既不 Swap 也不 Close → 泄漏；
//  3. NewRpcClient 首次连接失败分支执行 atomic.Value.Store(nil) → panic；
//  4. recvLoop / 发送失败各自零散地 streamValid.Store(false)，既不释放 context，
//     旧 recvLoop 还可能误杀重连后的新流。
//
// 修复后：流使用独立的可取消 context（streamCancel），重建/断线统一走
// invalidateStream → closeStreamLocked；换连接用 atomic.Pointer.Swap 后关旧连接。
// ============================================================================

// fakeMsgStream 只实现 Send/Recv 的假流，用于确定性地驱动 recvLoop 分支。
type fakeMsgStream struct {
	message.Grpc_MsgClient
	recvCh chan *message.FSMessage
	errCh  chan error
}

func (f *fakeMsgStream) Send(*message.FSMessage) error { return nil }

func (f *fakeMsgStream) Recv() (*message.FSMessage, error) {
	select {
	case m := <-f.recvCh:
		return m, nil
	case err := <-f.errCh:
		return nil, err
	}
}

// rpcTestEnv 装配一个最小可用的 RPC 测试环境。
func rpcTestEnv(t *testing.T) {
	t.Helper()

	prevCfg := config.Cfg_
	prevRunCtx := runCtx()
	cfg := &config.Cfg{Rpc: &config.RpcConfig{Auth: &config.RpcAuthConfig{Mode: "none"}}}
	config.Cfg_ = cfg
	setRunCtx(corectx.WithCfg(context.Background(), cfg))
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prevCfg
		setRunCtx(prevRunCtx)
	})
}

func newFakeClient() *RpcClient {
	c := &RpcClient{
		serverName: "fake",
		fullAddr:   "fake://127.0.0.1:0",
		timeout:    time.Second,
	}
	c.SetCallbacks(NewClientCallbacks())
	return c
}

// startEchoServer 启动一个真实 RPC 服务端并注册回包 handler。
func startEchoServer(t *testing.T, name string, port int, proto1, proto2 int32, resp string) {
	t.Helper()

	if err := StartGrpcServer(name, port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer(name)
	if srv == nil {
		t.Fatal("服务端未注册")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })

	util.RegisterProtocolType(proto1, proto2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, proto1, proto2)
	id := util.DefaultCallFunc.Register(key, func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String(resp)
	})
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, id) })
}

// freeAddr 抢占一个空闲 TCP 端口并立即释放，返回端口号。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// closeTestClient 释放测试客户端持有的流与连接。
func closeTestClient(c *RpcClient) {
	_ = c.Close()
}

// TestRpcClient_StreamSurvivesCallTimeout 一次调用结束（含超时）后，
// 缓存流必须仍然可用：连续两次调用都要拿到响应。
//
// 修复前第二次调用复用到了被首次 cancel() 作废的流，收不到响应 → 用例超时。
func TestRpcClient_StreamSurvivesCallTimeout(t *testing.T) {
	rpcTestEnv(t)

	port := freePort(t)
	startEchoServer(t, "s16-echo", port, 71, 72, "pong")

	client := NewRpcClient("s16-client", "127.0.0.1", port)
	if client == nil {
		t.Fatal("客户端未创建")
	}
	client.timeout = 3 * time.Second
	t.Cleanup(func() { closeTestClient(client) })

	got := make(chan string, 4)
	client.SetCallbacks(&ClientCallbacks{
		OnMessageReceived: func(_ string, _, _ int32, msg proto.Message) {
			if sv, ok := msg.(*wrapperspb.StringValue); ok {
				select {
				case got <- sv.GetValue():
				default:
				}
			}
		},
	})

	for i := 0; i < 2; i++ {
		client.SendMsg(71, 72, wrapperspb.String("ping"), nil)
		select {
		case v := <-got:
			if v != "pong" {
				t.Fatalf("✘ 第 %d 次调用响应异常: %q", i+1, v)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("✘ 第 %d 次调用没拿到响应（修复前：缓存流被上一次调用的 ctx cancel 连带作废）", i+1)
		}
	}

	if !client.streamValid.Load() {
		t.Fatal("✘ 两次调用后流应仍然有效")
	}
	first := client.currentStream()
	if first == nil {
		t.Fatal("✘ 未缓存流")
	}
	second, err := client.ensureStream(client.conn.Load())
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("✘ 流未被复用，说明每次调用都在重建长流")
	}
	t.Log("✔ 调用级超时不再牵连缓存流，两次调用复用同一条流")
}

// TestRpcClient_ReconnectClosesOldConn 重连必须关闭旧连接、作废旧流。
func TestRpcClient_ReconnectClosesOldConn(t *testing.T) {
	rpcTestEnv(t)

	// grpc.NewClient 是惰性建连，地址格式合法即可，无需真实监听
	const addr = "127.0.0.1:65001"
	c := newFakeClient()
	c.fullAddr = addr

	oldConn, err := newClient(addr, false)
	if err != nil {
		t.Fatal(err)
	}
	c.conn.Store(oldConn)
	c.connStatus.Store(true)

	oldStream := &fakeMsgStream{recvCh: make(chan *message.FSMessage, 1), errCh: make(chan error, 1)}
	c.stream.Store(message.Grpc_MsgClient(oldStream))
	c.streamCancel = func() {}
	c.streamValid.Store(true)

	c.Tick()

	if c.conn.Load() == oldConn {
		t.Fatal("✘ Tick 未更换连接")
	}
	// connectivity.Shutdown 是「连接已 Close」的确定信号
	if got := oldConn.GetState(); got != connectivity.Shutdown {
		t.Fatalf("✘ 旧连接未被 Tick 关闭（重连会累积泄漏），当前状态: %v", got)
	}
	if c.streamValid.Load() {
		t.Fatal("✘ 换连接后旧流必须作废")
	}
	t.Log("✔ 重连替换连接时关闭旧连接并作废旧流")
}

// TestRpcClient_NilConnNoPanic 未连接状态不得对原子容器写入 nil 值。
func TestRpcClient_NilConnNoPanic(t *testing.T) {
	rpcTestEnv(t)

	// 对照组：修复前 conn 是 atomic.Value，Store(nil) 必然 panic
	var legacy atomic.Value
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("✘ 预期 atomic.Value.Store(nil) panic（说明改用 atomic.Pointer 的必要性）")
			}
		}()
		legacy.Store(nil)
	}()

	c := newFakeClient()
	if c.conn.Load() != nil {
		t.Fatal("✘ 零值应为未连接")
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("✘ 未连接路径写入 nil 触发 panic: %v", r)
			}
		}()
		c.conn.Store(nil) // 断开时清空连接，新协议允许
	}()
	if c.conn.Load() != nil {
		t.Fatal("✘ 清空后应仍是未连接")
	}
}

// TestRpcClient_OldRecvLoopCannotKillNewStream 迟到的旧 recvLoop 退出
// 不得作废重连后的新流。
func TestRpcClient_OldRecvLoopCannotKillNewStream(t *testing.T) {
	rpcTestEnv(t)
	c := newFakeClient()

	oldStream := &fakeMsgStream{recvCh: make(chan *message.FSMessage, 1), errCh: make(chan error, 1)}
	newStream := &fakeMsgStream{recvCh: make(chan *message.FSMessage, 1), errCh: make(chan error, 1)}

	c.stream.Store(message.Grpc_MsgClient(oldStream))
	c.streamCancel = func() {}
	c.streamValid.Store(true)

	go c.recvLoop(oldStream)
	oldStream.errCh <- errors.New("模拟旧流断开")
	time.Sleep(100 * time.Millisecond)

	if c.streamValid.Load() {
		t.Fatal("✘ 前置条件被破坏：旧流报错后应作废")
	}

	// 换成新流后，旧 recvLoop 的二次报错不得牵连当前流
	c.stream.Store(message.Grpc_MsgClient(newStream))
	c.streamCancel = func() {}
	c.streamValid.Store(true)

	if c.invalidateStream(oldStream) {
		t.Fatal("✘ 非当前流不应被判定为作废")
	}
	if !c.streamValid.Load() || c.currentStream() != newStream {
		t.Fatal("✘ 旧 recvLoop 误杀了当前流")
	}

	// 当前流报错则必须作废
	if !c.invalidateStream(newStream) {
		t.Fatal("✘ 当前流应被作废")
	}
	if c.streamValid.Load() {
		t.Fatal("✘ 作废后 streamValid 应为 false")
	}
	t.Log("✔ recvLoop 只会作废自己那条流，不会牵连新流")
}
