package vars

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 优化的旋转日志写入器 ====================

// ageCleanupInterval 过期日志的定期清理间隔
const ageCleanupInterval = time.Hour

// RotatingFileWriter 实现日志轮转
type RotatingFileWriter struct {
	file            *os.File
	currentSize     int64
	maxSize         int64
	maxAge          int
	maxBackups      int
	compress        bool
	filePath        string
	fileName        string
	mu              sync.Mutex
	closed          atomic.Bool
	lastRotateCheck time.Time
	cleanupCtx      context.Context
	cleanupCancel   context.CancelFunc
	wg              sync.WaitGroup
	nowFunc         func() time.Time
	renameFunc      func(oldPath, newPath string) error
}

// NewRotatingFileWriter 创建旋转日志写入器
func NewRotatingFileWriter(filePath, fileName string, maxSize int64, maxAge, maxBackups int, compress bool) (*RotatingFileWriter, error) {
	// 确保目录存在
	if err := os.MkdirAll(filePath, 0755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDirectoryCreateFailed, err)
	}

	writer := &RotatingFileWriter{
		maxSize:         maxSize,
		maxAge:          maxAge,
		maxBackups:      maxBackups,
		compress:        compress,
		filePath:        filePath,
		fileName:        fileName,
		lastRotateCheck: time.Now(),
		nowFunc:         time.Now,
		renameFunc:      os.Rename,
	}
	writer.cleanupCtx, writer.cleanupCancel = context.WithCancel(context.Background())

	// 打开或创建文件
	file, size, err := writer.openLogFile()
	if err != nil {
		writer.cleanupCancel()
		return nil, err
	}
	writer.file = file
	writer.currentSize = size

	// 启动过期日志清理（首次立即执行，之后按小时定期执行）
	writer.startAgeCleanup()

	return writer, nil
}

// currentLogPath 当前活动日志文件路径
func (w *RotatingFileWriter) currentLogPath() string {
	return path.Join(w.filePath, w.fileName+".log")
}

// openLogFile 打开当前日志文件（追加模式）并返回句柄与实际大小
func (w *RotatingFileWriter) openLogFile() (*os.File, int64, error) {
	file, err := os.OpenFile(w.currentLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrFileCreateFailed, err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("failed to stat file: %v", err)
	}

	return file, info.Size(), nil
}

// Write 实现io.Writer接口
func (w *RotatingFileWriter) Write(p []byte) (n int, err error) {
	if w.closed.Load() {
		return 0, fmt.Errorf("writer is closed")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// 取锁后复核关闭状态，避免与 Close 并发使用已释放的句柄
	if w.closed.Load() {
		return 0, fmt.Errorf("writer is closed")
	}

	// 轮转失败后句柄为空，此处惰性重建，避免日志永久全丢
	if w.file == nil {
		file, size, openErr := w.openLogFile()
		if openErr != nil {
			return 0, openErr
		}
		w.file = file
		w.currentSize = size
	}

	// 检查是否需要轮转
	w.checkRotate()

	n, err = w.file.Write(p)
	if err != nil {
		return n, err
	}

	w.currentSize += int64(n)

	// 检查写入后是否需要轮转
	w.checkRotate()

	return n, nil
}

// Sync 实现Sync方法
func (w *RotatingFileWriter) Sync() error {
	if w.closed.Load() {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		return w.file.Sync()
	}
	return nil
}

// Close 关闭写入器
func (w *RotatingFileWriter) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}

	// 先停止定期清理协程，再落盘关闭句柄
	w.cleanupCancel()

	w.mu.Lock()
	var err error
	if w.file != nil {
		if syncErr := w.file.Sync(); syncErr != nil {
			err = syncErr
		}
		if closeErr := w.file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		w.file = nil
	}
	w.mu.Unlock()

	// 等待压缩/清理协程收尾，避免进程退出时与文件删除竞争
	w.wg.Wait()

	return err
}

// checkRotate 检查并执行轮转
func (w *RotatingFileWriter) checkRotate() {
	// 限制检查频率
	if w.nowFunc().Sub(w.lastRotateCheck) < time.Second {
		return
	}
	w.lastRotateCheck = w.nowFunc()

	// 检查文件大小
	if w.maxSize > 0 && w.currentSize >= w.maxSize {
		w.rotate()
	}
}

// rotatedPath 生成不与已有备份冲突的轮转目标路径
// 同一秒内多次轮转（或上次轮转残留）都会让 os.Rename 因目标已存在而失败
func (w *RotatingFileWriter) rotatedPath() string {
	timestamp := w.nowFunc().Format("2006-01-02_15-04-05")
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("%s_%s.log", w.fileName, timestamp)
		if i > 0 {
			name = fmt.Sprintf("%s_%s_%d.log", w.fileName, timestamp, i)
		}
		candidate := path.Join(w.filePath, name)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return path.Join(w.filePath, fmt.Sprintf("%s_%d.log", w.fileName, w.nowFunc().UnixNano()))
}

// rotate 执行日志轮转（调用方必须持有 w.mu）
func (w *RotatingFileWriter) rotate() {
	if w.file == nil {
		return
	}

	// 轮转前先落盘，确保旧文件内容完整
	_ = w.file.Sync()

	// 关闭当前文件
	if err := w.file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close log file: %v\n", err)
	}
	w.file = nil
	w.currentSize = 0

	rotatedPath := w.rotatedPath()
	if err := w.renameFunc(w.currentLogPath(), rotatedPath); err != nil {
		// 重命名失败：旧文件仍在原位，按真实大小重新接管，
		// 不能把 currentSize 归零（否则体积永久失真），下次检查时自动重试轮转
		fmt.Fprintf(os.Stderr, "failed to rotate log file: %v\n", err)
		file, size, openErr := w.openLogFile()
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "failed to reopen log file: %v\n", openErr)
			return
		}
		w.file = file
		w.currentSize = size
		return
	}

	// 异步压缩旧日志与清理超额备份
	if w.compress {
		w.spawnCleanup(func() { w.compressFile(rotatedPath) })
	}
	w.spawnCleanup(w.cleanupOldBackups)

	// 创建新文件；失败时保持 w.file 为 nil，由 Write 惰性重建
	file, size, err := w.openLogFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create new log file: %v\n", err)
		return
	}
	w.file = file
	w.currentSize = size
}

// spawnCleanup 在关闭流程之外挂载一次性后台任务
func (w *RotatingFileWriter) spawnCleanup(task func()) {
	if w.closed.Load() || w.cleanupCtx.Err() != nil {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if w.cleanupCtx.Err() != nil {
			return
		}
		task()
	}()
}

// startAgeCleanup 启动过期日志清理循环：首次立即执行，之后每小时执行一次
func (w *RotatingFileWriter) startAgeCleanup() {
	if w.maxAge <= 0 {
		return
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()

		w.cleanupOldLogs()

		ticker := time.NewTicker(ageCleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-w.cleanupCtx.Done():
				return
			case <-ticker.C:
				w.cleanupOldLogs()
			}
		}
	}()
}

// compressFile 压缩日志文件
func (w *RotatingFileWriter) compressFile(filePath string) {
	gzipPath := filePath + ".gz"

	// 打开源文件
	src, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer src.Close()

	// 创建压缩文件
	dst, err := os.Create(gzipPath)
	if err != nil {
		return
	}
	defer dst.Close()

	// 执行压缩
	gw, _ := gzip.NewWriterLevel(dst, gzip.BestCompression)
	if gw != nil {
		defer gw.Close()
		if _, err := io.Copy(gw, src); err == nil {
			// 删除原始文件
			os.Remove(filePath)
		} else {
			os.Remove(gzipPath)
		}
	}
}

// cleanupOldBackups 清理旧备份
func (w *RotatingFileWriter) cleanupOldBackups() {
	if w.maxBackups <= 0 {
		return
	}

	// 列出所有备份文件
	pattern := path.Join(w.filePath, w.fileName+"_*.log*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	// 按修改时间排序
	type fileInfo struct {
		path    string
		modTime time.Time
	}

	var files []fileInfo
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			path:    match,
			modTime: info.ModTime(),
		})
	}

	// 按修改时间降序排序（最新的在前），使用标准库排序
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	// 删除超过maxBackups的文件
	if len(files) > w.maxBackups {
		for i := w.maxBackups; i < len(files); i++ {
			os.Remove(files[i].path)
		}
	}
}

// cleanupOldLogs 清理过期日志
func (w *RotatingFileWriter) cleanupOldLogs() {
	if w.maxAge <= 0 {
		return
	}

	cutoff := w.nowFunc().AddDate(0, 0, -w.maxAge)

	// 遍历日志目录
	err := filepath.Walk(w.filePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// 只处理日志文件
		if info.IsDir() || !strings.HasPrefix(info.Name(), w.fileName+"_") {
			return nil
		}

		// 检查文件年龄
		if info.ModTime().Before(cutoff) {
			os.Remove(path)
		}

		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to cleanup old logs: %v\n", err)
	}
}
