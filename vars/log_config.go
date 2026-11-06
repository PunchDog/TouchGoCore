package vars

import (
	"errors"
	"fmt"
	"strings"
)

// 错误定义
var (
	ErrLoggerNotInitialized  = errors.New("logger not initialized")
	ErrInvalidLogLevel       = errors.New("invalid log level")
	ErrFileCreateFailed      = errors.New("failed to create log file")
	ErrDirectoryCreateFailed = errors.New("failed to create log directory")
)

// 日志级别枚举
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
	LogLevelOff   = "off"
)

// LogConfig 日志配置；级别与异步参数均可在运行期经 SetLevel 调整。
type LogConfig struct {
	LogPath         string            // 落盘目录；轮转与过期清理都基于该目录扫描
	LogName         string            // 日志文件主名（不含扩展名与轮转时间戳）
	LogLevel        string            // 文件写入级别；off 表示完全静默
	MaxSize         int64             // 单文件轮转阈值（MB）
	MaxAge          int               // 备份保留天数，超龄文件被清理
	MaxBackups      int               // 同时保留的备份数上限
	Compress        bool              // 轮转后的备份是否 gzip 压缩
	Stdout          bool              // 是否同时实时打印到标准输出
	Async           bool              // 是否启用异步通道落盘
	AsyncBufferSize int               // 异步通道缓冲容量，决定峰值内存与积压上限
	CallerSkip      int               // 调用栈上跳层数，定位业务代码而非框架帧
	Fields          map[string]string // 每条日志都附带的全局字段
}

// Validate 验证配置有效性
func (cfg *LogConfig) Validate() error {
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100
	}
	if cfg.MaxAge < 0 {
		cfg.MaxAge = 30
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = 10
	}
	if cfg.AsyncBufferSize <= 0 {
		cfg.AsyncBufferSize = 10000
	}
	if cfg.CallerSkip < 0 {
		cfg.CallerSkip = 1
	}

	validLevels := map[string]bool{
		LogLevelDebug: true,
		LogLevelInfo:  true,
		LogLevelWarn:  true,
		LogLevelError: true,
		LogLevelOff:   true,
	}

	level := strings.ToLower(cfg.LogLevel)
	if !validLevels[level] {
		return fmt.Errorf("invalid log level: %s", cfg.LogLevel)
	}
	cfg.LogLevel = level // 归一为小写：后续比较统一走小写，避免 "INFO" 与 "info" 不一致

	return nil
}

// DefaultConfig 默认配置
func DefaultConfig() LogConfig {
	return LogConfig{
		LogPath:         "./logs",
		LogName:         "default",
		LogLevel:        LogLevelDebug,
		MaxSize:         100,   // 100MB
		MaxAge:          30,    // 30天
		MaxBackups:      10,    // 最大备份数
		Compress:        false, // 默认保留明文备份，轮转后不再生成 .gz
		Stdout:          true,
		Async:           true,  // 异步日志
		AsyncBufferSize: 10000, // 异步缓冲区
		CallerSkip:      1,     // 调用栈跳过
		Fields:          make(map[string]string),
	}
}
