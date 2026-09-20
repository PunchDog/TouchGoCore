package telegram

import (
	"context"
	"testing"
	"touchgocore/config"
)

// tg==nil 时旧实现先解引用 &tg.BotToken 再判空 → 必然 panic
func TestTelegramStart_NilConfigNoPanic(t *testing.T) {
	old := config.Cfg_
	config.Cfg_ = nil
	defer func() { config.Cfg_ = old }()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("TelegramStart 在无配置时 panic: %v", r)
		}
	}()
	TelegramStart(context.Background())
	if globalBot.Load() != nil {
		t.Fatal("无配置不应创建 bot")
	}
}
