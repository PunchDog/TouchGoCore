package whatsapp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.mau.fi/whatsmeow"

	"touchgocore/config"
	"touchgocore/vars"
)

// 全局出网代理。
//
// 一个进程只有一条出口（用户裁决）：whatsmeow 的 SetProxyAddress 是「每客户端一份
// transport」，多出口要按账号配多个代理行，与本包「一个 whatsapp.sessions 段管全通道」
// 的结构冲突。运行时接口优先于配置文件的 proxy_url，后设的覆盖先设的。
//
// 三态刻意用 atomic.Pointer[string] 而不是 string：nil 表示「从未设置」（保留
// whatsmeow 默认，即读 HTTPS_PROXY/直连），指向 "" 表示「运行时显式清除」。
// 两者都不能用 cli.SetProxy(nil) 表达——库文档写明 nil 会关掉环境变量代理
// （WM/client.go:348-376），那会把「清除显式代理」变成「禁用环境代理」。
var waProxyURL atomic.Pointer[string]

// waProxyApplyMu 串行化「设置状态 + 重放全部在跑会话」：并发两次设置时
// 最终状态与最后一次调用一致，且不会出现两次重放交错。
//
// 读锁给 StartClient：构造客户端要原子地读到「当前代理」并赶在重放扫描之前
// 登记进 clients 表；否则「设置代理」与「同时启动账号」交错时，新账号可能拿着
// 旧 transport 且错过这一轮重放（直到下次断线才换出口）。
var waProxyApplyMu sync.RWMutex

// waApplyClientProxy 是「把代理装进新客户端」的挂点：默认真实现，测试整体替换、
// 用完还原（照 waSessionFactory 的范式）。
var waApplyClientProxy = applyWAProxy

// applyWAProxy 给一个 whatsmeow 客户端装上代理。
//
// addr 为空表示「没有显式代理」：这里必须回调 http.ProxyFromEnvironment 恢复默认
// 行为，而不是 SetProxy(nil)（后者会禁用环境变量代理，见文件头注释）。
func applyWAProxy(cli *whatsmeow.Client, addr string) error {
	if addr == "" {
		cli.SetProxy(http.ProxyFromEnvironment)
		return nil
	}
	// SetProxyAddress 支持 http/https/socks5，且必须在 Connect 之前调用才作用于
	// websocket（WM/client.go:317-344）；运行中的连接由调用方 Disconnect 后重连。
	return cli.SetProxyAddress(addr)
}

// normalizeWAProxy 把调用方给的 url+port 折成 SetProxyAddress 能吃的规范 URL。
//
// 规则：scheme 缺省按 http（whatsmeow 只认 http/https/socks5，本函数提前拒绝其它；
// 也让调用方可以只传 "127.0.0.1" + port）；port>0 覆盖 URL 里的端口，port<=0 用
// URL 自带端口；拒绝 path/query/fragment，避免把 "http://host:80/x" 这种拼接错误
// 静默当成有效代理。错误一律是常量文案：url.Parse 的错误会带上含 userinfo 的原串，
// 不能直接透出到日志。
func normalizeWAProxy(raw string, port int) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("代理地址为空（清除显式代理请用 WhatsappClearProxy）")
	}
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("代理端口非法: %d（取值 1-65535，0 表示沿用 URL 端口）", port)
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("代理地址不可解析（检查 host/端口/userinfo 格式）")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return "", fmt.Errorf("代理协议不支持: %q（只支持 http/https/socks5）", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", errors.New("代理地址缺少 host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("代理地址不应带 path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("代理地址不应带查询串或片段")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("代理端口非法（取值 1-65535）")
		}
	}
	if port > 0 {
		// JoinHostPort 会替 IPv6 补方括号；userinfo 原样保留在 u.User 上。
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	}
	return u.String(), nil
}

// waProxyDisplay 是代理的日志/展示形态：只给 scheme://host:port（用户名可留），
// 口令绝不回显。
func waProxyDisplay(addr string) string {
	if addr == "" {
		return ""
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "(代理地址不可解析)"
	}
	authed := u.User != nil
	if authed {
		if name := u.User.Username(); name != "" {
			u.User = url.User(name)
		} else {
			u.User = nil
		}
	}
	s := u.String()
	if authed {
		s += "（含认证）"
	}
	return s
}

// waProxyAddr 读当前显式代理；空串表示走环境变量/直连。
func waProxyAddr() string {
	if p := waProxyURL.Load(); p != nil {
		return *p
	}
	return ""
}

// WhatsappSetProxy 设置全局出网代理，供外部工程调用。
//
// proxyURL 可带 scheme（http/https/socks5，缺省按 http）与 userinfo；port>0 覆盖
// URL 里的端口，port<=0 用 URL 自带端口。设置后立即重连已在跑账号（返回时已发起
// 重连，不等登录完成）；在途发送会因断开失败，不在请求路径上调用。
// 空串报错：清除请用 WhatsappClearProxy（两条语义不同，不合并成一个空值）。
func WhatsappSetProxy(proxyURL string, port int) error {
	addr, err := normalizeWAProxy(proxyURL, port)
	if err != nil {
		return fmt.Errorf("whatsapp 设置代理失败: %w", err)
	}
	waProxyApplyMu.Lock()
	defer waProxyApplyMu.Unlock()
	waProxyURL.Store(&addr)
	vars.Info("WhatsApp 全局代理已设置: %s", waProxyDisplay(addr))
	if m := waMgr.Load(); m != nil {
		m.ReapplyProxy(addr)
	}
	return nil
}

// WhatsappClearProxy 清除显式代理，恢复环境变量/直连行为，并重连在跑账号。
// 可重复调用；清除后 startWA 不再用配置文件覆盖（运行时裁决优先）。
func WhatsappClearProxy() error {
	waProxyApplyMu.Lock()
	defer waProxyApplyMu.Unlock()
	empty := ""
	waProxyURL.Store(&empty)
	vars.Info("WhatsApp 全局代理已清除（恢复环境变量/直连）")
	if m := waMgr.Load(); m != nil {
		m.ReapplyProxy("")
	}
	return nil
}

// WhatsappProxy 返回当前显式代理的脱敏展示串（scheme://host:port，含认证时加注，
// 不回显口令）；空串表示走环境变量/直连。只读，供运维与外部工程对账。
func WhatsappProxy() string {
	return waProxyDisplay(waProxyAddr())
}

// waProxySetter 是「运行中切换代理」的能力面。刻意不进 waSession 接口：
// 配对会话（换代理要重连，会打断 QR 泵）与测试假件不该被迫实现，重放侧按需断言。
type waProxySetter interface{ SetProxy(string) error }

// installWAProxyDefault 在消息侧启动时装配配置文件的 proxy_url。
//
// 运行时已设置过（含显式清除）就完全不动：接口优先于配置是这条功能的对外契约，
// 进程内重读配置不能把外部工程刚设的代理覆盖掉。配置非法由调用方 fail-closed
// （消息侧不启动），否则「配了代理却静默直连」是最难查的部署事故。
func installWAProxyDefault(cfg *config.WhatsappSessionsConfig) error {
	if cfg == nil || waProxyURL.Load() != nil {
		return nil
	}
	raw := strings.TrimSpace(cfg.ProxyURL)
	if raw == "" {
		return nil
	}
	addr, err := normalizeWAProxy(raw, 0)
	if err != nil {
		return err
	}
	waProxyApplyMu.Lock()
	defer waProxyApplyMu.Unlock()
	if waProxyURL.Load() != nil {
		return nil
	}
	waProxyURL.Store(&addr)
	vars.Info("WhatsApp 全局代理已按配置装配: %s", waProxyDisplay(addr))
	return nil
}
