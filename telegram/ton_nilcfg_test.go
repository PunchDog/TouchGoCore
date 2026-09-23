package telegram

import (
	"context"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

// TestTonStartNilCfgDoesNotPanic 没有 telegram 段、或段里没有 ton 引用时，
// TON 启动必须安静通过，且后续操作被拒而不是拿着空链路发请求。
func TestTonStartNilCfgDoesNotPanic(t *testing.T) {
	TonStop(nil)
	TonStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	TonStart(corectx.WithCfg(context.Background(), &config.Cfg{Telegram: &config.TelegramConfig{}}))
	// 传 nil 时回落到包级运行快照：上一条已经把「没有配置」写进快照，这里仍应不启动。
	TonStart(nil)
	// 引用指向一个没开的 SDK 段：同样是不启动，而不是启动一条发不出去请求的链路。
	closed := payTonCfg("https://ton.example", tonEndpoints())
	closed.PaySDks[tonSection].Enable = "off"
	TonStart(corectx.WithCfg(context.Background(), closed))
	if _, err := TonQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	if _, err := TonRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Fatal("未启动时充值应当被拒")
	}
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: tonAcctBounceable}); err == nil {
		t.Fatal("未启动时提现应当被拒")
	}
	if _, err := TonAccount(nil, nil); err == nil {
		t.Fatal("未启动时账户查询应当被拒")
	}
	TonStop(nil)
	TonStop(nil) // 幂等
}

// TestTonStopDoesNotAffectBot 资金链路的开关不碰 Bot：
// TelegramStart 与 TonStart 各自独立，Stop 一边不应把另一边摘掉。
func TestTonStopDoesNotAffectBot(t *testing.T) {
	startTon(t, payTonCfg("https://ton.example", tonEndpoints()))
	TonStop(nil)
	if _, err := TonQuery(nil, "O1"); err == nil {
		t.Fatal("停止后查询应当被拒")
	}
	if globalBot.Load() != nil {
		t.Fatal("TonStop 不该动 Bot（这里断言的是它没有把别的东西一起摘掉）")
	}
}

// TestTonFlagOffKeepsBotPathIntact SDK 段没开时，telegram 段其余字段的读取不受影响。
func TestTonFlagOffKeepsBotPathIntact(t *testing.T) {
	cfg := payTonCfg("https://ton.example", tonEndpoints())
	cfg.Telegram.BotToken = "123456:ABC"
	cfg.PaySDks[tonSection].Enable = "off"
	TonStart(corectx.WithCfg(context.Background(), cfg))
	if p := tonActive.Load(); p != nil {
		t.Fatal("SDK 段没开却启动了资金链路")
	}
	if got := telegramCfg(); got == nil || got.BotToken != "123456:ABC" {
		t.Fatalf("TonStart 改动了运行配置快照: %+v", got)
	}
}
