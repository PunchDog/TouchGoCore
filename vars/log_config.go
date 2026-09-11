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

// LogConfig 日志配置结构
// 使用结构体封装配置，提高可维护性
type LogConfig struct {
	LogPath         string            // 日志文件路径
	LogName         string            // 日志文件名（不含扩展名）
	LogLevel        string            // 日志级别
	MaxSize         int64             // 日志文件最大大小（MB）
	MaxAge          int               // 日志文件最大保留天数
	MaxBackups      int               // 最大备份数量（新增）
	Compress        bool              // 是否压缩旧日志
	Stdout          bool              // 是否输出到标准输出
	Async           bool              // 是否异步日志（新增）
	AsyncBufferSize int               // 异步缓冲区大小（新增）
	CallerSkip      int               // 调用栈跳过层数（新增）
	Fields          map[string]string // 全局字段（新增）
}

// Validate 验证配置有效性
func (cfg *LogConfig) Validate() error {
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 100 // 设置默认值
	}
	if cfg.MaxAge < 0 {
		cfg.MaxAge = 30 // 设置默认值
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = 10 // 设置默认值
	}
	if cfg.AsyncBufferSize <= 0 {
		cfg.AsyncBufferSize = 10000 // 设置默认值
	}
	if cfg.CallerSkip < 0 {
		cfg.CallerSkip = 1 // 设置默认值
	}

	// 验证日志级别
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
	cfg.LogLevel = level

	return nil
}

// DefaultConfig 默认配置
func DefaultConfig() LogConfig {
	return LogConfig{
		LogPath:         "./logs",
		LogName:         "default",
		LogLevel:        LogLevelDebug,
		MaxSize:         100, // 100MB
		MaxAge:          30,  // 30天
		MaxBackups:      10,  // 最大备份数
		Compress:        true,
		Stdout:          true,
		Async:           true,  // 异步日志
		AsyncBufferSize: 10000, // 异步缓冲区
		CallerSkip:      1,     // 调用栈跳过
		Fields:          make(map[string]string),
	}
}
