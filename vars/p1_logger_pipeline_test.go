package vars

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ---------- S27：统计字段原子化 ----------

// TestAsyncChannel_StatsConservation 回归：多协程投递后，入队总数必须恒等于
// 落盘数 + 丢弃数；统计字段此前为裸 int64，读写双向竞争会读到撕裂值。
func TestAsyncChannel_StatsConservation(t *testing.T) {
	w := &memChanWriter{}
	cfg := DefaultAsyncChannelConfig()
	cfg.BufferSize = 8
	cfg.DropOnFull = true
	cfg.FlushInterval = 5 * time.Millisecond
	a := NewAsyncLoggerChannel(w, cfg)

	const producerCount = 8
	const perProducer = 200

	stop := make(chan struct{})

	var readers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := a.GetStats()
				if s.TotalEnqueued < 0 || s.TotalWritten < 0 || s.TotalDropped < 0 {
					t.Errorf("统计出现负值/撕裂: %+v", s)
					return
				}
				time.Sleep(50 * time.Microsecond)
			}
		}()
	}

	var producers sync.WaitGroup
	for i := 0; i < producerCount; i++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for j := 0; j < perProducer; j++ {
				a.EnqueueSimple(slog.LevelInfo, "stats-conservation")
			}
		}()
	}

	producers.Wait()
	close(stop)
	readers.Wait()

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s := a.GetStats()
	if s.TotalEnqueued != int64(producerCount*perProducer) {
		t.Fatalf("入队计数丢失: got=%d want=%d", s.TotalEnqueued, producerCount*perProducer)
	}
	if s.TotalWritten+s.TotalDropped != s.TotalEnqueued {
		t.Fatalf("计数不守恒: enqueued=%d written=%d dropped=%d", s.TotalEnqueued, s.TotalWritten, s.TotalDropped)
	}
	if s.TotalWritten == 0 {
		t.Fatal("没有任何日志落盘")
	}
	if s.LastFlush.IsZero() {
		t.Fatal("上次刷新时间未被记录")
	}
}

// ---------- S28：channel 原子快照 ----------

// TestChannelLoggerManager_ConcurrentLogAndClose 回归：SetLevel/Close 换绑 channel 时，
// 业务协程的无锁读会拿到悬空指针或对已关闭通道投递。
func TestChannelLoggerManager_ConcurrentLogAndClose(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogLevel = LogLevelInfo
	cfg.Async = true
	cfg.AsyncBufferSize = 64
	cfg.Stdout = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	if err := m.initWithChannel(); err != nil {
		t.Fatalf("initWithChannel: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_ = runWithRecover(func() {
					m.LogAsyncSimple(slog.LevelInfo, "concurrent-close")
					_ = m.GetStats()
					_ = m.IsHealthy()
					_ = m.GetLoadFactor()
				})
			}
		}()
	}

	time.Sleep(2 * time.Millisecond)
	if err := runWithRecover(func() { _ = m.Close() }); err != nil {
		t.Fatalf("Close 并发 panic: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close 后业务协程卡死")
	}
}

// TestChannelLoggerManager_SetLevelKeepsAsyncChannel 回归：异步模式下 SetLevel 只重建了
// 同步 zap 路径，换绑后的 writer 没有生产者，日志会静默全丢。
func TestChannelLoggerManager_SetLevelKeepsAsyncChannel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogName = "setlevel"
	cfg.LogLevel = LogLevelInfo
	cfg.Async = true
	cfg.AsyncBufferSize = 64
	cfg.Stdout = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	if err := m.initWithChannel(); err != nil {
		t.Fatalf("initWithChannel: %v", err)
	}

	if err := m.SetLevel(LogLevelWarn); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}

	m.LogAsyncSimple(slog.LevelWarn, "must-survive-setlevel")
	if ch := m.channel.Load(); ch == nil {
		t.Fatal("SetLevel 后异步通道未重建，日志通道已断开")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path.Join(cfg.LogPath, "setlevel.log"))
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if !strings.Contains(string(data), "must-survive-setlevel") {
		t.Fatalf("SetLevel 后日志未落盘: %q", data)
	}
}

// ---------- S29：FlushAndWait 信号位被占用 ----------

// TestAsyncChannel_FlushAndWaitSignalFull 回归：flushSig 只有一个槽位，被占用时
// 必须按超时快速失败（并可由 stop 立即解套），而不是静默丢掉刷新信号。
func TestAsyncChannel_FlushAndWaitSignalFull(t *testing.T) {
	// 不带消费协程的裸通道：槽位一旦占住就不会被任何读者排空
	a := &AsyncLoggerChannel{
		input:    make(chan logEntry, 4),
		stop:     make(chan struct{}),
		flushSig: make(chan chan struct{}, 1),
	}
	a.flushSig <- make(chan struct{}, 1)

	start := time.Now()
	err := a.FlushAndWait(200 * time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("信号位被占用时应返回明确错误")
	}
	if elapsed > time.Second {
		t.Fatalf("未及时返回（疑似硬等）: %v", elapsed)
	}

	// 关闭信号必须让等待方立刻解套，而不是继续等满超时
	close(a.stop)
	start = time.Now()
	if err := a.FlushAndWait(5 * time.Second); err == nil {
		t.Fatal("通道停止后 FlushAndWait 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stop 未能解套 FlushAndWait: %v", elapsed)
	}
}

// ---------- S31：级别解析与全局属性 ----------

func TestZapLevelFor(t *testing.T) {
	cases := []struct {
		in      string
		want    zapcore.Level
		wantErr bool
	}{
		{in: "debug", want: zapcore.DebugLevel},
		{in: "INFO", want: zapcore.InfoLevel},
		{in: "", want: zapcore.InfoLevel},
		{in: "warn", want: zapcore.WarnLevel},
		{in: "warning", want: zapcore.WarnLevel},
		{in: "error", want: zapcore.ErrorLevel},
		// off 曾经是 FatalLevel：zap 收到 Fatal 会直接 os.Exit 干掉进程
		{in: "off", want: zapcore.InvalidLevel},
		{in: "verbose", wantErr: true},
		{in: "WRONG", wantErr: true},
	}

	for _, c := range cases {
		got, err := zapLevelFor(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("zapLevelFor(%q) 未知级别应报错，实际=%v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("zapLevelFor(%q) 意外报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("zapLevelFor(%q)=%v want=%v", c.in, got, c.want)
		}
	}
}

// TestOptimizedHandler_GlobalAttrsCoW 回归：WithGlobalAttrs 曾对共享切片就地 append，
// 与并发的 Handle 读取构成数据竞争。
func TestOptimizedHandler_GlobalAttrsCoW(t *testing.T) {
	h := NewOptimizedZapSlogHandler(zap.New(zapcore.NewNopCore()), slog.LevelInfo, 0)

	if got := len(h.globalAttrList()); got != 0 {
		t.Fatalf("初始全局属性应为空，实际=%d", got)
	}

	child := h.WithGroup("grp").(*OptimizedZapSlogHandler)

	h.WithGlobalAttrs(slog.String("service", "gate"))
	if got := len(child.globalAttrList()); got != 0 {
		t.Fatalf("子处理器快照被父级追加污染: %d", got)
	}
	if got := len(h.globalAttrList()); got != 1 {
		t.Fatalf("父级全局属性未生效: %d", got)
	}

	h.WithGlobalAttrs(slog.Int("worker", 7))
	if got := len(h.globalAttrList()); got != 2 {
		t.Fatalf("第二次追加丢失: %d", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 60; j++ {
				_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "concurrent-attrs", 0))
				h.WithGlobalAttrs(slog.Int("n", id))
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("并发 Handle + WithGlobalAttrs 卡死")
	}

	if got := len(h.globalAttrList()); got != 2+8*60 {
		t.Fatalf("并发追加计数丢失: %d", got)
	}
}

// ---------- S33：AsyncWriteSyncer 由 manager 持有并最后关闭 ----------

// TestCreateOptimizedZapCore_AsyncWriterOwned 回归：异步包装器此前只在函数内部可见，
// 调用方无法关闭它，缓冲区尾部日志随进程退出丢失。
func TestCreateOptimizedZapCore_AsyncWriterOwned(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogName = "owned"
	cfg.LogLevel = LogLevelDebug
	cfg.Stdout = false
	cfg.Async = true
	cfg.AsyncBufferSize = 64

	core, writer, asyncWriter, err := createOptimizedZapCore(cfg)
	if err != nil {
		t.Fatalf("createOptimizedZapCore: %v", err)
	}
	if asyncWriter == nil {
		t.Fatal("异步模式下必须回传 AsyncWriteSyncer 供调用方关闭")
	}

	core.Write(zapcore.Entry{Level: zapcore.InfoLevel, Message: "tail-log-must-flush", Time: time.Now()}, nil)

	// 关闭顺序：先 flush zap，再关异步包装把缓冲落到文件，最后关底层 writer
	_ = asyncWriter.Sync()
	if err := asyncWriter.Close(); err != nil {
		t.Fatalf("asyncWriter.Close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	data, err := os.ReadFile(path.Join(cfg.LogPath, "owned.log"))
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(data), "tail-log-must-flush") {
		t.Fatalf("尾部日志未随关闭落盘: %q", data)
	}
}

// TestChannelLoggerManager_SyncClosePersists 回归：同步模式下 zap 与底层 writer 的关闭顺序。
func TestChannelLoggerManager_SyncClosePersists(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.LogName = "syncclose"
	cfg.LogLevel = LogLevelDebug
	cfg.Stdout = false
	cfg.Async = false

	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}

	m.GetLogger().Info("sync-close-entry")
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path.Join(cfg.LogPath, "syncclose.log"))
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(data), "sync-close-entry") {
		t.Fatalf("Close 未收敛 zap 缓冲: %q", data)
	}
}

// ---------- S32：命令行输出缓冲 ----------

// TestConsoleBufferedOutput 回归：命令行输出改走缓冲后，内容必须在 Flush / CloseConsole
// 时可见，且关闭后仍能输出（退回直写），不能因缓冲而静默丢弃。
func TestConsoleBufferedOutput(t *testing.T) {
	resetConsole := func() {
		consoleMu.Lock()
		consoleWriter, consoleClosed, consoleTicker, consoleFlushStop = nil, false, nil, nil
		consoleMu.Unlock()
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	resetConsole()
	os.Stdout = w

	defer func() {
		os.Stdout = oldStdout
		resetConsole()
		_ = r.Close()
	}()

	printToConsole(slog.LevelInfo, "buffered-flush-line")
	if err := FlushConsole(); err != nil {
		t.Fatalf("FlushConsole: %v", err)
	}

	printToConsole(slog.LevelWarn, "buffered-close-line")
	if err := CloseConsole(); err != nil {
		t.Fatalf("CloseConsole: %v", err)
	}

	// 关闭控制台后仍有兜底直写
	printToConsole(slog.LevelError, "after-console-closed")

	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取管道失败: %v", err)
	}
	out := string(data)

	for _, want := range []string{"buffered-flush-line", "buffered-close-line", "after-console-closed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("命令行输出丢失 %q，实际=%q", want, out)
		}
	}
}
