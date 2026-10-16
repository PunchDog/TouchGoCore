package whatsapp

// 本文件是「手机号配对码」门面：WhatsappGetPairingCode 借 QR 扫码配对
// 的同一套会话/扇出/清理机制（wa_pairing.go 的 startPairing + QRSession + runPairing）。
// PairPhone 在泵读到首个 "code" 项时就地调用；门面只等会话上的结果通道。
//
// 库事实（查自 whatsmeow pair-code.go，版本 v0.0.0-20261007111105-c386243a72ba）：
//   - 配对码是 PairPhone 的**返回值**（encodedLinkingCode[0:4] + "-" + [4:]，
//     8 位形态 XXXX-XXXX，pair-code.go:142），不是回调推给调用方的；
//   - 前置是「客户端已正常连接且 QR 通道首个项已出现」（pair-code.go:78-81），
//     所以必须等首个 "code" 项再调 PairPhone——取码发生在泵协程里，
//     门面同步等结果通道，不再另起 Subscribe 握手；
//   - 库自己会拒非法号码：去非数字后 len<=6 → ErrPhoneNumberTooShort
//     （pair-code.go:98-99），以 "0" 开头 → ErrPhoneNumberIsNotInternational
//     （pair-code.go:100-101）；本门面在联网前先拒，省一次建连与烧码；
//   - clientType 必须是 PairClient* 常量之一、displayName 必须形如
//     `Browser (OS)`，写错服务器回 400（pair-code.go:87-89）；本包固定
//     PairClientChrome + "Chrome (Windows)"（见下方包级 var）；
//   - 码配对完成后服务器推 pair-success → handlePair → Store.Save
//     （pair.go:226），与扫码配对同一落库点，既有裁决不变：配对成功
//     **不自动 StartClient**，只广播 JID 供业务方显式启动（泵协程既有语义）；
//   - 配对码与二维码共用同一条 socket 寿命（QR 轮换耗尽后库发 "timeout" 并
//     Disconnect，qrchan.go:77-83，约 160 秒），waQRTTL 的 5 分钟只是兜底。
//
// 事件出口不新开：pair_code 事件经同一个 QRSession.emit → publishWA("QR") 广播、
// 经 GET /whatsapp/qr/:session_id 的同一条 WebSocket 流出；外部仓（t1job 后台）
// 按 QREventPairCode 在既有流上分支。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

// QREventPairCode 是「配对码已取得」的事件名：码配对成功后经同一个 QRSession
// 广播 QREvent{Event: QREventPairCode, Code: <8位码>}。终局事件（success/
// timeout/error/err-*）仍用库原值，本包不改既有语义。
const QREventPairCode = "pair_code"

// 码配对的固定参数放包级 var：便于测试逐字断言，也便于后续按业务口径调整
// （displayName 受服务器白名单校验，改动前先看 pair-code.go:87-89 的注释）。
var (
	waPairCodeClientType           = whatsmeow.PairClientChrome
	waPairCodeClientDisplayName    = "Chrome (Windows)"
	waPairCodeShowPushNotification = true
	// waPairCodeHandshakeTimeout 是「Connect 之后等 QR 通道首个 code 项」的上限。
	// 到点没等到即判终局 error（库侧首个 QR 项正常在连接建立后秒级到达，10 秒
	// 已远超观测值）；测试注入短值断言超时清理路径。
	waPairCodeHandshakeTimeout = 10 * time.Second
)

// waPairCodeActive 是「同号码单飞」记账：键 = 归一后的数字串，值 = 会话 id。
// 握手未完成时占位拒绝第二呼；码已交给调用方后允许替换。会话终局时由 Done
// 钩子按会话 id 对账删除——失败路径若漏删，号码会被永久锁死。
var waPairCodeActive sync.Map

// waPairCodeMaxInflight 是码配对会话的在飞总量上限（T8 WA-PC-01）。
//
// 单飞只按号码去重，管不住「不同号码各来一条」：每发起一次就多一条泵协程、
// 一条 done 钩子协程和一次出到 WhatsApp 服务器的 WebSocket，约 160 秒内不回收。
// max_concurrent（wa_manager.go:161）只数已启动的账号会话，够不着配对会话这一支，
// 所以这里给一个包内硬上限兜住「调用方失控/被刷」的形态；业务侧限流仍归外部仓。
// 变量而非配置键：这不是运维可调项，是给框架自身的止损闸，测试注入小值断言拒绝路径。
var waPairCodeMaxInflight = 5

// waPairCodeClaimMu 把「数在飞 → 占号」串成单行道：不锁则并发突发能同时越过计数闸，
// 上限就成了建议值。锁内只做内存操作，不夹网络与建连动作。
var waPairCodeClaimMu sync.Mutex

// waPairCodeInflight 数在飞的码配对会话（记账条目即在册会话）。
func waPairCodeInflight() int {
	n := 0
	waPairCodeActive.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// waMaskedErr 是「文案消毒、分类保留」的错误：Error() 给出号码已掩码的文本，
// Unwrap() 仍交回原错误，errors.Is/As 的判定能力不受影响。
type waMaskedErr struct {
	inner error
	text  string
}

func (e *waMaskedErr) Error() string { return e.text }
func (e *waMaskedErr) Unwrap() error { return e.inner }

// waMaskErr 把错误串里出现的完整号码换成掩码形态再对外暴露。
//
// 非做不可的理由：库的 IQError 在 Code==0 时把整个 error/raw 节点回显进 Error()
// （errors.go:214-222），而 PairPhone 的请求节点带着明文 jid（pair-code.go:111）——
// 用 %w 原样上抛就等于把本包的掩码承诺交给服务器决定（T8 WA-PC-03）。
func waMaskErr(err error, digits, masked string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if digits != "" {
		text = strings.ReplaceAll(text, digits, masked)
	}
	return &waMaskedErr{inner: err, text: text}
}

// waMaskPhone 号码脱敏：长度 ≥7 时保留前 2 后 2、中间逐位以 * 填充，否则全 *。
// 门面自己写入的错误文案、日志与事件载荷一律走掩码。两处既有例外按裁决保留、
// 不在本批改动：配对成功事件的 JID 字段与本包 success 日志（wa_pairing.go:292）、
// 以及库侧日志（pair.go:155）——码配对成功后 JID 本就是该号码，业务方要靠它显式启动账号。
func waMaskPhone(digits string) string {
	if len(digits) >= 7 {
		return digits[:2] + strings.Repeat("*", len(digits)-4) + digits[len(digits)-2:]
	}
	return strings.Repeat("*", len(digits))
}

// WhatsappGetPairingCode 用手机号发起一次配对码配对，同步返回 8 位配对码与会话 id。
//
// 执行顺序（库前置钉死，测试逐字断言）：startPairing 内部 NewDevice → QRChannel →
// Connect → 泵读到首个 "code" 项立刻 PairPhone → 结果写入会话通道并 emit pair_code。
// 门面只等该通道（超 waPairCodeHandshakeTimeout 即终局 error）。会话交回既有泵跑到终局
// （用户在手机上输码后收到 success/timeout/error，与扫码同一条事件流）。
//
// 号码先过 WhatsappNormalizePhone；归一后 ≤6 位或以 '0' 开头 ⇒ 在建 Client 之前
// 就地拒绝（错误文案只含掩码）。消息侧未启动原样返回 errWaNotStarted，不联网。
// 同一号码握手未完成时拒绝重复发起；码已交给调用方后的下一次同号请求会取消旧会话
// 并换新码。失败终局后记账随会话清理删除。在飞码配对会话总数另有一个包内硬上限
// （waPairCodeMaxInflight），替换旧会话不占第二条名额。
// 配对成功不自动 StartClient——沿用扫码配对的既有裁决。
//
// 两条失败文案是分开的：调用方 ctx 结束报「配对码握手被取消」，等不到首个二维码项
// 报「配对码握手超时」；库带回来的错误串先经 waMaskErr 消毒，完整号码不随错误外抛。
func WhatsappGetPairingCode(ctx context.Context, phone string) (code, sessionID string, err error) {
	m, err := currentWA()
	if err != nil {
		return "", "", err
	}
	digits := WhatsappNormalizePhone(phone)
	masked := waMaskPhone(digits)
	// 联网前预校验，口径与库一致（pair-code.go:98-101）：省一次建连、省烧码。
	if len(digits) <= 6 {
		return "", "", fmt.Errorf("whatsapp 配对码失败: 号码过短（digits=%s）", masked)
	}
	if strings.HasPrefix(digits, "0") {
		return "", "", fmt.Errorf("whatsapp 配对码失败: 号码需为国际号码（不能以 0 开头）（digits=%s）", masked)
	}

	replace, err := waPairCodeClaim(m, digits, masked)
	if err != nil {
		return "", "", err
	}

	s, err := m.startPairing(digits)
	if err != nil {
		if replace == nil {
			waPairCodeActive.CompareAndDelete(digits, "")
		}
		return "", "", waMaskErr(fmt.Errorf("whatsapp 配对码失败: %w", err), digits, masked)
	}
	waPairCodeActive.Store(digits, s.ID)
	if replace != nil {
		replace.Cancel()
	}
	// 记账清理挂在会话 done 上：只在自己仍占着该号码时删除，避免替换后的新会话被旧钩子清掉。
	go func(id string) {
		<-s.Done()
		if v, ok := waPairCodeActive.Load(digits); ok {
			if sid, _ := v.(string); sid == id {
				waPairCodeActive.Delete(digits)
			}
		}
	}(s.ID)

	code, err = waWaitPairCode(ctxOrBackground(ctx), s, masked)
	if err != nil {
		return "", s.ID, err
	}
	return code, s.ID, nil
}

// waPairCodeClaim 占号。已出码的旧会话作为 replace 返回，调用方建好新会话后再 Cancel。
// 握手未完成的同号第二呼拒绝。替换不另占在飞名额。
func waPairCodeClaim(m *waManager, digits, masked string) (*QRSession, error) {
	waPairCodeClaimMu.Lock()
	defer waPairCodeClaimMu.Unlock()
	if prev, loaded := waPairCodeActive.Load(digits); loaded {
		prevID, _ := prev.(string)
		if prevID == "" {
			return nil, fmt.Errorf("whatsapp 配对码失败: 该号码已有进行中的配对码会话（digits=%s, session=%s）", masked, prevID)
		}
		if old, ok := m.LookupQRSession(prevID); ok {
			if !old.hasIssuedPairCode() {
				return nil, fmt.Errorf("whatsapp 配对码失败: 该号码已有进行中的配对码会话（digits=%s, session=%s）", masked, prevID)
			}
			return old, nil
		}
		return nil, nil
	}
	if n := waPairCodeInflight(); n >= waPairCodeMaxInflight {
		return nil, fmt.Errorf("whatsapp 配对码失败: 在飞配对码会话已达上限 %d（digits=%s），先等已有会话收场", n, masked)
	}
	waPairCodeActive.Store(digits, "")
	return nil, nil
}

// ctxOrBackground 门面惯例允许调用方传 nil ctx（与 StartPairing 同口径），
// 但 context.WithTimeout 对 nil 会 panic，握手段入口统一兜底。
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// waWaitPairCode 等泵把 PairPhone 结果写入会话通道。取码本身在 runPairing 里完成，
// 这里只约束调用方等待上限，并把超时/取消写成两条分开的文案。
func waWaitPairCode(ctx context.Context, s *QRSession, masked string) (string, error) {
	if s.pairResult == nil {
		return "", fmt.Errorf("whatsapp 配对码失败: 配对会话未持有一次性 Client（digits=%s）", masked)
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, waPairCodeHandshakeTimeout)
	defer cancel()

	select {
	case res, ok := <-s.pairResult:
		if !ok {
			err := fmt.Errorf("whatsapp 配对码失败: 配对会话在首个二维码项之前结束（digits=%s）", masked)
			s.emit(QREvent{Event: "error", Error: err.Error()})
			s.Cancel()
			return "", err
		}
		if res.err != nil {
			return "", res.err
		}
		return res.code, nil
	case <-handshakeCtx.Done():
		var err error
		if errors.Is(handshakeCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("whatsapp 配对码失败: 配对码握手超时（%s 内未出现首个二维码项，digits=%s）", waPairCodeHandshakeTimeout, masked)
		} else {
			err = fmt.Errorf("whatsapp 配对码失败: 配对码握手被取消（调用方 ctx 已结束，非 WhatsApp 侧故障，digits=%s）: %w", masked, handshakeCtx.Err())
		}
		s.completePairCode("", err)
		s.emit(QREvent{Event: "error", Error: err.Error()})
		s.Cancel()
		return "", err
	}
}
