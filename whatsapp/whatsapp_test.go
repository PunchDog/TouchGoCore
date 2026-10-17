package whatsapp

// 生命周期与资金门面存续的测试：enable 总闸不得放行消息侧装配，
// 三个资金门面仍是包外可见调用面且一律返回未接入哨兵、不广播回执。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
)

// ghostSessions 是一个形状合法但必然连不上的会话库 DSN（127.0.0.1:1 直接拒连）。
// 带上它是为了复现「配置齐备但没开闸」这一现网形态：闸失效时 startWA 会一路走到
// 开会话库那一步（随后因连不上而放弃），本用例钉的是这一步绝不能留下装配产物。
func ghostSessions() *config.WhatsappSessionsConfig {
	return &config.WhatsappSessionsConfig{DSN: "postgres://u:p@127.0.0.1:1/ghost?sslmode=disable"}
}

// TestChannelEnableGateBlocksStart whatsapp.enable 不为 on 时消息侧一律不装配：
// 不开会话库、不建管理器（因此绝不会去连 WhatsApp 服务器），门面全部 fail-closed。
// enable 缺失与显式 off 必须同一个结果——「忘了开」不该表现成「连上了但发不出去」。
//
// 口径变更（资金链路删除时留下的口子，写明白）：原来这圈用例的正向对照是
// 「enable 为 on 时 fund 非空」，那是唯一能证明「闸确实放行过」的可观测产物。
// 消息侧要装配成功必须连得上真 Postgres，测试里造不出来，所以正向对照退到
// 闸的判据本身（on 的大小写/空白形态），负向那半退到「没有装配产物 + 门面拒」。
func TestChannelEnableGateBlocksStart(t *testing.T) {
	off := []struct {
		name string
		wa   *config.WhatsappConfig
	}{
		{"没有 whatsapp 段", nil},
		{"enable 键没写", &config.WhatsappConfig{Sessions: ghostSessions()}},
		{"enable 为空串", &config.WhatsappConfig{Enable: "  ", Sessions: ghostSessions()}},
		{"enable 为 off", &config.WhatsappConfig{Enable: "off", Sessions: ghostSessions()}},
		{"enable 写了非 on 的值", &config.WhatsappConfig{Enable: "true", Sessions: ghostSessions()}},
	}
	for _, cs := range off {
		WhatsappStop(nil)
		WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{Whatsapp: cs.wa}))
		if m := waMgr.Load(); m != nil {
			t.Errorf("%s：消息侧管理器却装配了（enable 闸失效）", cs.name)
		}
		if s := waStore.Load(); s != nil {
			t.Errorf("%s：会话库却打开了（enable 闸失效）", cs.name)
		}
		if got := WhatsappListClients(); len(got) != 0 {
			t.Errorf("%s：未启动却有 %d 个在跑会话", cs.name, len(got))
		}
		if err := WhatsappStartClient("8613800000000@s.whatsapp.net"); err == nil {
			t.Errorf("%s：未启动时启动账号应当被拒", cs.name)
		}
		if _, err := WhatsappSendMessage(nil, "8613800000000", "8613900000000", "hi"); err == nil {
			t.Errorf("%s：未启动时发信应当被拒", cs.name)
		}
	}
	WhatsappStop(nil)

	for _, on := range []string{"on", "ON", "On", " on "} {
		wa := &config.WhatsappConfig{Enable: on}
		if !wa.Enabled() {
			t.Errorf("enable=%q 应当放行消息侧装配", on)
		}
		// 闸放行但 sessions 未配：消息侧仍按「没配 dsn 即不装配」独立判定，安静返回不 panic。
		WhatsappStop(nil)
		WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{Whatsapp: wa}))
		if waMgr.Load() != nil {
			t.Errorf("enable=%q 但 sessions 未配，消息侧不该装配", on)
		}
	}
	WhatsappStop(nil)
}

// TestPayoutInterfaceStaysWired 三个资金门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = WhatsappWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = WhatsappQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = WhatsappAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := WhatsappWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := WhatsappQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := WhatsappAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单与查询参数为 nil/空也一样先拒：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := WhatsappWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
	if _, err := WhatsappQuery(nil, ""); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空单号的拒绝口径不符: %v", err)
	}
}

// TestPayoutNotWiredDoesNotBroadcast 未接入的门面不得广播任何回执。
//
// 广播键（CallWhatsappMsg+Withdraw/Query）在下游挂着落库回调，
// 误广播一次就给账本记一笔不存在的资金动作。
func TestPayoutNotWiredDoesNotBroadcast(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*pay.PayResult
		kind []string
	)
	for _, k := range []string{"Withdraw", "Query"} {
		id := util.DefaultCallFunc.Register(util.CallWhatsappMsg+k, func(res *pay.PayResult) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, res)
			kind = append(kind, k)
		})
		defer util.DefaultCallFunc.Unregister(util.CallWhatsappMsg+k, id)
	}
	o := &pay.PayOrder{OrderNo: "O9", Amount: 1}
	_, _ = WhatsappWithdraw(nil, o)
	_, _ = WhatsappQuery(nil, "O9")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("未接入的链路广播了回执: kinds=%v %+v", kind, got)
	}
}
