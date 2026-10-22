package zalo

// 本文件是 Zalo 包的顶层门面活闸：资金哨兵保持接线、未启动时 14 个公开 API 一律
// 拒绝（不 panic、不发请求）、ZaloLogout 只清 conn 不删库行、ZaloStart 在
// sessions 缺失时不安装 manager。
//
// 旧供应商登录链路的夹具（payCfg/recorder/签名域用例）随 auth.go/client.go 一起
// 删除；本文件不再引用 pay_sdks 或任何 HTTP 供应商配置。

import (
	"context"
	"errors"
	"testing"

	"github.com/amrakk/zcago"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

// TestPayoutInterfaceStaysWired 三个资金门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = ZaloWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = ZaloQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = ZaloAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, Currency: pay.CurrencyUSDT, To: &pay.Party{Address: "TTargetAddr"}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := ZaloWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := ZaloQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := ZaloAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := ZaloWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
	// 空订单号也一样：收口前后都是同一个哨兵，不给「参数校验错」留第二条出口。
	if _, err := ZaloQuery(nil, "  "); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单号的拒绝口径不符: %v", err)
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时 14 个公开 API 全部返回
// ErrZaloNotStarted，而不是 panic、空结果或 nil error。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	ZaloStop(nil)
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"start_pairing", func() error {
			_, err := ZaloStartPairing(ctx, "acct")
			return err
		}},
		{"login_with_cookie", func() error {
			_, err := ZaloLoginWithCookie(ctx, "acct", []byte("[]"))
			return err
		}},
		{"start_client", func() error {
			_, err := ZaloStartClient(ctx, "acct")
			return err
		}},
		{"logout", func() error { return ZaloLogout(ctx, "acct") }},
		{"delete_account", func() error { return ZaloDeleteAccount(ctx, "acct") }},
		{"list_accounts", func() error {
			_, err := ZaloListAccounts(ctx)
			return err
		}},
		{"send_message", func() error {
			_, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello")
			return err
		}},
		{"send_group_message", func() error {
			_, err := ZaloSendGroupMessage(ctx, "acct", "group-1", "hello")
			return err
		}},
		{"list_friends", func() error {
			_, err := ZaloListFriends(ctx, "acct")
			return err
		}},
		{"sync_friends", func() error {
			_, err := ZaloSyncFriends(ctx, "acct")
			return err
		}},
		{"list_groups", func() error {
			_, err := ZaloListGroups(ctx, "acct")
			return err
		}},
		{"sync_groups", func() error {
			_, err := ZaloSyncGroups(ctx, "acct")
			return err
		}},
		{"get_user_info", func() error {
			_, err := ZaloGetUserInfo(ctx, "acct", "u1")
			return err
		}},
		{"get_group_info", func() error {
			_, err := ZaloGetGroupInfo(ctx, "acct", "g1")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrZaloNotStarted) {
				t.Fatalf("未启动 err=%v，期望 ErrZaloNotStarted", err)
			}
		})
	}
}

// TestZaloLogoutClearsConnKeepsStore 钉住 ZaloLogout 的口径：停 listener、清内存
// conn，但不删会话库行（下次使用按需重新登录）。
func TestZaloLogoutClearsConnKeepsStore(t *testing.T) {
	store := newFakeZAStore()
	store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	zaMgr.Store(m)
	t.Cleanup(func() { zaMgr.Store(nil) })

	ctx := context.Background()
	if _, err := ZaloStartClient(ctx, "acct"); err != nil {
		t.Fatalf("ZaloStartClient: %v", err)
	}
	acc, ok := m.account("acct")
	if !ok {
		t.Fatal("ZaloStartClient 后账号不在内存")
	}
	if acc.currentConn() == nil {
		t.Fatal("ZaloStartClient 后没有 conn")
	}

	if err := ZaloLogout(ctx, "acct"); err != nil {
		t.Fatalf("ZaloLogout: %v", err)
	}
	if acc.currentConn() != nil {
		t.Fatal("ZaloLogout 后 conn 未清")
	}
	if _, ok := store.record("acct"); !ok {
		t.Fatal("ZaloLogout 不应删除会话库行")
	}
}

// TestZaloStartWithoutSessionsDoesNotInstallManager 覆盖「enable=on 但整段
// sessions 缺失」：只记日志，不打开会话库、不安装 manager。走 ctx 注入配置，
// 不触碰真实 Postgres。
func TestZaloStartWithoutSessionsDoesNotInstallManager(t *testing.T) {
	ZaloStop(nil)
	t.Cleanup(func() {
		ZaloStop(nil)
		setRunCtx(context.Background())
	})

	cfg := &config.Cfg{Zalo: &config.ZaloConfig{Enable: "on"}}
	ctx := corectx.WithCfg(context.Background(), cfg)
	setRunCtx(ctx)

	ZaloStart(ctx)
	if zaMgr.Load() != nil {
		t.Fatal("zalo.sessions 缺失时不应安装 manager")
	}
}
