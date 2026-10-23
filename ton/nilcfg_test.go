package ton

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

// refuse 钉住资金门面在「链路未启动」时的 fail-closed 口径：明确报错，且结果必须为 nil。
//
// 两条都判是因为上游一旦只看「有没有报错」，一个非 nil 的半截回执就会被当成受理凭证——
// 而对出款来说，「被当成已受理」的下半场就是重复打款。
func refuse[T any](t *testing.T, name string, call func() (*T, error)) {
	t.Helper()
	res, err := call()
	if err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
		t.Errorf("%s 应报「TON 通道未启动」，实得 %v", name, err)
	}
	if res != nil {
		t.Errorf("%s 未启动却给出了结果对象: %+v", name, res)
	}
}

// refuseAll 让三个门面对同一份入参走一遍同一口径。
func refuseAll(t *testing.T, ctx context.Context, order *pay.PayOrder) {
	t.Helper()
	refuse(t, "TonWithdraw", func() (*pay.PayResult, error) { return TonWithdraw(ctx, order) })
	refuse(t, "TonQuery", func() (*pay.PayResult, error) { return TonQuery(ctx, order.OrderNo) })
	refuse(t, "TonAccount", func() (*pay.AccountInfo, error) { return TonAccount(ctx, nil) })
}

// TestPayoutNoCfgRefusesEveryFacade 没有 ton 段、连配置快照都没有时，三个门面一律被拒。
func TestPayoutNoCfgRefusesEveryFacade(t *testing.T) {
	prev := tonNativeRoute.runCtx()
	defer tonNativeRoute.setRunCtx(prev)

	order := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}
	for _, cfg := range []*config.Cfg{nil, {}, {Ton: &config.TonConfig{}}} {
		refuseAll(t, corectx.WithCfg(context.Background(), cfg), order)
	}
	// ctx 为 nil 时回落到包级运行上下文；这里没有启动过通道，仍然拒绝。
	refuseAll(t, nil, order)
}

// TestPayoutInterfaceStaysWired 三个门面仍是包外可见的调用面，且未启动时一律就地拒绝。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = TonWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = TonQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = TonAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: tonAcctBounceable}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := TonWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := TonQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := TonAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
			t.Errorf("%s 应报「TON 通道未启动」，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := TonWithdraw(nil, nil); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}

// TestTonStartWithoutConfigLeavesChannelDown 装配入口对空配置只留日志、不点亮通道。
func TestTonStartWithoutConfigLeavesChannelDown(t *testing.T) {
	prev := tonNativeRoute.runCtx()
	defer func() {
		tonNativeRoute.setRunCtx(prev)
		TonStop(context.Background())
	}()

	orders := []*pay.PayOrder{{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"无 ton 段", &config.Cfg{}},
		{"ton 段存在但没开", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{}}}},
		{"jetton 被填了", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Jetton: tonAcctBounceable}}}},
		{"network 填了 TonAPI 的自述名", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Network: "testnet-v4"}}}},
		{"开了但驱动名不对", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Driver: "generic_md5"}}}},
		{"开了但没接入点", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Driver: DriverTonAPI}}}},
	}
	for _, cs := range cases {
		TonStart(corectx.WithCfg(context.Background(), cs.cfg))
		if _, err := TonWithdraw(nil, orders[0]); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
			t.Errorf("%s：通道不该起来，实得 %v", cs.name, err)
		}
	}
}

// TestExampleConfigLeavesTonChannelDown 示例配置本身配不出「已启动」的 TON 通道。
func TestExampleConfigLeavesTonChannelDown(t *testing.T) {
	raw, err := os.ReadFile("../config/example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example.json 未通过 Validate: %v", err)
	}
	if cfg.Ton == nil || cfg.Ton.Native == nil {
		t.Fatal("示例缺 ton.native 段")
	}
	if ch, ok := newTONNativeClient(&cfg); ok || ch != nil {
		t.Fatal("示例配置装配出了 TON 通道：示例不该是「复制示例即出款」的形态")
	}
}
