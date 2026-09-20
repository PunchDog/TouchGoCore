package vars

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// ============================================================================
// 基准（S65）：日志热路径的两处开销——调用栈定位与批处理计量/合并写入。
//
// 本文件刻意只用稳定字段构造 batch，改前后都能编译，便于直接对比 ns/op。
// ============================================================================

func callerDepth4() (string, int) { return callerDepth3() }
func callerDepth3() (string, int) { return callerDepth2() }
func callerDepth2() (string, int) { return callerDepth1() }

// callerDepth1 位于 vars 包内，getCaller 必须跳过它才能落到业务帧
func callerDepth1() (string, int) { return getCaller() }

func BenchmarkGetCallerDepth4(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = callerDepth4()
	}
}

func newBenchEntries(n int) []logEntry {
	es := make([]logEntry, n)
	for i := range es {
		es[i] = logEntry{
			level: slog.LevelInfo,
			msg:   "benchmark log line with some payload",
			time:  time.Now(),
			file:  "D:\\TouchGoCore\\vars\\async_channel.go",
			line:  i + 1,
		}
	}
	return es
}

func BenchmarkWriteBatch100(b *testing.B) {
	a := NewAsyncLoggerChannel(io.Discard, DefaultAsyncChannelConfig())
	defer a.Close()

	batch := &batchEntries{}
	for _, e := range newBenchEntries(100) {
		batch.add(e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.writeBatch(batch)
	}
}

// BenchmarkBatchThresholdCheck 测量「每收到一条就判一次是否达到刷盘阈值」的成本：
// 改前每次判定都重算整批字节数（一批 100 条 ⇒ 累计 O(n²) 次遍历，100 条即近 5000 圈），
// 改后 bytes 随 append 增量维护，判定退化为一次整数比较。
func BenchmarkBatchThresholdCheck(b *testing.B) {
	cfg := DefaultAsyncChannelConfig()
	cfg.DropOnFull = true
	a := NewAsyncLoggerChannel(io.Discard, cfg)
	defer a.Close()

	es := newBenchEntries(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := &batchEntries{}
		for j := 0; j < 100; j++ {
			batch.add(es[j])
			if batch.bytes >= cfg.FlushThreshold {
				batch.reset()
			}
		}
	}
}
