package vars

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 全局变量（使用单例模式）
var (
	globalLogger *LoggerManager
	once         sync.Once
)

// Run 初始化全局日志器（向后兼容入口）
func Run(path, name, level string) {
	cfg := DefaultConfig()
	cfg.LogLevel = level
	cfg.LogPath = path
	cfg.LogName = name
	Initialize(cfg)
}

// Initialize 初始化全局日志器（默认使用异步Channel模式）
func Initialize(cfg LogConfig) error {
	var initErr error
	once.Do(func() {
		// 默认启用异步模式以保证引擎效率
		if cfg.AsyncBufferSize <= 0 {
			cfg.AsyncBufferSize = 10000
		}

		// 创建基于Channel的异步日志管理器
		channelManager, err := NewChannelLoggerManager(cfg)
		if err != nil {
			initErr = err
			return
		}

		// 如果启用异步，使用Channel模式
		if cfg.Async {
			initErr = channelManager.initWithChannel()
			if initErr != nil {
				return
			}
		}

		// 存储到全局变量（保持兼容性）
		globalLogger, initErr = NewLoggerManager(cfg)
		if initErr != nil {
			// 如果旧模式失败，使用Channel管理器
			initErr = nil
		}

		// 将Channel管理器也设为全局
		InitializeChannelLogger(cfg)

		if GetChannelLogger() != nil && GetChannelLogger().IsEnabled() {
			slog.SetDefault(GetChannelLogger().GetLogger())
		}
	})

	return initErr
}

// InitializeWithDefaults 使用默认配置初始化
func InitializeWithDefaults() error {
	return Initialize(DefaultConfig())
}

// Shutdown 关闭全局日志器
func Shutdown() error {
	// 先关闭异步Channel日志（确保所有日志刷出）
	if err := ShutdownChannelLogger(); err != nil {
		return err
	}

	if globalLogger == nil {
		return nil
	}

	return globalLogger.Close()
}

// ==================== 日志门面（命令行 + 文件双通道） ====================

// hasFormatVerbs 判断字符串是否包含 fmt 风格的格式化占位符（如 %s、%d）。
// 连续的 %% 表示字面的百分号，不算作占位符。
func hasFormatVerbs(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			if i+1 < len(s) && s[i+1] == '%' {
				i++ // 跳过的转义百分号 %%
				continue
			}
			return true
		}
	}
	return false
}

// assembleLine 统一格式化日志消息：
//   - 首参含 %s 等格式化占位符时，按 fmt.Sprintf 处理；
//   - 首参不含占位符时，将所有参数按顺序直接拼接（以空格分隔），避免多余参数被丢弃。
//
// 参数签名采用 []any 而非 ...any，避开 go vet 对 (string, ...any) 形式的 printf 静态分析。
func assembleLine(msg string, args []any) string {
	if len(args) == 0 {
		return msg
	}
	if hasFormatVerbs(msg) {
		return sprintfDynamic(msg, args)
	}

	// 无格式化占位符：将所有参数直接拼接（首参 + 各参数，空格分隔）
	var sb strings.Builder
	sb.Grow(len(msg) + len(args)*16)
	sb.WriteString(msg)
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(fmt.Sprint(a))
	}
	return sb.String()
}

// sprintfDynamic 使用动态格式串的 Sprintf 封装。
func sprintfDynamic(format string, args []any) string {
	return fmt.Sprintf(format, args...)
}

// consoleLevelPrefix 返回命令行输出使用的级别前缀
func consoleLevelPrefix(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "[DEBUG]"
	case slog.LevelInfo:
		return "[INFO]"
	case slog.LevelWarn:
		return "[WARN]"
	case slog.LevelError:
		return "[ERROR]"
	default:
		return "[INFO]"
	}
}

// printToConsole 将日志同步输出到命令行。
// Debug 受日志级别约束（高并发下每条请求打 Debug 会把 fmt.Printf 锁打满）；
// Info/Warn/Error 仍始终打到命令行，便于实时观察。
func printToConsole(level slog.Level, msg string) {
	if level <= slog.LevelDebug {
		if ch := GetChannelLogger(); ch != nil && !ch.ShouldWriteFile(level) {
			return
		}
	}
	ts := time.Now().Format("15:04:05")
	fmt.Printf("%s %s %s\n", ts, consoleLevelPrefix(level), msg)
}

// writeToFile 按配置日志级别将日志异步写入 log 文件；
// 级别不足、日志器未启用或级别为 off 时跳过（不写文件，但命令行已先行输出）。
func writeToFile(level slog.Level, msg string) {
	ch := GetChannelLogger()
	if ch == nil || !ch.IsEnabled() {
		// 回退到旧版日志器（其内部已按等级过滤）
		logWithLevel(level, "%s", msg)
		return
	}
	if ch.IsOff() || !ch.ShouldWriteFile(level) {
		return
	}
	ch.LogAsyncSimple(level, msg)
}

// Debug 调试级别日志：优先输出到命令行，其次按等级写入 log 文件
func Debug(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := assembleLine(msg, args)
	printToConsole(slog.LevelDebug, formatted)
	writeToFile(slog.LevelDebug, formatted)
}

// Info 信息级别日志：优先输出到命令行，其次按等级写入 log 文件
func Info(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := assembleLine(msg, args)
	printToConsole(slog.LevelInfo, formatted)
	writeToFile(slog.LevelInfo, formatted)
}

// Warning 警告级别日志：优先输出到命令行，其次按等级写入 log 文件
func Warning(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := assembleLine(msg, args)
	printToConsole(slog.LevelWarn, formatted)
	writeToFile(slog.LevelWarn, formatted)
}

// Error 错误级别日志：优先输出到命令行，其次按等级写入 log 文件
func Error(msg string, args ...any) {
	if ch := GetChannelLogger(); ch != nil && ch.IsOff() {
		return // off 完全静默
	}
	formatted := assembleLine(msg, args)
	printToConsole(slog.LevelError, formatted)
	writeToFile(slog.LevelError, formatted)
}

// logWithLevel 统一的日志记录函数
func logWithLevel(level slog.Level, msg string, args ...any) {
	var formattedMsg string
	if len(args) > 0 {
		// 如果有参数，使用fmt.Sprintf格式化消息
		formattedMsg = fmt.Sprintf(msg, args...)
	} else {
		// 没有参数，直接使用原始消息
		formattedMsg = msg
	}

	if globalLogger == nil || !globalLogger.IsEnabled() {
		// 使用简单的fmt输出作为fallback
		logSimple(level, formattedMsg)
		return
	}

	logger := globalLogger.GetLogger()

	switch level {
	case slog.LevelDebug:
		logger.Debug(formattedMsg)
	case slog.LevelInfo:
		logger.Info(formattedMsg)
	case slog.LevelWarn:
		logger.Warn(formattedMsg)
	case slog.LevelError:
		logger.Error(formattedMsg)
	}
}

// logSimple 简单的日志输出（用于未初始化时）
func logSimple(level slog.Level, msg string) {
	levelStr := "[INFO]"
	switch level {
	case slog.LevelDebug:
		levelStr = "[DEBUG]"
	case slog.LevelWarn:
		levelStr = "[WARN]"
	case slog.LevelError:
		levelStr = "[ERROR]"
	}

	fmt.Printf("%s %s\n", levelStr, msg)
}

// 初始化函数（默认使用默认配置）
func init() {
	// 默认初始化，但允许后续重新配置
	// _ = InitializeWithDefaults()
}
