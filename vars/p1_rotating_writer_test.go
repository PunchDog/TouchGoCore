package vars

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testClock 可注入的原子时钟，供轮转与过期清理的确定性测试使用
type testClock struct {
	ns atomic.Int64
}

func (c *testClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *testClock) Set(t time.Time)         { c.ns.Store(t.UnixNano()) }
func (c *testClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// newClockWriter 构造不启动后台清理协程（maxAge=0）的写入器并挂上假时钟，
// 需要测试定期清理时用返回的写入器自行置 maxAge 并调用 startAgeCleanup。
func newClockWriter(t *testing.T, maxSize int64, maxBackups int, compress bool) (*RotatingFileWriter, *testClock) {
	t.Helper()

	w, err := NewRotatingFileWriter(t.TempDir(), "probe", maxSize, 0, maxBackups, compress)
	if err != nil {
		t.Fatalf("创建轮转写入器失败: %v", err)
	}

	clock := &testClock{}
	clock.Set(time.Now())
	w.nowFunc = clock.Now

	t.Cleanup(func() { _ = w.Close() })
	return w, clock
}

func listFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("匹配文件失败: %v", err)
	}
	return matches
}

func readFile(t *testing.T, p string) string {
	t.Helper()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", p, err)
	}
	return string(data)
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", p, err)
	}
	return info.Size()
}

// 轮转触发后备份文件存在，活动文件只保留轮转后的新内容
func TestRotatingWriter_RotateBySize(t *testing.T) {
	w, clock := newClockWriter(t, 8, 0, false)

	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	clock.Advance(2 * time.Second)
	if _, err := w.Write([]byte("tail")); err != nil {
		t.Fatalf("轮转后写入失败: %v", err)
	}

	backups := listFiles(t, w.filePath, "probe_*.log")
	if len(backups) != 1 {
		t.Fatalf("应生成 1 个备份文件，实际=%d (%v)", len(backups), backups)
	}
	if got := strings.TrimSpace(readFile(t, backups[0])); got != "0123456789" {
		t.Fatalf("备份内容错误: %q", got)
	}
	if got := readFile(t, w.currentLogPath()); got != "tail" {
		t.Fatalf("新文件内容错误: %q", got)
	}
}

// 同一秒内连续轮转时，备份文件名必须互不冲突（os.Rename 目标已存在会失败）
func TestRotatingWriter_BackupNameNoCollision(t *testing.T) {
	w, _ := newClockWriter(t, 4, 0, false)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	w.nowFunc = func() time.Time { return fixed }

	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte("data-data")); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i, err)
		}
		w.mu.Lock()
		w.rotate()
		w.mu.Unlock()
	}

	backups := listFiles(t, w.filePath, "probe_*.log")
	if len(backups) != 3 {
		t.Fatalf("3 次轮转应留下 3 个互不覆盖的备份，实际=%d (%v)", len(backups), backups)
	}
	for _, b := range backups {
		if size := fileSize(t, b); size == 0 {
			t.Fatalf("备份文件 %s 内容丢失", b)
		}
	}
}

// rename 失败时不得把 currentSize 归零后继续写原文件（体积失真），
// 也不得丢掉已写入的日志
func TestRotatingWriter_RenameFailureKeepsSize(t *testing.T) {
	w, clock := newClockWriter(t, 4, 0, false)

	if _, err := w.Write([]byte("first-second")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	w.renameFunc = func(_, _ string) error { return fmt.Errorf("mock rename failed") }

	clock.Advance(2 * time.Second)
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("rename 失败后写入应当继续成功: %v", err)
	}

	sizeOnDisk := fileSize(t, w.currentLogPath())
	if w.currentSize != sizeOnDisk {
		t.Fatalf("rename 失败后 currentSize=%d 与磁盘实际大小=%d 不一致", w.currentSize, sizeOnDisk)
	}
	if !strings.Contains(readFile(t, w.currentLogPath()), "first-second") {
		t.Fatal("rename 失败后原日志内容被截断丢失")
	}
}

// 轮转后新建文件失败时句柄置空，下次写入必须惰性重建而不是永久静默
func TestRotatingWriter_LazyReopenAfterFailure(t *testing.T) {
	w, _ := newClockWriter(t, 1024, 0, false)

	// 模拟 rotate 中 OpenFile 失败后的状态：句柄为空且目录缺失
	w.mu.Lock()
	_ = w.file.Close()
	w.file = nil
	w.mu.Unlock()

	if err := os.RemoveAll(w.filePath); err != nil {
		t.Fatalf("清理目录失败: %v", err)
	}
	if _, err := w.Write([]byte("dropped?")); err == nil {
		t.Fatal("目录缺失时写入应返回错误而不是静默成功")
	}

	if err := os.MkdirAll(w.filePath, 0755); err != nil {
		t.Fatalf("重建目录失败: %v", err)
	}
	if _, err := w.Write([]byte("recovered")); err != nil {
		t.Fatalf("目录恢复后写入应自愈: %v", err)
	}
	if got := readFile(t, w.currentLogPath()); got != "recovered" {
		t.Fatalf("自愈后内容错误: %q", got)
	}
}

// Close 前必须 Sync，关闭后文件内容完整且后台协程全部退出
func TestRotatingWriter_ClosePersistsAndStops(t *testing.T) {
	w, err := NewRotatingFileWriter(t.TempDir(), "probe", 1<<20, 1, 5, true)
	if err != nil {
		t.Fatalf("创建轮转写入器失败: %v", err)
	}

	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	start := time.Now()
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close 未等待后台清理协程退出: %v", elapsed)
	}

	if got := readFile(t, w.currentLogPath()); got != sb.String() {
		t.Fatalf("Close 后文件内容不完整，长度 %d != %d", len(got), sb.Len())
	}

	// 关闭后写入必须明确报错，而不是写到已释放的句柄上
	if _, err := w.Write([]byte("after-close")); err == nil {
		t.Fatal("Close 后写入应返回错误")
	}
}

// 过期清理按 maxAge 判定，且只处理本前缀的备份文件
func TestRotatingWriter_CleanupOldLogsByAge(t *testing.T) {
	w, clock := newClockWriter(t, 1024, 0, false)
	w.maxAge = 1

	oldBackup := filepath.Join(w.filePath, "probe_2020-01-01_00-00-00.log")
	freshBackup := filepath.Join(w.filePath, "probe_2020-01-02_00-00-00.log")
	activeLog := filepath.Join(w.filePath, "probe.log")
	freshArchive := filepath.Join(w.filePath, "probe_2020-01-03_00-00-00.log.gz")
	oldArchive := filepath.Join(w.filePath, "probe_2020-01-04_00-00-00.log.gz")

	now := clock.Now()
	for _, p := range []string{oldBackup, freshBackup, activeLog, freshArchive, oldArchive} {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("预置文件失败: %v", err)
		}
		if err := os.Chtimes(p, now, now); err != nil {
			t.Fatalf("设置时间失败: %v", err)
		}
	}

	tenDaysAgo := now.AddDate(0, 0, -10)
	for _, p := range []string{oldBackup, activeLog, oldArchive} {
		if err := os.Chtimes(p, tenDaysAgo, tenDaysAgo); err != nil {
			t.Fatalf("设置时间失败: %v", err)
		}
	}

	w.cleanupOldLogs()

	if _, err := os.Stat(oldBackup); !os.IsNotExist(err) {
		t.Fatal("超过 maxAge 的备份未被清理")
	}
	if _, err := os.Stat(oldArchive); !os.IsNotExist(err) {
		t.Fatal("超过 maxAge 的压缩包未被清理")
	}
	if _, err := os.Stat(freshBackup); err != nil {
		t.Fatalf("未过期的备份被误删: %v", err)
	}
	if _, err := os.Stat(freshArchive); err != nil {
		t.Fatalf("未过期的压缩包被误删: %v", err)
	}
	// 活动文件不带 "probe_" 前缀，即使 mtime 很旧也不能被清理
	if _, err := os.Stat(activeLog); err != nil {
		t.Fatalf("活动日志文件被误删: %v", err)
	}
}

// 过期清理由构造启动、由 Close 收束，不残留协程
func TestRotatingWriter_AgeCleanupGoroutineStops(t *testing.T) {
	w, err := NewRotatingFileWriter(t.TempDir(), "probe", 1<<20, 7, 5, false)
	if err != nil {
		t.Fatalf("创建轮转写入器失败: %v", err)
	}

	before := runtime.NumGoroutine()
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() < before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Close 后仍有残留协程: before=%d after=%d", before, runtime.NumGoroutine())
}

// 超额备份清理只保留 maxBackups 个最新文件
func TestRotatingWriter_CleanupOldBackupsKeepsMax(t *testing.T) {
	w, clock := newClockWriter(t, 1024, 2, false)

	base := clock.Now()
	for i := 0; i < 5; i++ {
		p := filepath.Join(w.filePath, fmt.Sprintf("probe_bak%d.log", i))
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("预置备份失败: %v", err)
		}
		stamp := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatalf("设置时间失败: %v", err)
		}
	}

	w.cleanupOldBackups()

	if got := len(listFiles(t, w.filePath, "probe_bak*.log")); got != 2 {
		t.Fatalf("maxBackups=2 应只剩 2 个备份，实际=%d", got)
	}
}
