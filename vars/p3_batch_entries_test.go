package vars

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// 配套用例（阶段12 复核整改 F4）：S65 里除「调用点定位」之外的三项改动。
//
// 这三项此前只有 bench 没有断言：bench 只证明「更快」，证明不了「输出逐字不变」
// 和「归还池时不带上一批」。补在这里而不是外部包，因为 batchEntries / appendEntry
// 都是私有实现。
// ============================================================================

func sampleEntry(msg string) logEntry {
	return logEntry{
		level: slog.LevelInfo,
		msg:   msg,
		time:  time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
		file:  "business/handler.go",
		line:  42,
	}
}

// TestBatchEntriesTracksBytesIncrementally 增量计量必须与逐条求和一致：
// 阈值判定改成一次整数比较后，这个等式是「刷盘时机不变」的唯一依据。
func TestBatchEntriesTracksBytesIncrementally(t *testing.T) {
	var b batchEntries
	msgs := []string{"", "short", "一条稍微长一点的消息用来验证按字节而非按条数计量"}
	want := 0
	for _, m := range msgs {
		b.add(sampleEntry(m))
		want += len(m) + entryHeaderBytes
		if b.bytes != want {
			t.Fatalf("✘ 第 %d 条后计量漂移: got=%d want=%d", len(b.entries), b.bytes, want)
		}
	}
	if len(b.entries) != len(msgs) {
		t.Fatalf("✘ 条数不符: got=%d want=%d", len(b.entries), len(msgs))
	}
}

// TestBatchEntriesResetClearsBackingArray reset 必须逐槽清引用后再截断。
//
// 只写 entries[:0] 的话底层数组继续攥着整批 msg/attrs/context；容器来自
// sync.Pool，这批字符串最长要拖到下一批重新填满才可能释放——日志低峰期等于
// 把峰值整批条目常驻在池里。归还前也必须清，否则下一个认领者会读到上一批。
func TestBatchEntriesResetClearsBackingArray(t *testing.T) {
	var b batchEntries
	for i := 0; i < 8; i++ {
		b.add(sampleEntry(strings.Repeat("x", 32)))
	}
	b.add(logEntry{
		level: slog.LevelError,
		msg:   "带字段",
		time:  time.Unix(0, 0),
		attrs: []slog.Attr{slog.String("k", "v")},
	})

	backing := b.entries
	b.reset()

	if len(b.entries) != 0 || b.bytes != 0 {
		t.Fatalf("✘ reset 不彻底: len=%d bytes=%d", len(b.entries), b.bytes)
	}
	for i, slot := range backing {
		if slot.msg != "" || slot.attrs != nil || slot.context != nil || slot.file != "" {
			t.Fatalf("✘ 底层数组第 %d 槽仍引用上一批条目: %+v", i, slot)
		}
	}
}

// TestAppendEntryMatchesHistoricalFormat 输出格式与旧的 formatEntry 逐字一致，
// 含 attrs 分支（生产入口 channel_logger_manager 会真的带 attrs 进来）。
func TestAppendEntryMatchesHistoricalFormat(t *testing.T) {
	a := &AsyncLoggerChannel{}

	entry := sampleEntry("登录成功")
	got := string(a.appendEntry(nil, entry))
	if want := "2026-09-21 10:00:00 INFO [business/handler.go:42 登录成功]\n"; got != want {
		t.Fatalf("✘ 无字段格式变了:\n got=%q\nwant=%q", got, want)
	}

	entry.level = slog.LevelError
	entry.attrs = []slog.Attr{slog.String("uid", "1001"), slog.Int("code", 7)}
	got = string(a.appendEntry(nil, entry))
	if want := "2026-09-21 10:00:00 ERROR [business/handler.go:42 登录成功] uid=1001 code=7\n"; got != want {
		t.Fatalf("✘ 带字段格式变了:\n got=%q\nwant=%q", got, want)
	}

	// file 为空（拿不到调用点）时不能输出空的 "[]" 之外的多余分隔符
	got = string(a.appendEntry(nil, logEntry{level: slog.LevelWarn, msg: "m", time: entry.time}))
	if want := "2026-09-21 10:00:00 WARN [m]\n"; got != want {
		t.Fatalf("✘ 无调用点格式变了:\n got=%q\nwant=%q", got, want)
	}
}

// TestVarsFuncPrefixResolves 前缀反推失败会返回空串，而 HasPrefix(x, "") 恒真 ——
// 届时每一帧都被当成本包内部帧跳过，调用点定位整体失效且不会报错。
func TestVarsFuncPrefixResolves(t *testing.T) {
	if varsFuncPrefix == "" {
		t.Fatal("✘ varsFuncPrefix 反推失败，帧归属判定会退化为「全是内部帧」")
	}
	if !strings.HasSuffix(varsFuncPrefix, ".") || !strings.Contains(varsFuncPrefix, "vars") {
		t.Fatalf("✘ varsFuncPrefix 形态不对: %q", varsFuncPrefix)
	}
	if file, line := getCaller(); file == "" || line == 0 {
		t.Fatalf("✘ 前缀生效却定位不到调用点: file=%q line=%d", file, line)
	}
}
