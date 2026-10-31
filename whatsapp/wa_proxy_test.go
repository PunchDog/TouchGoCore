package whatsapp

// 全局出网代理的测试。全部走 seam：构造期用 waApplyClientProxy 挂点，运行期用
// fakeSession 的 SetProxy 实现，不联网、不碰真 whatsmeow transport。

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"

	"touchgocore/config"
)

// resetWAProxy 把全局代理状态与构造期挂点还原到测试前，避免用例互相污染。
func resetWAProxy(t *testing.T) {
	t.Helper()
	oldURL := waProxyURL.Load()
	oldHook := waApplyClientProxy
	t.Cleanup(func() {
		waProxyURL.Store(oldURL)
		waApplyClientProxy = oldHook
	})
	waProxyURL.Store(nil)
}

// installWAProxyTestManager 把管理器挂到包级快照上（WhatsappSetProxy 靠它找在跑会话）。
func installWAProxyTestManager(t *testing.T, m *waManager) {
	t.Helper()
	old := waMgr.Swap(m)
	t.Cleanup(func() { waMgr.Store(old) })
}

// SetProxy 是 waProxySetter 的假实现：记录地址；proxyErr 非 nil 时模拟库侧失败。
func (f *fakeSession) SetProxy(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proxyErr != nil {
		return f.proxyErr
	}
	f.proxyAddrs = append(f.proxyAddrs, addr)
	return nil
}

func (f *fakeSession) proxyLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.proxyAddrs...)
}

func TestWaNormalizeProxy(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		port    int
		want    string
		wantErr bool
	}{
		{"裸host带端口参数", "127.0.0.1", 7890, "http://127.0.0.1:7890", false},
		{"host:port无scheme", "127.0.0.1:8080", 0, "http://127.0.0.1:8080", false},
		{"裸host无端口", "proxy.local", 0, "http://proxy.local", false},
		{"https端口覆盖", "https://proxy.local", 443, "https://proxy.local:443", false},
		{"socks5带认证", "socks5://user:pass@proxy.local", 1080, "socks5://user:pass@proxy.local:1080", false},
		{"认证且端口覆盖", "http://user:pass@127.0.0.1:8080", 7890, "http://user:pass@127.0.0.1:7890", false},
		{"IPv6", "[::1]", 9000, "http://[::1]:9000", false},
		{"空串", "   ", 0, "", true},
		{"不支持的协议", "ftp://host:21", 0, "", true},
		{"URL端口越界", "http://host:70000", 0, "", true},
		{"参数端口越界", "host", 65536, "", true},
		{"参数端口负数", "host", -1, "", true},
		{"带path", "http://host:8080/x", 0, "", true},
		{"带查询串", "http://host:8080?x=1", 0, "", true},
		{"缺host", "http://", 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeWAProxy(tc.raw, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("规范化结果 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestWaProxyDisplayMasksPassword(t *testing.T) {
	got := waProxyDisplay("http://user:s3cret@127.0.0.1:8080")
	if strings.Contains(got, "s3cret") {
		t.Fatalf("展示串泄露了口令: %q", got)
	}
	if want := "http://user@127.0.0.1:8080（含认证）"; got != want {
		t.Fatalf("展示串 = %q，期望 %q", got, want)
	}
	if waProxyDisplay("") != "" {
		t.Fatalf("空代理的展示串应为空，得到 %q", waProxyDisplay(""))
	}
}

func TestWaProxyConstructorHookAppliesAddr(t *testing.T) {
	resetWAProxy(t)
	var got []string
	waApplyClientProxy = func(_ *whatsmeow.Client, addr string) error {
		got = append(got, addr)
		return nil
	}

	newWaConn(&store.Device{}, waLog.Noop)
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("未设置代理时应以空地址交给 hook（走环境变量），实际 %v", got)
	}

	addr, err := normalizeWAProxy("127.0.0.1", 7890)
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	waProxyURL.Store(&addr)
	newWaConn(&store.Device{}, waLog.Noop)
	newWaPairConn(&store.Device{}, waLog.Noop)
	if len(got) != 3 || got[1] != addr || got[2] != addr {
		t.Fatalf("发信/配对构造都该拿到 %q，实际 %v", addr, got)
	}
}

func TestWaProxyConstructorFailureBlocksLogin(t *testing.T) {
	resetWAProxy(t)
	boom := context.Canceled
	waApplyClientProxy = func(_ *whatsmeow.Client, _ string) error { return boom }

	sess := newWaConn(&store.Device{}, waLog.Noop)
	if err := sess.Login(context.Background()); err != boom {
		t.Fatalf("装代理失败时应拒绝连接并返回原错误，实际 %v", err)
	}

	pair := newWaPairConn(&store.Device{}, waLog.Noop)
	if err := pair.Connect(); err != boom {
		t.Fatalf("配对会话装代理失败时应拒绝连接，实际 %v", err)
	}
}

func TestWaProxySetReappliesRunningClients(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	waitUntil(t, 5*time.Second, "代理重放并重连", func() bool {
		lc, dc, logged, _, _ := f.snapshot()
		log := f.proxyLog()
		return len(log) == 1 && log[0] == "http://127.0.0.1:1080" && dc >= 1 && lc >= 2 && logged
	})
}

func TestWaProxySetFailureKeepsConnection(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, func(s *fakeSession) {
		s.proxyErr = context.Canceled
	})
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	lc, dc, logged, _, _ := f.snapshot()
	if dc != 0 || lc != 1 || !logged {
		t.Fatalf("单账号切代理失败时应保留旧连接（login=%d disconnect=%d logged=%v）", lc, dc, logged)
	}
}

func TestWaProxyClearRestoresEnvBehavior(t *testing.T) {
	resetWAProxy(t)
	dev := fakeDevice(t, "8613800000000@s.whatsapp.net", "android", "A")
	st := newFakeStore(dev)
	get := installFakeSessionFactory(t, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{ReconnectMinSec: 1, ReconnectMaxSec: 1})
	installWAProxyTestManager(t, m)

	if err := m.StartClient(*dev.ID); err != nil {
		t.Fatalf("启动会话失败: %v", err)
	}
	f := get(dev.ID.String())
	waitUntil(t, 2*time.Second, "会话登录", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		return lc >= 1 && logged
	})

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	waitUntil(t, 5*time.Second, "首次重放", func() bool {
		lc, _, _, _, _ := f.snapshot()
		return len(f.proxyLog()) == 1 && lc >= 2
	})

	if err := WhatsappClearProxy(); err != nil {
		t.Fatalf("清除代理失败: %v", err)
	}
	if got := WhatsappProxy(); got != "" {
		t.Fatalf("清除后 WhatsappProxy 应为空，实际 %q", got)
	}
	waitUntil(t, 5*time.Second, "清除后重放空地址并重连", func() bool {
		lc, _, logged, _, _ := f.snapshot()
		log := f.proxyLog()
		return len(log) == 2 && log[1] == "" && lc >= 3 && logged
	})
}

func TestWaProxyBeforeStartOK(t *testing.T) {
	resetWAProxy(t)
	old := waMgr.Swap(nil)
	t.Cleanup(func() { waMgr.Store(old) })

	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("未启动时设置代理应成功（留给之后构造的客户端）: %v", err)
	}
	if got := WhatsappProxy(); got != "http://127.0.0.1:1080" {
		t.Fatalf("WhatsappProxy = %q", got)
	}
	if err := WhatsappClearProxy(); err != nil {
		t.Fatalf("清除代理失败: %v", err)
	}
	if got := WhatsappProxy(); got != "" {
		t.Fatalf("清除后 WhatsappProxy 应为空，实际 %q", got)
	}
}

func TestWaProxyConfigDefaultOnlyWhenUnset(t *testing.T) {
	t.Run("配置装配", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "socks5://proxy.local"}); err != nil {
			t.Fatalf("配置装配失败: %v", err)
		}
		if got := waProxyAddr(); got != "socks5://proxy.local" {
			t.Fatalf("代理地址 = %q", got)
		}
	})
	t.Run("运行时优先于配置", func(t *testing.T) {
		resetWAProxy(t)
		if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
			t.Fatalf("设置代理失败: %v", err)
		}
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "http://other:1"}); err != nil {
			t.Fatalf("已有运行时值时配置装配应静默跳过: %v", err)
		}
		if got := waProxyAddr(); got != "http://127.0.0.1:1080" {
			t.Fatalf("配置覆盖了运行时值: %q", got)
		}
	})
	t.Run("非法配置报错", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{ProxyURL: "ftp://host:21"}); err == nil {
			t.Fatal("非法配置应报错（startWA 据此 fail-closed 不启动消息侧）")
		}
		if got := waProxyAddr(); got != "" {
			t.Fatalf("非法配置不应落状态，实际 %q", got)
		}
	})
	t.Run("空配置不动作", func(t *testing.T) {
		resetWAProxy(t)
		if err := installWAProxyDefault(&config.WhatsappSessionsConfig{}); err != nil {
			t.Fatalf("空配置应静默: %v", err)
		}
		if waProxyURL.Load() != nil {
			t.Fatal("空配置不应把状态从 nil（未设置）改成显式空串")
		}
	})
}

func TestWaProxyReapplySkipsPairing(t *testing.T) {
	resetWAProxy(t)
	st := newFakeStore()
	tr := installFakePairFactory(t, st, nil)
	m := newTestManager(t, st, &config.WhatsappSessionsConfig{})
	installWAProxyTestManager(t, m)

	if _, err := m.StartPairing(context.Background()); err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}
	ps := tr.last()
	if ps == nil {
		t.Fatal("配对会话未创建")
	}
	var sess waPairSession = ps
	if _, ok := sess.(waProxySetter); ok {
		t.Fatal("配对会话不应实现 waProxySetter：换代理重连会打断 QR 泵")
	}
	if err := WhatsappSetProxy("127.0.0.1", 1080); err != nil {
		t.Fatalf("设置代理失败: %v", err)
	}
	if got := ps.disconnectCount(); got != 0 {
		t.Fatalf("进行中的配对会话被切换代理断开了 %d 次", got)
	}
}
