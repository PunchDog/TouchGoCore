package vars

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// logEntry 日志条目
type logEntry struct {
	level   slog.Level
	msg     string
	time    time.Time
	attrs   []slog.Attr
	context context.Context
	file    string // 调用者文件路径（新增）
	line    int    // 调用者行号（新增）
}

// batchEntries 批处理条目容器。
//
// bytes 随 append 增量维护：旧实现每收到一条日志就调用 size() 重算整批字节数，
// 一批 n 条累计 O(n²) 次遍历，默认阈值（4KB ≈ 100 条）下每批白走近 5000 圈。
type batchEntries struct {
	entries []logEntry
	bytes   int
}

// entryHeaderBytes 时间、级别、括号、路径与行号的估算头部
const entryHeaderBytes = 64

func (b *batchEntries) add(entry logEntry) {
	b.entries = append(b.entries, entry)
	b.bytes += len(entry.msg) + entryHeaderBytes
}

// reset 清空批次。
//
// clear 逐槽置零是必须的：entries[:0] 只把长度归零，底层数组依旧攥着这一批的
// msg / attrs / context，而容器来自 sync.Pool，被钉住的整批字符串最长要拖到下一
// 批填满才可能释放。
func (b *batchEntries) reset() {
	clear(b.entries)
	b.entries = b.entries[:0]
	b.bytes = 0
}

// levelString 日志级别的展示文本，与历史输出逐字一致。
func levelString(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// appendEntry 把一条日志按既有格式追加到 buf 尾部并返回扩容后的切片。
// 输出与旧的 formatEntry 完全一致，只是不再为每条日志单独分配字符串。
//
// 做成自由函数而不是通道方法：通道不在时（SetLevel 换绑窗口、异步关闭之后仍有
// 零星日志）管理器要往同一个文件句柄上同步落一条，格式必须逐字相同，否则同一个
// .log 里会出现两种时间戳样式，按格式解析的工具直接废掉。
func appendEntry(buf []byte, entry logEntry) []byte {
	buf = entry.time.AppendFormat(buf, time.DateTime)
	buf = append(buf, ' ')
	buf = append(buf, levelString(entry.level)...)
	buf = append(buf, " ["...)
	if entry.file != "" {
		buf = append(buf, entry.file...)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(entry.line), 10)
		buf = append(buf, ' ')
	}
	buf = append(buf, entry.msg...)
	buf = append(buf, ']')

	for _, attr := range entry.attrs {
		buf = append(buf, ' ')
		buf = append(buf, attr.Key...)
		buf = append(buf, '=')
		buf = fmt.Appendf(buf, "%v", attr.Value.Any())
	}

	return append(buf, '\n')
}
