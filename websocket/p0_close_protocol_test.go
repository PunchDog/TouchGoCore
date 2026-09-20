package websocket

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"touchgocore/syncmap"
)

// ============================================================================
// S15：WebSocket 关闭协议回归。
//
// 修复前的三连缺陷（client.go Close）：
//  1. Close 里 close(c.msgChan)，而业务协程的 SendMsg 仍在写入 → send on closed channel；
//  2. Close 里立刻清空 wsConnect/remoteAddr/UID 并 clientpool.Put(c)，
//     此时 readLoop/handleLoop 还在使用同一实例 → 池别名（新连接复用同一对象）；
//  3. shutdownWebsocket 里 close(msgQueue); msgQueue = nil，读协程仍在投递 → 同样 panic。
//
// 修复后：只 close(c.closeCh) 通知退出；msgChan 永不 close；
// 清空字段与回池移入 recycle()，由最后退出的常驻协程（finishLoop）执行一次。
//
// 本机无 gcc，go test -race 不可用，因此用「发送方 recover 断言 + 池命中探测」替代。
// ============================================================================

// wsTestEnv 装配单元测试所需的全局依赖，测试结束自动还原。
func wsTestEnv(t *testing.T) {
	t.Helper()

	oldMap, oldPool, oldCall := clientMap, clientpool, clientcall
	oldMsgQueue, oldWorkerPool := msgQueue, workerPool.Swap(nil)
	oldServers := takeServers()

	clientMap = syncmap.NewMap[int64, *Client]()
	clientpool = &sync.Pool{New: func() any { return &Client{} }}
	clientcall = syncmap.NewMap[string, *sync.Pool]()
	clientcall.Store("wsTestCall", &sync.Pool{New: func() any { return &defaultCall{} }})
	msgQueue = make(chan *msgQueueType, 8)
	// 队列容量按「条」计，测试里压小避免无谓分配
	useQueueParams(t, wsQueueParams{writeEntries: 16, readEntries: 8, dropOnFull: true})

	t.Cleanup(func() {
		clientMap, clientpool, clientcall = oldMap, oldPool, oldCall
		msgQueue = oldMsgQueue
		workerPool.Store(oldWorkerPool)
		for _, srv := range oldServers {
			registerServer(srv)
		}
	})
}

// useQueueParams 临时替换队列/背压参数快照，测试结束自动还原。
func useQueueParams(t *testing.T, p wsQueueParams) {
	t.Helper()
	prev := wsQueue.Swap(&p)
	t.Cleanup(func() { wsQueue.Store(prev) })
}

// newBareClient 构造一个不依赖网络的客户端实例（常驻协程未启动）。
func newBareClient(t *testing.T) *Client {
	t.Helper()
	c := &Client{}
	c.iCallName = "wsTestCall"
	c.ICall = &defaultCall{}
	c.UID = atomicNextTestUID()
	c.remoteAddr = "bare://test"
	c.initChannels()
	clientMap.Store(c.UID, c)
	return c
}

var testUIDMu sync.Mutex
var testUID int64 = 1_000_000

func atomicNextTestUID() int64 {
	testUIDMu.Lock()
	defer testUIDMu.Unlock()
	testUID++
	return testUID
}

// pooledClient 探测实例是否已经回到对象池（同 P 下 Put 后 Get 必命中）。
func pooledClient(c *Client) bool {
	got := clientpool.Get()
	if got == nil {
		return false
	}
	return got.(*Client) == c
}

// TestClient_CloseDuringSendNoPanic 关闭与发送并发进行：不得出现 send on closed channel。
//
// 复刻真实形态：一条消费者协程抽取 msgChan（对应 handleLoop），多条业务协程投递，
// 期间执行 Close。修复前在约 100 轮内即可稳定复现 panic（见本次提交的说明）。
func TestClient_CloseDuringSendNoPanic(t *testing.T) {
	wsTestEnv(t)
	useQueueParams(t, wsQueueParams{writeEntries: 16, readEntries: 8})

	const rounds = 300
	const senders = 4

	for round := 0; round < rounds; round++ {
		c := newBareClient(t)
		queue := c.msgChan

		stop := make(chan struct{})
		var wg sync.WaitGroup
		panics := make(chan any, senders+2)

		// 消费者：对应 handleLoop 的抽取动作，保证发送方真的会阻塞在通道上
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			for {
				select {
				case <-stop:
					return
				case <-queue:
				}
			}
		}()

		for i := 0; i < senders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						select {
						case panics <- r:
						default:
						}
					}
				}()
				for {
					select {
					case <-stop:
						return
					default:
					}
					c.SendMsg([]byte("payload"))
				}
			}()
		}

		// 模拟一个仍在跑的常驻协程，Close 不应因此阻塞，也不应抢先回池
		c.liveLoops.Add(1)
		c.Close("测试并发关闭")
		c.finishLoop()

		close(stop)
		wg.Wait()
		close(panics)
		for r := range panics {
			t.Fatalf("✘ 第 %d 轮：Close 与 SendMsg 并发出现 panic（修复前为 send on closed channel）: %v", round, r)
		}
	}
	t.Log("✔ 多轮关闭/发送并发无 panic，msgChan 不再被 close")
}

// TestClient_NotRecycledUntilLoopsExit 常驻协程未退出前不得清空在用字段/回池。
func TestClient_NotRecycledUntilLoopsExit(t *testing.T) {
	wsTestEnv(t)
	c := newBareClient(t)

	// 模拟两个仍在运行的常驻协程
	c.liveLoops.Store(2)
	uid := c.UID

	c.Close("测试")

	if c.msgChan == nil || c.closeCh == nil {
		t.Fatal("✘ 协程仍在运行时通道已被清空（旧代码在 Close 内直接清）")
	}
	if pooledClient(c) {
		t.Fatal("✘ 协程仍在运行时实例已被归还对象池，新连接会复用同一对象造成别名")
	}
	if _, ok := clientMap.Load(uid); ok {
		t.Fatal("✘ Close 应先把客户端从 clientMap 摘除")
	}

	// 第一个协程退出：还不能回收
	c.finishLoop()
	if c.ICall == nil {
		t.Fatal("✘ 仍有协程在跑就释放了回调引用")
	}
	if pooledClient(c) {
		t.Fatal("✘ 仍有协程在跑就回池")
	}

	// 最后一个协程退出：此时才回收
	c.finishLoop()
	if !pooledClient(c) {
		t.Fatal("✘ 全部协程退出后未归还对象池（资源泄漏）")
	}
	if c.wsConnect != nil || c.ICall != nil || c.UID != 0 || c.remoteAddr != "" {
		t.Fatalf("✘ 回池后仍持有在用资源: wsConnect=%v ICall=%v uid=%d addr=%q",
			c.wsConnect != nil, c.ICall != nil, c.UID, c.remoteAddr)
	}
	t.Log("✔ 实例在最后一条协程退出后才清空字段并回池")
}

// TestClient_CloseRecycleStatsOnce 关闭+回收只做一次连接数 -1，重复 Close 无副作用。
func TestClient_CloseRecycleStatsOnce(t *testing.T) {
	wsTestEnv(t)
	c := newBareClient(t)

	before := GetServerStats().CurrentConnections
	c.counted.Store(true)
	UpdateConnectionStats(true) // 模拟 NewClient 成功路径的 +1

	c.liveLoops.Store(1)
	c.Close("第一次")
	c.Close("第二次") // 幂等：不得再次触发 OnClose/回池
	c.finishLoop()
	c.recycle() // 重复回收同样应为 no-op

	if got := GetServerStats().CurrentConnections; got != before {
		t.Fatalf("✘ 多次 Close/回收后连接统计漂移: before=%d now=%d", before, got)
	}
	t.Log("✔ Close 幂等，连接统计精确配对")
}

// TestClose_NoLoopsRecyclesImmediately 连接建立失败（协程从未启动）时也要回池。
func TestClose_NoLoopsRecyclesImmediately(t *testing.T) {
	wsTestEnv(t)
	c := newBareClient(t)

	c.Close("拨号失败")
	if !pooledClient(c) {
		t.Fatal("✘ 未启动协程的失败连接应立即回收，避免对象池白白消耗")
	}
}

// TestShutdownWebsocket_DoesNotCloseMsgQueue 停机不得关闭全局 msgQueue，
// 否则仍在投递的读协程会 send on closed channel。
func TestShutdownWebsocket_DoesNotCloseMsgQueue(t *testing.T) {
	wsTestEnv(t)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- &shutdownPanic{v: fmt.Sprint(r)}
			}
		}()
		shutdownWebsocket()
		// 停机后通道必须仍然可写（未满时立即成功）
		select {
		case msgQueue <- &msgQueueType{uid: 42, data: []byte("x")}:
			done <- nil
		case <-time.After(2 * time.Second):
			done <- errors.New("停机后向 msgQueue 投递超时")
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("✘ 停机后 msgQueue 不可用（修复前为 close 后发送 panic）: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("✘ shutdownWebsocket 超时未返回")
	}

	if msgQueue == nil {
		t.Fatal("✘ shutdownWebsocket 不应把 msgQueue 置 nil，Tick 仍会读取该变量")
	}
	t.Log("✔ shutdownWebsocket 保留 msgQueue，不再关闭")
}

type shutdownPanic struct{ v string }

func (e *shutdownPanic) Error() string { return "panic: " + e.v }
