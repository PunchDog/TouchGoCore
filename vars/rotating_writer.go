package vars

import (
	"compress/gzip"
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
}

// NewRotatingFileWriter 创建旋转日志写入器
func NewRotatingFileWriter(filePath, fileName string, maxSize int64, maxAge, maxBackups int, compress bool) (*RotatingFileWriter, error) {
	// 确保目录存在
	if err := os.MkdirAll(filePath, 0755); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDirectoryCreateFailed, err)
	}

	fullPath := path.Join(filePath, fileName+".log")

	// 打开或创建文件
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFileCreateFailed, err)
	}

	// 获取当前文件大小
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to stat file: %v", err)
	}

	writer := &RotatingFileWriter{
		file:            file,
		currentSize:     info.Size(),
		maxSize:         maxSize,
		maxAge:          maxAge,
		maxBackups:      maxBackups,
		compress:        compress,
		filePath:        filePath,
		fileName:        fileName,
		lastRotateCheck: time.Now(),
	}

	// 启动时清理旧日志
	go writer.cleanupOldLogs()

	return writer, nil
}

// Write 实现io.Writer接口
func (w *RotatingFileWriter) Write(p []byte) (n int, err error) {
	if w.closed.Load() {
		return 0, fmt.Errorf("writer is closed")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

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

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}

	return nil
}

// checkRotate 检查并执行轮转
func (w *RotatingFileWriter) checkRotate() {
	// 限制检查频率
	if time.Since(w.lastRotateCheck) < time.Second {
		return
	}
	w.lastRotateCheck = time.Now()

	// 检查文件大小
	if w.maxSize > 0 && w.currentSize >= w.maxSize {
		w.rotate()
	}
}

// rotate 执行日志轮转
func (w *RotatingFileWriter) rotate() {
	if w.file == nil {
		return
	}

	// 关闭当前文件
	if err := w.file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close log file: %v\n", err)
	}

	// 重命名当前文件
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	rotatedPath := path.Join(w.filePath, fmt.Sprintf("%s_%s.log", w.fileName, timestamp))

	if err := os.Rename(path.Join(w.filePath, w.fileName+".log"), rotatedPath); err != nil {
		fmt.Fprintf(os.Stderr, "failed to rotate log file: %v\n", err)
	}

	// 异步压缩旧日志
	if w.compress {
		go w.compressFile(rotatedPath)
	}

	// 清理旧备份
	go w.cleanupOldBackups()

	// 创建新文件
	newPath := path.Join(w.filePath, w.fileName+".log")
	file, err := os.OpenFile(newPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create new log file: %v\n", err)
		return
	}

	w.file = file
	w.currentSize = 0
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

	cutoff := time.Now().AddDate(0, 0, -w.maxAge)

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
