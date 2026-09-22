package vars

import (
	"runtime"
	"strings"
)

// getCaller 获取业务调用者的文件路径与行号。
// 一次 runtime.Callers 采集整条栈、一次 CallersFrames 展开，跳过本包（vars）内部
// 帧，返回第一个不属于 vars 的调用者——即真正发起日志的业务代码位置。
//
// 旧实现对 depth=1..15 逐个调用 runtime.Caller：每次都要从栈顶重新走一遍（合计
// O(深度²)），且每次 Callers/FuncForPC 各分配一次，本机实测单条日志 4.3µs / 15 次
// 分配。同时判定用的是 Contains("/vars.")，任何路径里带 /vars. 的第三方包
// （example.com/vars.Foo）都会被误当成本包帧而跳过，定位到更外层。
func getCaller() (string, int) {
	// 采集代价与窗口大小成正比（从栈顶逐帧走到窗口上限）：常规链只有个位数 vars
	// 内部帧，先用小窗口；小窗口采满仍未见业务帧才说明链异常深，再用大窗口重来。
	// 两个窗口各自定长，小窗口不参与大窗口的逃逸分析，常态路径不会把 64 帧的数组
	// 顶上堆。
	var fast [callerFastFrames]uintptr
	if file, line, ok := expandCaller(fast[:]); ok {
		return file, line
	}

	var full [callerFrameLimit]uintptr
	if file, line, ok := expandCaller(full[:]); ok {
		return file, line
	}
	return "", 0
}

// expandCaller 展开 pcs 采到的帧，返回第一个不属于 vars 包的调用者位置。
// ok=false 表示窗口内全是本包帧（或整条栈都不属于业务），调用方应换更大的窗口重试。
func expandCaller(pcs []uintptr) (string, int, bool) {
	n := runtime.Callers(2, pcs)
	if n == 0 {
		return "", 0, false
	}

	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.File != "" && !strings.HasPrefix(frame.Function, varsFuncPrefix) {
			return normalizeCallerFile(frame.File), frame.Line, true
		}
		if !more {
			return "", 0, false
		}
	}
}

// callerFromPC 把 slog.Record 自带的调用点 PC 解析成与 getCaller 同一风格的路径与行号。
//
// 走 slog.* 入口的日志不能再用 getCaller 扫栈：栈里第一个非 vars 帧是 log/slog
// 自己的转发帧，调用点会被错报到 Go 标准库文件上。Record.PC 是 slog 在调用点采好的。
func callerFromPC(pc uintptr) (string, int) {
	if pc == 0 {
		return "", 0
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "", 0
	}
	file, line := fn.FileLine(pc)
	if file == "" {
		return "", 0
	}
	return normalizeCallerFile(file), line
}

// 小窗口覆盖 vars 内部帧（Info → writeToFile → writeLogged → getCaller）加余量；
// 采满仍未命中就退回大窗口重扫。
const (
	callerFastFrames = 8
	callerFrameLimit = 64
)

// normalizeCallerFile 统一路径分隔符：Windows 下与系统文件路径风格一致。
// 只有真的含正斜杠时才重建字符串，同一调用点反复打日志时不再每次分配。
func normalizeCallerFile(file string) string {
	if isWindows && strings.Contains(file, "/") {
		return strings.ReplaceAll(file, "/", "\\")
	}
	return file
}

const isWindows = runtime.GOOS == "windows"

// varsFuncPrefix 本包函数名的公共前缀（形如 "touchgocore/vars."），用于精确判定
// 「这一帧是不是 vars 内部帧」。前缀在包初始化时从自身函数名反推，换模块路径或
// 被 vendor 后依然成立。
var varsFuncPrefix = computeVarsFuncPrefix()

func computeVarsFuncPrefix() string {
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return ""
	}
	name := fn.Name() // 形如 touchgocore/vars.computeVarsFuncPrefix
	tail := name[strings.LastIndexByte(name, '/')+1:]
	if i := strings.IndexByte(tail, '.'); i >= 0 {
		return name[:len(name)-len(tail)+i+1]
	}
	return name
}
