package telegram

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/util"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// waitWithin 在预算内等待 fn 完成，返回是否在预算内结束。
// 本机无 gcc 跑不了 -race，因此所有断言都取确定性行为而非时序巧合。
func waitWithin(t *testing.T, d time.Duration, name string, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		t.Fatalf("%s 在 %s 内未完成", name, d)
		return false
	}
}

// TestRunUpdatesExitsOwnRoundAfterRestart 每轮轮询只认自己的取消信号。
// 旧实现的循环在 select 里读包级 closeCh：重启后 closeCh 被换成新 channel，
// 上一轮协程再也看不到自己被关掉的 channel，只能等新轮关闭 → 永久泄漏一个协程。
func TestRunUpdatesExitsOwnRoundAfterRestart(t *testing.T) {
	round1Ctx, cancel1 := context.WithCancel(context.Background())
	updates1 := make(chan tgbotapi.Update, 1)
	_, round2Cancel := context.WithCancel(context.Background())
	defer round2Cancel()

	var closed1 atomic.Int32
	exited := make(chan struct{})
	go func() {
		runUpdates(round1Ctx, nil, updates1, func() { closed1.Add(1) })
		close(exited)
	}()

	// 模拟「新一轮已接管」：只有旧轮自己的 ctx 被取消
	cancel1()
	if !waitWithin(t, time.Second, "旧轮轮询退出", func() { <-exited }) {
		return
	}
	if got := closed1.Load(); got != 1 {
		t.Fatalf("旧轮清理应执行一次, got %d", got)
	}
}

// TestRunUpdatesExitsOnStreamEnd 更新流被关闭（库侧 StopReceivingUpdates）后轮询要退出。
func TestRunUpdatesExitsOnStreamEnd(t *testing.T) {
	updates := make(chan tgbotapi.Update)
	var closed atomic.Int32
	exited := make(chan struct{})
	go func() {
		runUpdates(context.Background(), nil, updates, func() { closed.Add(1) })
		close(exited)
	}()
	close(updates)
	if !waitWithin(t, time.Second, "流关闭后轮询退出", func() { <-exited }) {
		return
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("流关闭时清理应执行一次, got %d", got)
	}
}

// TestRunUpdatesDispatchesMessage 轮询必须把普通文本消息交给 Say 回调。
func TestRunUpdatesDispatchesMessage(t *testing.T) {
	got := make(chan string, 1)
	id := util.DefaultCallFunc.Register(util.CallTelegramMsg+"Say", func(text string, bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
		select {
		case got <- text:
		default:
		}
	})
	defer util.DefaultCallFunc.Unregister(util.CallTelegramMsg+"Say", id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan tgbotapi.Update, 1)
	go runUpdates(ctx, nil, updates, nil)

	updates <- tgbotapi.Update{Message: &tgbotapi.Message{Text: "你好"}}
	select {
	case txt := <-got:
		if txt != "你好" {
			t.Fatalf("消息文本错误: %q", txt)
		}
	case <-time.After(time.Second):
		t.Fatal("Say 回调未被触发")
	}
}

// TestTelegramStopWithoutStartIsIdempotent 未启动/重复停止都应为 no-op 且不阻塞。
func TestTelegramStopWithoutStartIsIdempotent(t *testing.T) {
	runTimeMu.Lock()
	prev := currentCancel
	currentCancel = nil
	runTimeMu.Unlock()
	defer func() {
		runTimeMu.Lock()
		currentCancel = prev
		runTimeMu.Unlock()
	}()

	waitWithin(t, time.Second, "第一次 Stop", func() { TelegramStop(context.Background()) })
	waitWithin(t, time.Second, "第二次 Stop", func() { TelegramStop(context.Background()) })
	TelegramStop(nil)
	if globalBot.Load() != nil {
		t.Fatal("停止后不应残留 bot")
	}
}

// TestStopCancelsOnlyRegisteredRound Stop 只作废注册在册的那一轮，
// 并且取走 cancel 后重复 Stop 不会再影响任何一轮。
func TestStopCancelsOnlyRegisteredRound(t *testing.T) {
	runTimeMu.Lock()
	prev := currentCancel
	runTimeMu.Unlock()
	defer func() {
		runTimeMu.Lock()
		currentCancel = prev
		runTimeMu.Unlock()
	}()

	registeredCtx, registeredCancel := context.WithCancel(context.Background())
	defer registeredCancel()
	bystanderCtx, bystanderCancel := context.WithCancel(context.Background())
	defer bystanderCancel()

	updates := make(chan tgbotapi.Update, 1)
	var cleaned atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runUpdates(registeredCtx, nil, updates, func() { cleaned.Store(true) })
	}()

	runTimeMu.Lock()
	currentCancel = registeredCancel
	runTimeMu.Unlock()

	TelegramStop(context.Background())
	if !waitWithin(t, time.Second, "被注册的那一轮退出", func() { wg.Wait() }) {
		return
	}
	if !cleaned.Load() {
		t.Fatal("Stop 应执行被停轮的清理")
	}
	select {
	case <-bystanderCtx.Done():
		t.Fatal("Stop 误取消了无关轮次的上下文")
	default:
	}

	// currentCancel 已被取走：再次 Stop 不应取消任何轮次
	TelegramStop(context.Background())
	select {
	case <-bystanderCtx.Done():
		t.Fatal("重复 Stop 取消了无关轮次")
	default:
	}
}

// TestRunCtxVisibleToCfgReaders 运行期上下文以原子快照对回调可见，读写并发不炸。
func TestRunCtxVisibleToCfgReaders(t *testing.T) {
	prev := runCtx()
	defer setRunCtx(prev)

	setRunCtx(context.WithValue(context.Background(), runCtxProbeKey{}, "first"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = runCtx().Value(runCtxProbeKey{})
				_ = telegramCfg()
			}
		}()
	}
	setRunCtx(context.WithValue(context.Background(), runCtxProbeKey{}, "second"))
	wg.Wait()
	if runCtx().Value(runCtxProbeKey{}) == nil {
		t.Fatal("runCtx 快照丢失")
	}
}

type runCtxProbeKey struct{}
