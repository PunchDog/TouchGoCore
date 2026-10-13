package whatsapp

// 本文件把 github.com/TA-rathnayaka/whatsmeow-antiban 的防封闸接进发送路径。
//
// 口径三条：
//   - 一账号一闸：闸的状态（warm-up 进度、已知会话集、限速窗口）就是这只号码的
//     行为画像，跨账号共用等于互相污染。状态文件按账号 JID 落在
//     whatsapp.sessions.antiban_state_dir 下，重启由 antiban.Load 续档。
//   - 开了闸，同账号发送就是单行道：BeforeSend→睡眠→真发→AfterSend 整段持
//     per-account 互斥。库的 GroupRate 子门没有自带锁，且 BeforeSend/AfterSend
//     跨两次调用不成原子序列（README 的用法就是单线程顺序流）；并发穿透会写出
//     错乱账。防封的本意就是别并发轰炸，排队是特性不是代价。
//   - 闸拦下的发送以错误形态返回（错误串含「防封闸拦截」），不静默丢弃、不排队
//     重试——业务方拿到 ErrRateLimited 该改期就改期，那是它的调度问题。
//
// 没开（显式配 antiban_enabled=false，或整段 sessions 缺失）时本文件零作用：
// gate 为 nil，发送路径与不引这个依赖时逐字节一致。键缺省读作「开」。

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	antiban "github.com/TA-rathnayaka/whatsmeow-antiban"

	"touchgocore/config"
)

// waSendGate 是发送路径对防封闸的最小期望面（未导出 seam）。
// 方法签名逐字照抄 *antiban.Gate（查自 antiban/gate.go:111-245），测试整体替换。
type waSendGate interface {
	// IsKnownChat 在 BeforeSend 之前问：收件人是否已在闸的已知会话集里
	// （AfterSend 的 wasNewChat 必须与 BeforeSend 时刻的判定一致，库注释要求调用方自取）。
	IsKnownChat(jid string) bool
	BeforeSend(recipient, text string, isGroupOrNewsletter bool, sourceGroup string, now time.Time) (int, error)
	AfterSend(recipient, text string, isGroupOrNewsletter bool, sourceGroup string, wasNewChat bool, now time.Time)
	AfterSendFailed(now time.Time)
}

// waGateFactory 是「怎么给账号造闸」的挂点：默认真实现，测试整体替换（照 waSessionFactory 范式）。
var waGateFactory = newWaAntibanGate

// newWaAntibanGate 装配真闸：先 Load 旧档再 NewGate，warm-up 进度跨重启续上。
func newWaAntibanGate(cfg *config.WhatsappSessionsConfig, accountJID string) waSendGate {
	path := waAntibanStatePath(cfg.EffectiveAntibanStateDir(), accountJID)
	restored, _ := antiban.Load(path)
	return antiban.NewGate(antiban.GateConfig{
		Preset:        antiban.Resolve(cfg.EffectiveAntibanPreset()),
		Enabled:       true,
		StateFilePath: path,
		Restored:      restored,
		Now:           time.Now(),
	})
}

// waAntibanStatePath 把账号 JID 洗成安全文件名：JID 里的 ":" "@" 在 Windows 上
// 是非法/危险字符，逐字替换成 "_"，不同账号不会再撞同一个文件。
func waAntibanStatePath(dir, accountJID string) string {
	var b strings.Builder
	b.Grow(len(accountJID))
	for _, r := range accountJID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s.antiban.json", b.String()))
}

// 编译期钉住真闸满足 seam。
var _ waSendGate = (*antiban.Gate)(nil)
