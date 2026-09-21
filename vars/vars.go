package vars

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// 全局日志器状态：Initialize/Shutdown 之间的切换由 loggerMu 串行化，
// loggerActive 只是「当前这轮是否已装好」的标记（旧实现是 sync.Once，
// 一旦 Shutdown 就无法再 Initialize，重启日志器只能换进程）。
var (
	loggerMu     sync.Mutex
	loggerActive bool
)

// Run 初始化全局日志器（向后兼容入口）
func Run(path, name, level string) {
	cfg := DefaultConfig()
	cfg.LogLevel = level
	cfg.LogPath = path
	cfg.LogName = name
	if err := Initialize(cfg); err != nil {
		// 日志器尚未就绪，只能直接打到 stderr，否则初始化失败会完全静默
		fmt.Fprintf(os.Stderr, "日志初始化失败: %v\n", err)
	}
}

// Initialize 初始化全局日志器（默认使用异步Channel模式）。
//
// 只有一条路径（S70）：文件句柄从头到尾归 ChannelLoggerManager 独占。过去这里还会
// 另建一个同步 LoggerManager 当「fallback」，两个管理器各开一个句柄写同一个 .log，
// 而旁路那个句柄不认识旋转——日志轮转后它继续往被改名的备份文件里追加，主文件从此
// 看不出后半段现场；需要降级时由管理器自己同步写自己那个句柄。
//
// 返回聚合后的初始化错误：任一子路径失败都不再被吞掉，调用方仍可选择忽略。
// 允许 Shutdown 之后再次 Initialize（旧实现用 sync.Once 焊死，重启日志器只能换进程）。
func Initialize(cfg LogConfig) error {
	loggerMu.Lock()
	defer loggerMu.Unlock()

	// 默认启用异步模式以保证引擎效率
	if cfg.AsyncBufferSize <= 0 {
		cfg.AsyncBufferSize = 10000
	}

	if loggerActive {
		return nil
	}
	manager, err := initializeChannelLoggerLocked(cfg)
	if err != nil {
		return fmt.Errorf("异步日志器初始化失败: %w", err)
	}
	loggerActive = true

	if manager.IsEnabled() {
		adoptSlogDefault(manager)
	}
	return nil
}

// slogPrevDefault 被接管前的默认 slog 记录器。Shutdown 之后必须换回去：
// 默认器还指着已关闭管理器的 handler，进程收尾阶段（退出日志、panic 打印）的
// slog.Info 会静默消失，而这类日志恰恰是最不能丢的。
var slogPrevDefault *slog.Logger

// adoptSlogDefault 接管默认 slog 记录器。调用方必须持有 loggerMu。
func adoptSlogDefault(manager *ChannelLoggerManager) {
	if slogPrevDefault == nil {
		slogPrevDefault = slog.Default()
	}
	slog.SetDefault(manager.GetLogger())
}

// releaseSlogDefault 交还默认 slog 记录器。调用方必须持有 loggerMu。
func releaseSlogDefault() {
	if slogPrevDefault == nil {
		return
	}
	slog.SetDefault(slogPrevDefault)
	slogPrevDefault = nil
}

// InitializeWithDefaults 使用默认配置初始化
func InitializeWithDefaults() error {
	return Initialize(DefaultConfig())
}

// Shutdown 关闭全局日志器。关闭后文件写入静默丢弃（命令行仍在），
// 再次 Initialize 可恢复。
func Shutdown() error {
	loggerMu.Lock()
	defer loggerMu.Unlock()

	var errs []error

	if err := shutdownChannelLoggerLocked(); err != nil {
		errs = append(errs, err)
	}
	loggerActive = false
	releaseSlogDefault()

	// 命令行缓冲最后落盘，避免退出时丢掉尾部输出
	if err := CloseConsole(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ==================== 日志门面（命令行 + 文件双通道） ====================

// verbBytes 占位符动词集合（fmt 标准库允许的全部动词字母）
const verbBytes = "bcdefFGgopsqStTvxXUwW"

// scanPercent 从 format[i]（必为 '%'）起尝试匹配一个合法的 fmt 片段。
// 返回该片段结束后的下标、它是否为真正的占位符、以及匹配是否合法；
// 不合法时说明这是一个裸百分号（如中文里的 "50%"）。
func scanPercent(format string, i int) (end int, verb bool, ok bool) {
	j := i + 1
	if j >= len(format) {
		return 0, false, false // 串尾裸百分号
	}
	if format[j] == '%' {
		return j + 1, false, true // %% 是字面百分号，不是占位符
	}

	// 可选参数索引 %[n]
	if format[j] == '[' {
		idx := strings.IndexByte(format[j:], ']')
		if idx < 0 {
			return 0, false, false
		}
		j += idx + 1
		if j >= len(format) {
			return 0, false, false
		}
	}

	for j < len(format) && strings.IndexByte("+ #-0", format[j]) >= 0 {
		j++
	}
	for j < len(format) && (format[j] == '*' || isDigitByte(format[j])) {
		j++
	}
	if j < len(format) && format[j] == '.' {
		j++
		for j < len(format) && (format[j] == '*' || isDigitByte(format[j])) {
			j++
		}
	}

	if j < len(format) && strings.IndexByte(verbBytes, format[j]) >= 0 {
		return j + 1, true, true
	}
	return 0, false, false
}

// countFormatVerbs 统计动态格式串中占位符（verb）的数量。
// '%%' 与 "100% 完成" 这类字面百分号不计入，避免把参数错误地喂给 Sprintf。
func countFormatVerbs(format string) int {
	count := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		end, verb, ok := scanPercent(format, i)
		if !ok {
			continue
		}
		if verb {
			count++
		}
		i = end - 1
	}
	return count
}

// escapeStrayPercents 把格式串里不构成合法占位符的裸百分号补写成 %%，
// 使 "进度 50% 命中 %d" 这类中文消息不会把后面的字节当成动词、
// 进而吃掉真实参数（曾输出 "50%!命(int=7)中 %!d(MISSING)"）。
// 无裸百分号时原样返回，不产生额外分配。
func escapeStrayPercents(format string) string {
	var sb strings.Builder
	written := -1

	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		end, _, ok := scanPercent(format, i)
		if ok {
			i = end - 1
			continue
		}
		if written < 0 {
			sb.Grow(len(format) + 8)
			written = 0
		}
		sb.WriteString(format[written:i])
		sb.WriteString("%%")
		written = i + 1
	}

	if written < 0 {
		return format
	}
	sb.WriteString(format[written:])
	return sb.String()
}

func isDigitByte(c byte) bool {
	return c >= '0' && c <= '9'
}

// hasFormatVerbs 判断字符串是否包含 fmt 风格的格式化占位符（如 %s、%d）。
// 连续的 %% 以及后不跟动词的裸百分号（如 "100%"）表示字面百分号，不算占位符。
func hasFormatVerbs(s string) bool {
	return countFormatVerbs(s) > 0
}

// assembleLine 统一格式化日志消息：
//   - 首参含 %s 等格式化占位符时，按 fmt.Sprintf 处理（裸百分号先转义）；
//   - 首参不含占位符时，将所有参数按顺序直接拼接（以空格分隔），避免多余参数被丢弃。
//
// 参数签名采用 []any 而非 ...any，避开 go vet 对 (string, ...any) 形式的 printf 静态分析。
func assembleLine(msg string, args []any) string {
	if len(args) == 0 {
		return msg
	}
	if !hasFormatVerbs(msg) {
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

	return sprintfDynamic(escapeStrayPercents(msg), args)
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
// Debug 受日志级别约束（高并发下每条请求打 Debug 会把输出锁打满）；
// Info/Warn/Error 仍始终打到命令行，便于实时观察。
func printToConsole(level slog.Level, msg string) {
	if level <= slog.LevelDebug {
		if ch := GetChannelLogger(); ch != nil && !ch.ShouldWriteFile(level) {
			return
		}
	}
	ts := time.Now().Format("15:04:05")
	writeConsoleLine(fmt.Sprintf("%s %s %s\n", ts, consoleLevelPrefix(level), msg))
}

// ==================== 命令行输出缓冲 ====================

const (
	consoleBufferSize    = 8 * 1024
	consoleFlushInterval = 100 * time.Millisecond
)

var (
	consoleMu        sync.Mutex
	consoleWriter    *bufio.Writer
	consoleTicker    *time.Ticker
	consoleFlushStop chan struct{}
	consoleClosed    bool
)

// writeConsoleLine 把一行日志写入命令行缓冲。
// 缓冲写失败或已关闭时退回直接输出，保证日志不会因缓冲而丢失。
func writeConsoleLine(line string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if consoleClosed {
		os.Stdout.WriteString(line)
		return
	}

	if consoleWriter == nil {
		consoleWriter = bufio.NewWriterSize(os.Stdout, consoleBufferSize)
		startConsoleFlusherLocked()
	}

	if _, err := consoleWriter.WriteString(line); err != nil {
		_ = consoleWriter.Flush()
		_, _ = os.Stdout.WriteString(line)
	}
}

// startConsoleFlusherLocked 定期把缓冲刷到终端，调用方必须持有 consoleMu
func startConsoleFlusherLocked() {
	ticker := time.NewTicker(consoleFlushInterval)
	consoleTicker = ticker
	consoleFlushStop = make(chan struct{})
	stop := consoleFlushStop

	go func() {
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = FlushConsole()
			}
		}
	}()
}

// FlushConsole 立即把命令行缓冲区的内容落屏
func FlushConsole() error {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if consoleWriter == nil {
		return nil
	}
	return consoleWriter.Flush()
}

// CloseConsole 停止定期刷屏协程并把剩余内容落屏；关闭后写入退回直接输出
func CloseConsole() error {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if consoleClosed {
		return nil
	}
	consoleClosed = true

	if consoleFlushStop != nil {
		close(consoleFlushStop)
		consoleFlushStop = nil
	}
	if consoleTicker != nil {
		consoleTicker.Stop()
		consoleTicker = nil
	}

	var err error
	if consoleWriter != nil {
		err = consoleWriter.Flush()
		consoleWriter = nil
	}
	return err
}

// writeToFile 按配置日志级别把一行日志交给日志管理器落盘。
//
// 未初始化（或已 Shutdown）时只丢文件、不再另开句柄补写：命令行那一路已经先跑过了，
// 消息不会整体消失；而这里过去会退回一个独立的同步 LoggerManager，那第二个文件句柄
// 就是「轮转之后日志写进备份文件」的来源。
func writeToFile(level slog.Level, msg string) {
	ch := GetChannelLogger()
	if ch == nil {
		warnLoggerMissing()
		return
	}
	ch.writeLogged(level, msg, nil)
}

// warnLoggerMissing 未初始化提示只报一次：这条路径本身在日志器上，反复打印会刷屏，
// 而 Initialize 失败的原因已经由调用方拿到并打在 stderr 上。
func warnLoggerMissing() {
	loggerMissingOnce.Do(func() {
		fmt.Fprint(os.Stderr, "日志管理器未初始化，文件日志被跳过（命令行输出不受影响）\n")
	})
}

var loggerMissingOnce sync.Once

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

// 初始化函数（默认使用默认配置）
func init() {
	// 默认初始化，但允许后续重新配置
	// _ = InitializeWithDefaults()
}
