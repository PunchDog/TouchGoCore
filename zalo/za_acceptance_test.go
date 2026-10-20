package zalo

// 本文件是独立验收补的覆盖缺口用例，只新增、不改既有测试与生产代码。
//
// 经对照覆盖矩阵与 go tool cover 实测，既有套件在以下四处只有「编译通过」而没有
// 行为断言（对应块在 cover profile 里是 0 次执行）：
//  1. config/example.json 的 zalo 段是否默认关闭：config 侧的示例闸只钉了
//     whatsapp.enable，把示例改成 zalo.enable=on 没有用例变红；
//  2. ensureConn 的写库失败路径（za_manager.go:694-699）：既有用例只覆盖
//     store.Get 失败，Upsert 失败是否安装 conn 没有断言；
//  3. cookie 导入的写库失败路径（za_manager.go:521-529）：同样只测了成功路径；
//  4. 库里凭证损坏（za_manager.go:668-671）：JSON 解析失败必须拒绝登录，
//     不能拿零值凭证去调 SDK。
//
// helper 一律 acc 前缀；fake 复用 za_manager_test.go 的定义，不修改该文件。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrakk/zcago"

	"touchgocore/config"
)

var (
	// errAccStoreBoom 是写库失败的注入错误，用于断言 errors.Is 能穿过安全包装。
	errAccStoreBoom = errors.New("acc store boom")
	// errAccSnapBoom 是凭证快照失败的注入错误。
	errAccSnapBoom = errors.New("acc snapshot boom")
)

// accReadExampleCfg 读取仓库随附的 config/example.json 并反序列化成 Cfg。
func accReadExampleCfg(t *testing.T) *config.Cfg {
	t.Helper()
	path := filepath.Join("..", "config", "example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return &cfg
}

// TestZAAcceptanceExampleJSONZaloDisabledByDefault 钉住示例配置不默认开启 Zalo：
// 非官方协议客户端默认不连 Zalo，示例若写成 on，等于教出一批照抄即上线的部署。
// 同时要求 sessions 段在示例里出现（否则用户不知道有这组限频键）。
func TestZAAcceptanceExampleJSONZaloDisabledByDefault(t *testing.T) {
	cfg := accReadExampleCfg(t)
	if cfg.Zalo == nil {
		t.Fatal(`example.json 缺少 "zalo" 配置块`)
	}
	if cfg.Zalo.Sessions == nil {
		t.Fatal(`example.json 缺少 "zalo"."sessions" 配置段：限频键没有可照抄的形态`)
	}
	if cfg.Zalo.Enabled() {
		t.Fatalf(`example.json 的 zalo.enable=%q：示例不得默认开启非官方协议客户端`, cfg.Zalo.Enable)
	}

	// 反证：同一份配置把 enable 改成 on 后判定必须翻转，证明上面的断言不是恒真。
	cfg.Zalo.Enable = "on"
	if !cfg.Zalo.Enabled() {
		t.Fatal("zalo.enable=on 时 Enabled() 仍为 false，示例断言失去判别力")
	}
}

// TestZAAcceptanceEnsureConnStoreFailureNoInstall 覆盖按需登录链路的失败不安装：
// 写库失败与凭证快照失败都必须返回错误、不安装 conn、不再写库。
// 既有 TestZAManagerEnsureConnStoreErrors 只覆盖 store.Get 失败，这两条是缺口。
func TestZAAcceptanceEnsureConnStoreFailureNoInstall(t *testing.T) {
	cases := []struct {
		name string
		// prep 在 seed 之后注入故障。
		prep     func(store *fakeZAStore, sdk *fakeZASDK)
		wantErr  error
		wantText string
		// wantUpserts 是本轮到达 store.Upsert 的次数：写库失败用例会尝试一次
		// （随后失败），快照失败用例一次都不该到达写库。
		wantUpserts int
	}{
		{
			name: "upsert_failure",
			prep: func(store *fakeZAStore, _ *fakeZASDK) {
				store.upsertErr = errAccStoreBoom
			},
			wantErr:     errAccStoreBoom,
			wantText:    "zalo 会话凭证写库失败",
			wantUpserts: 1,
		},
		{
			name: "snapshot_failure",
			prep: func(_ *fakeZAStore, sdk *fakeZASDK) {
				sdk.factory = func(zcago.Credentials) *fakeZAConn {
					return &fakeZAConn{uid: "uid-live", snapErr: errAccSnapBoom}
				}
			},
			wantErr:     errAccSnapBoom,
			wantText:    "zalo 会话快照失败，请重新扫码或导入",
			wantUpserts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeZAStore()
			store.seed("acct", "uid-seed", []byte(mgrTestCredJSON))
			sdk := newFakeZASDK()
			tc.prep(store, sdk)
			m := mgrNewTestManager(t, store, sdk, nil)

			acc, err := m.accountOrCreate("acct")
			if err != nil {
				t.Fatalf("accountOrCreate: %v", err)
			}
			conn, err := acc.ensureConn(context.Background())
			if conn != nil {
				t.Fatal("失败路径仍返回了 conn")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ensureConn err=%v，期望 errors.Is(%v)", err, tc.wantErr)
			}
			if err.Error() != tc.wantText {
				t.Fatalf("错误文案=%q，期望固定安全文案 %q（原始错误只进 Unwrap）", err.Error(), tc.wantText)
			}
			if acc.currentConn() != nil {
				t.Fatal("失败路径在账号上安装了 conn")
			}
			if got := store.upsertCount(); got != tc.wantUpserts {
				t.Fatalf("store.Upsert 到达 %d 次，期望 %d", got, tc.wantUpserts)
			}
			// 写库失败不得改写已有行：库里仍是 seed 的那份。
			if rec, ok := store.record("acct"); !ok || rec.UID != "uid-seed" {
				t.Fatalf("失败路径改写了会话行: %+v ok=%v", rec, ok)
			}
		})
	}
}

// TestZAAcceptanceLoginWithCookieUpsertFailureNoInstall 钉住 cookie 导入的写库语义：
// Upsert 失败按导入失败处理，不安装 conn、不把账号标成在线。
// 既有 TestZAManagerLoginWithCookie 只覆盖成功路径，这条是缺口。
func TestZAAcceptanceLoginWithCookieUpsertFailureNoInstall(t *testing.T) {
	store := newFakeZAStore()
	store.upsertErr = errAccStoreBoom
	sdk := newFakeZASDK()
	sdk.factory = func(zcago.Credentials) *fakeZAConn {
		return &fakeZAConn{uid: "uid-live", snap: zcago.Credentials{IMEI: "snap-imei", UserAgent: "snap-ua"}}
	}
	m := mgrNewTestManager(t, store, sdk, nil)

	raw := []byte(`[{"name":"zpw_sek","value":"not-a-real-token","domain":"zalo.me"}]`)
	info, err := m.LoginWithCookie(context.Background(), "acct", raw)
	if info != nil {
		t.Fatalf("写库失败仍返回了账号信息: %+v", info)
	}
	if !errors.Is(err, errAccStoreBoom) {
		t.Fatalf("LoginWithCookie err=%v，期望 errors.Is(%v)", err, errAccStoreBoom)
	}
	if err.Error() != "zalo 会话凭证写库失败" {
		t.Fatalf("错误文案=%q，期望固定安全文案", err.Error())
	}
	if got := sdk.loginCount(); got != 1 {
		t.Fatalf("登录调用 %d 次，期望 1", got)
	}
	if got := store.upsertCount(); got != 1 {
		t.Fatalf("写库调用 %d 次，期望 1", got)
	}
	if _, ok := store.record("acct"); ok {
		t.Fatal("写库失败却留下了会话行")
	}
	if acc, ok := m.account("acct"); ok && acc.currentConn() != nil {
		t.Fatal("写库失败仍安装了 conn")
	}
}

// TestZAAcceptanceEnsureConnCorruptStoredCredentials 覆盖库里的凭证 JSON 损坏：
// 必须拒绝登录（不打 SDK、不写库、不安装），而不是拿零值凭证继续。
func TestZAAcceptanceEnsureConnCorruptStoredCredentials(t *testing.T) {
	store := newFakeZAStore()
	// 截断的 JSON：既不是完整 Credentials，也不能被当成空凭证继续登录。
	store.seed("acct", "uid-seed", []byte(`{"imei":"MUST_NOT_LEAK"`))
	sdk := newFakeZASDK()
	m := mgrNewTestManager(t, store, sdk, nil)

	acc, err := m.accountOrCreate("acct")
	if err != nil {
		t.Fatalf("accountOrCreate: %v", err)
	}
	conn, err := acc.ensureConn(context.Background())
	if conn != nil {
		t.Fatal("凭证损坏仍返回了 conn")
	}
	if err == nil {
		t.Fatal("凭证损坏应返回错误")
	}
	if err.Error() != "zalo 会话凭证损坏，请重新扫码或导入" {
		t.Fatalf("错误文案=%q，期望固定安全文案", err.Error())
	}
	if got := sdk.loginCount(); got != 0 {
		t.Fatalf("凭证损坏仍调了 SDK 登录 %d 次", got)
	}
	if got := store.upsertCount(); got != 0 {
		t.Fatalf("凭证损坏仍写了 %d 次库", got)
	}
	if acc.currentConn() != nil {
		t.Fatal("凭证损坏仍安装了 conn")
	}
}
