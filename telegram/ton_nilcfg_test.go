package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// refuse 钉住资金门面在「链路未启动」时的 fail-closed 口径：明确报错，且结果必须为 nil。
//
// 两条都判是因为上游一旦只看「有没有报错」，一个非 nil 的半截回执就会被当成受理凭证——
// 而对出款来说，「被当成已受理」的下半场就是重复打款。
// 文案里点名的键名也要判：配置没开的人要靠这句话找到自己该改哪一格。
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

// TestPayoutNoCfgRefusesEveryFacade 没有 telegram 段、没有 ton 引用、连配置快照都没有时，
// 三个门面一律被拒，而不是 panic 或给出可被误读的回执。
//
// 原用例走的是 TonStart 的「静默不启动」路径；装配层删除后同一口径落在门面上，
// ctx 为 nil 的分支也一并钉住——它现在应当什么都不读。
func TestPayoutNoCfgRefusesEveryFacade(t *testing.T) {
	prev := runCtx()
	defer setRunCtx(prev)

	order := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}
	for _, cfg := range []*config.Cfg{nil, {}, {Telegram: &config.TelegramConfig{}}} {
		refuseAll(t, corectx.WithCfg(context.Background(), cfg), order)
	}
	refuseAll(t, nil, order)
}

// newLocalBot 造一个只对着本地假端点的 bot：凭证是假的，但 shutdownChannel 与
// TelegramStop 要走的那条清理路径是真的（零值 BotAPI 上 close 空 channel 会当场 panic）。
func newLocalBot(t *testing.T) *tgbotapi.BotAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
	}))
	t.Cleanup(srv.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("123456:ABC-DEF", srv.URL+"/bot%s/%s", srv.Client())
	if err != nil {
		t.Fatalf("本地假端点上的 bot 应当建起来: %v", err)
	}
	return bot
}

// TestPayoutCallsDoNotAffectBot 资金侧的调用不碰 Bot 侧运行时。
//
// 原用例钉的是「TonStop 只摘资金链路、不把 Bot 一起摘掉」；TonStop 已随实现删除，
// 同一保护改钉两面：门面既不摘 globalBot 也不取消在册轮询，而 Bot 自己的停止路径照样走得通。
func TestPayoutCallsDoNotAffectBot(t *testing.T) {
	bot := newLocalBot(t)
	roundCtx, roundCancel := context.WithCancel(context.Background())
	defer roundCancel()

	var wg sync.WaitGroup
	var cleaned atomic.Bool
	updates := make(chan tgbotapi.Update, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runUpdates(roundCtx, bot, updates, func() { cleaned.Store(true) })
	}()

	runTimeMu.Lock()
	prev := currentCancel
	globalBot.Store(bot)
	currentCancel = roundCancel
	runTimeMu.Unlock()
	defer func() {
		runTimeMu.Lock()
		currentCancel = prev
		globalBot.Store(nil)
		runTimeMu.Unlock()
	}()

	refuseAll(t, nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}})

	if got := globalBot.Load(); got != bot {
		t.Fatalf("资金门面调用把 Bot 摘掉了: got=%v", got)
	}
	select {
	case <-roundCtx.Done():
		t.Fatal("资金门面调用取消了 Bot 本轮轮询的上下文")
	default:
	}
	if cleaned.Load() {
		t.Fatal("资金门面调用触发了本轮轮询的清理")
	}

	// Bot 侧的停止链路仍按自己的语义走完：摘 Bot、取消本轮、跑清理。
	TelegramStop(context.Background())
	if !waitWithin(t, time.Second, "轮询退出", wg.Wait) {
		return
	}
	if !cleaned.Load() {
		t.Fatal("TelegramStop 没有执行本轮清理")
	}
	if globalBot.Load() != nil {
		t.Fatal("TelegramStop 后仍残留 bot")
	}
}

// TestPayoutFacadeKeepsBotConfigSnapshot 资金门面不动 Bot 侧读的那份运行配置快照。
//
// 原用例钉的是「SDK 段没开时 TonStart 不搅动 telegram 段快照」。TonStart 与 TelegramStart
// 共用同一个 telegramRunCtx，删掉前者之后这条保护仍要留着：门面若在 nil ctx 分支里
// 补一次 setRunCtx，就会把 Bot 的快照擦成 Background，而 Bot 侧下一轮读到的就是空配置。
func TestPayoutFacadeKeepsBotConfigSnapshot(t *testing.T) {
	prev := runCtx()
	defer setRunCtx(prev)

	setRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Telegram: &config.TelegramConfig{BotToken: "123456:ABC"},
	}))
	before := telegramRunCtx.Load()

	order := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}
	refuseAll(t, nil, order)
	refuseAll(t, context.Background(), order)

	if got := telegramRunCtx.Load(); got != before {
		t.Fatal("资金门面改写了 Bot 侧的运行上下文快照")
	}
	if got := telegramCfg(); got == nil || got.BotToken != "123456:ABC" {
		t.Fatalf("Bot 侧读到的配置快照被搅动: %+v", got)
	}
}

// TestPayoutInterfaceStaysWired 三个门面仍是包外可见的调用面，且未启动时一律就地拒绝。
//
// 签名钉子留着是因为服务注册与下游代码都按它写；这里钉的是另一半：链路没起来时
// 「没接入」绝不被读成「已受理」——结果必须是 nil 加一句指名配置键的错误。
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
//
// 这一条是新增的：TON 现在有真实实现，「配置没开即不能动钱」不再由哨兵白送，
// 必须有一条活闸证明 newTonClient 的每条回落分支都真的没把通道挂上去。
func TestTonStartWithoutConfigLeavesChannelDown(t *testing.T) {
	prev := runCtx()
	defer func() {
		setRunCtx(prev)
		TonStop(context.Background())
	}()

	orders := []*pay.PayOrder{{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"无 telegram 段", &config.Cfg{}},
		{"telegram 段没有 ton", &config.Cfg{Telegram: &config.TelegramConfig{BotToken: "1:x"}}},
		{"ton 段存在但没开", &config.Cfg{Telegram: &config.TelegramConfig{
			Ton: &config.TonChannelConfig{}}}},
		{"jetton 被填了", &config.Cfg{Telegram: &config.TelegramConfig{
			Ton: &config.TonChannelConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{Enable: "on"},
				Jetton:                tonAcctBounceable}}}},
		{"network 填了 TonAPI 的自述名", &config.Cfg{Telegram: &config.TelegramConfig{
			Ton: &config.TonChannelConfig{
				Network: "testnet-v4"}}}},
		{"开了但驱动名不对", &config.Cfg{Telegram: &config.TelegramConfig{
			Ton: &config.TonChannelConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{Enable: "on", Driver: "generic_md5"}}}}},
		{"开了但没接入点", &config.Cfg{Telegram: &config.TelegramConfig{
			Ton: &config.TonChannelConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{
					Enable: "on", Driver: DriverTonAPI,
					BaseURL: "https://pay.invalid",
					Accounts: map[string]*config.ChannelAccountConfig{
						"default": {Enable: "on", MerchantID: "MCH"},
					},
				}}}}},
	}
	for _, cs := range cases {
		TonStart(corectx.WithCfg(context.Background(), cs.cfg))
		if _, err := TonWithdraw(nil, orders[0]); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
			t.Errorf("%s：通道不该起来，实得 %v", cs.name, err)
		}
	}
}

// TestExampleConfigLeavesTonChannelDown 示例配置本身配不出「已启动」的 TON 通道。
//
// 示例是给下游抄的起点：它必须过 Validate（否则用户第一反应是改配置而不是怀疑示例），
// 但它的 enable 不是 on，所以装配必须停在 ok=false。
// 少了这条闸，示例里多一句 enable=on 就会被抄进一批会真出款的部署。
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
	if cfg.Telegram == nil || cfg.Telegram.Ton == nil {
		t.Fatal("示例缺 telegram.ton 段")
	}
	if ch, ok := newTonClient(&cfg); ok || ch != nil {
		t.Fatal("示例配置装配出了 TON 通道：示例不该是「复制示例即出款」的形态")
	}
}
