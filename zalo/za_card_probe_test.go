package zalo

// 本文件是 Zalo 卡片的真机探针：仅在显式提供环境变量时运行，缺省 Skip。
//
// 它会**真实发送一条消息**，因此必须指向一个可控的测试会话，不要在生产账号/群里跑。
// 用途：验证 fork 的显式卡片分支（跳过 ParseLink 后的 thumb/title/desc）是否被
// Zalo 服务端接受，以及 msgId 是否正常返回。
//
// 运行方式：
//
//	TEST_ZALO_PROBE_DSN='postgres://user:pass@127.0.0.1:5432/zalo?sslmode=disable' \
//	TEST_ZALO_PROBE_ACCOUNT='acct-id' \
//	TEST_ZALO_PROBE_TO='peer-uid' \
//	TEST_ZALO_PROBE_LINK='https://example.com/landing' \
//	TEST_ZALO_PROBE_TEXT=$'首行标题\n第二行描述' \
//	  go test -count=1 -v -run TestZACardProbeRealDevice ./zalo
//
// 群聊再加 TEST_ZALO_PROBE_GROUP=1。DSN 与会话材料不会打印。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"touchgocore/config"
)

func TestZACardProbeRealDevice(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_ZALO_PROBE_DSN"))
	account := strings.TrimSpace(os.Getenv("TEST_ZALO_PROBE_ACCOUNT"))
	to := strings.TrimSpace(os.Getenv("TEST_ZALO_PROBE_TO"))
	link := strings.TrimSpace(os.Getenv("TEST_ZALO_PROBE_LINK"))
	if dsn == "" || account == "" || to == "" || link == "" {
		t.Skip("真机探针未配置：需要 TEST_ZALO_PROBE_DSN/ACCOUNT/TO/LINK（会真实发送一条卡片）")
	}

	text := os.Getenv("TEST_ZALO_PROBE_TEXT")
	if strings.TrimSpace(text) == "" {
		text = "Zalo 卡片探针\n这是一条由 TouchGoCore 真机探针发送的卡片"
	}
	card := OutboundCard{
		Text:    text,
		LinkURL: link,
	}
	group := os.Getenv("TEST_ZALO_PROBE_GROUP") == "1"

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, err := newZAPGStore(ctx, dsn, zaSessionsTable)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sessions := &config.ZaloSessionsConfig{RequestTimeoutSec: 60}
	m := newZAManager(ctx, zaLimitsFromConfig(sessions), store, newZASDK(), realZAClock{}, nil)
	t.Cleanup(m.Stop)
	facInstall(t, m)

	var msgID string
	if group {
		msgID, err = ZaloSendGroupCard(ctx, account, to, card)
	} else {
		msgID, err = ZaloSendCard(ctx, account, to, card)
	}
	if err != nil {
		t.Fatalf("真机发送失败（group=%v）: %v", group, err)
	}
	if strings.TrimSpace(msgID) == "" {
		t.Fatalf("真机发送返回空 msgID（group=%v）", group)
	}
	t.Logf("真机探针通过: group=%v account=%s to=%s msgID=%s link=%s",
		group, account, to, msgID, link)
}
