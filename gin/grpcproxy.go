package gin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 反向 gRPC 路由代理：网关 gin 侧的中立契约与分发。
//
// 后端进程作为 gRPC 客户端连上网关后，把自身 HTTP 路由注册进网关的
// routerMap / paramRoutes；网页请求经根劫持命中这些「代理型」routeEntry 时，
// 由 routeEntry.proxy 把请求转发到远端客户端、等待响应并回写网页端。
//
// 本文件只依赖 net/http 与自定义接口，不 import rpc / network/message：
// 消息编解码与 gRPC 流交互全在 rpc 侧完成，gin 只认 GRPCProxyStream 抽象，
// 避免 gin→rpc→gin 的成环。
// ============================================================================

// ProxyRequest 是转发给远端 gRPC 客户端的 HTTP 请求快照。
// MatchedPattern 为服务端命中的注册模板（含 ":name" 原样，等价 MatchedRoute(c)）；
// Params 为服务端已捕获的路径参数，客户端据此直接回调、无需二次匹配。
type ProxyRequest struct {
	Method         string
	Path           string
	RawQuery       string
	MatchedPattern string
	Params         map[string]string
	Header         http.Header
	Body           []byte
}

// ProxyResponse 是远端客户端回传的 HTTP 响应。
type ProxyResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// GRPCProxyStream 把一条 HTTP 请求转发给远端 gRPC 客户端并阻塞等待响应。
// 实现方（rpc 侧）负责把请求编码进流、按 request_id 匹配响应；
// ctx 结束（超时/断连/取消）时必须尽快返回 error。
type GRPCProxyStream interface {
	Do(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error)
}

// defaultGRPCProxyTimeout 代理请求默认超时，与本地路由 buildRouteHandler 的 15s 同口径。
const defaultGRPCProxyTimeout = 15 * time.Second

// maxProxyRequestBody 代理请求体读取上限（A-F4）。
// 与 rpc 侧帧上限对齐：rpc/run.go 的 MAX_MSG_SIZE=10MB 限制的是整帧
// （body + headers + proto 编码开销），这里再留出 64KB 帧头余量，保证
// 读进来的 body 编码进 GinHTTPRequest 后必然不会超出 gRPC 发送上限。
// 超限直接回 413，不再 io.ReadAll 到内存（防未认证外网 POST 大 body 打爆网关内存）。
const maxProxyRequestBody = 10*1024*1024 - 64*1024

// RegisterGRPCRoute 把一条远端客户端提供的路由写入分发表，与 RegisterRouter 完全同构：
//   - 含 ":name" 段的注册键走 paramRoutes（paramMu 保护），字面路径走 routerMap（routerMu 保护）；
//   - method 为空落到通配键 methodAny，任意请求方法都可命中；
//   - sessionID 为注册来源会话的代际标识（rpc 侧传 clientSession.id，gin 只做等值比较），
//     记录进 routeEntry，供 UnregisterGRPCRoutes 按 owner+session 双键回收（A-F3）。
//
// 覆盖规则（A-F2）：同键（模板+方法）已存在条目时，只有 owner 相同（同一客户端
// 重注册/重连）才允许覆盖；owner 不同——包括被覆盖者是本地 fn 路由（owner==""）
// 或其他客户端的代理条目——一律拒绝注册并 vars.Error。否则任一已鉴权 RPC 客户端
// 都能劫持网关上的任意 HTTP 路由（所有客户端共享同一 token，被攻陷即接管 /pay/callback 类路径）。
//
// clientName 为注册来源客户端名，供断连时 UnregisterGRPCRoutes 按属主回收。
//
// 返回值 accepted：条目真正落库（含同属主覆盖）返回 true；参数非法、或同键
// 已被其他属主占用而被拒时返回 false。rpc 服务端据此只把 accepted==true 的条目
// 计入注册回执的成功数——被拒条目若也计数，客户端会误以为自己在服务该路由，
// 实际请求仍路由到旧属主（或 502），且无从自愈。
func RegisterGRPCRoute(clientName string, sessionID uint64, path, method string, stream GRPCProxyStream) bool {
	if path == "" || stream == nil {
		vars.Error("GRPC 路由注册参数非法: client=%s path=%q stream==nil:%v", clientName, path, stream == nil)
		return false
	}
	if clientName == "" {
		vars.Error("GRPC 路由注册被拒: client 名为空 path=%q method=%q（代理条目必须携带属主）", path, method)
		return false
	}
	entry := &routeEntry{proxy: stream, owner: clientName, session: sessionID}

	if hasParamSegment(path) {
		paramMu.Lock()
		defer paramMu.Unlock()
		pr := paramIndex[path]
		if pr == nil {
			pr = &paramRoute{key: path, segs: strings.Split(path, "/"), byMethod: make(map[string]*routeEntry, 1)}
			paramIndex[path] = pr
			paramRoutes = append(paramRoutes, pr)
		}
		if old, dup := pr.byMethod[method]; dup {
			if !sameProxyOwner(old, clientName) {
				vars.Error("GRPC 参数路由注册被拒: %s %q 已被 owner=%q 占用（session=%d），来源 client=%q session=%d 不得覆盖",
					path, method, old.owner, old.session, clientName, sessionID)
				return false
			}
			vars.Error("GRPC 参数路由 %s %q 同属主重复注册（client=%s session=%d），本次将覆盖先前 handler", path, method, clientName, sessionID)
		}
		pr.byMethod[method] = entry
		return true
	}

	routerMu.Lock()
	defer routerMu.Unlock()
	byMethod := routerMap[path]
	if byMethod == nil {
		byMethod = make(map[string]*routeEntry, 1)
		routerMap[path] = byMethod
	}
	if old, dup := byMethod[method]; dup {
		if !sameProxyOwner(old, clientName) {
			vars.Error("GRPC 路由注册被拒: %s %q 已被 owner=%q 占用（session=%d），来源 client=%q session=%d 不得覆盖",
				path, method, old.owner, old.session, clientName, sessionID)
			return false
		}
		vars.Error("GRPC 路由 %s %q 同属主重复注册（client=%s session=%d），本次将覆盖先前 handler", path, method, clientName, sessionID)
	}
	byMethod[method] = entry
	return true
}

// sameProxyOwner 判定既有条目能否被 clientName 覆盖：只有同属主的代理条目可覆盖。
// 本地 fn 路由 owner==""，任何代理注册都不得覆盖它。
func sameProxyOwner(old *routeEntry, clientName string) bool {
	return old != nil && old.proxy != nil && old.owner == clientName
}

// UnregisterGRPCRoutes 按「属主 + 会话代际」双键回收断连客户端注册的所有路由
// （字面 + 参数两侧），只摘 owner==clientName 且 session==sessionID 的条目。
// 双键是 A-F3 的修复核心：同名重连后，旧会话延迟执行的回收不得抹掉新会话
// （session 不同）刚注册的条目；也不影响本地路由、其他客户端、以及同一
// paramRoute 的其余方法。
func UnregisterGRPCRoutes(clientName string, sessionID uint64) {
	if clientName == "" {
		return
	}
	owned := func(e *routeEntry) bool {
		return e.owner == clientName && e.session == sessionID
	}
	routerMu.Lock()
	for path, byMethod := range routerMap {
		for method, e := range byMethod {
			if owned(e) {
				delete(byMethod, method)
			}
		}
		if len(byMethod) == 0 {
			delete(routerMap, path)
		}
	}
	routerMu.Unlock()

	paramMu.Lock()
	if len(paramRoutes) > 0 {
		kept := paramRoutes[:0]
		for _, pr := range paramRoutes {
			for method, e := range pr.byMethod {
				if owned(e) {
					delete(pr.byMethod, method)
				}
			}
			if len(pr.byMethod) == 0 {
				delete(paramIndex, pr.key)
				continue // 整个 paramRoute 已空，从扫描切片摘除
			}
			kept = append(kept, pr)
		}
		paramRoutes = kept
	}
	paramMu.Unlock()
}

// hopByHopHeaders 是 RFC 2616 §13.5.1 的逐跳头，只对单次传输连接有效，
// 代理不得透传（A-F5）；Content-Length 一并剔除——响应体已由网关重新成帧，
// 远端报的长度与实际字节数不再必然一致，留给 Go 自行计算。
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Content-Length":      {},
}

// serveGRPCProxy 把命中的 HTTP 请求经 proxy 转发给远端客户端并回写网页端。
// 读请求体（带上限，超限 413）、组装 ProxyRequest、带默认超时调用 Do；失败/超时回 502。
func serveGRPCProxy(c *gin.Context, proxy GRPCProxyStream) {
	p := c.Request.URL.Path
	var body []byte
	if c.Request.Body != nil {
		defer c.Request.Body.Close()
		// A-F4：限长读取。多读 1 字节用于区分「恰好等于上限」与「超限」。
		b, err := io.ReadAll(io.LimitReader(c.Request.Body, maxProxyRequestBody+1))
		if err != nil {
			vars.Error("GRPC 代理读取请求体失败 %s %s: %v", c.Request.Method, p, err)
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "bad gateway", "code": 502})
			return
		}
		if int64(len(b)) > maxProxyRequestBody {
			vars.Error("GRPC 代理请求体超限(>%d 字节) %s %s，已拒绝", maxProxyRequestBody, c.Request.Method, p)
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request entity too large", "code": 413})
			return
		}
		body = b
	}

	req := &ProxyRequest{
		Method:         c.Request.Method,
		Path:           p,
		RawQuery:       c.Request.URL.RawQuery,
		MatchedPattern: MatchedRoute(c),
		Params:         paramsToMap(c.Params),
		Header:         c.Request.Header,
		Body:           body,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultGRPCProxyTimeout)
	defer cancel()

	rsp, err := proxy.Do(ctx, req)
	if err != nil {
		vars.Error("GRPC 代理转发失败 %s %s: %v", c.Request.Method, p, err)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "bad gateway", "code": 502})
		return
	}
	if rsp == nil {
		vars.Error("GRPC 代理返回空响应 %s %s", c.Request.Method, p)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "bad gateway", "code": 502})
		return
	}

	status := rsp.Status
	if status == 0 {
		status = http.StatusOK
	}
	// A-F5：status 钳制——只有合法的最终状态 [200,599] 才透传。1xx 是信息性
	// 响应（Go net/http 把 WriteHeader(1xx) 当 interim，101 无 hijack 属异常），
	// 不能作为最终状态放行；负数/超界值写进 WriteHeader 会产生畸形响应
	//（Go 会打 unknown status code 警告并按 200 处理）。远端客户端已被视为
	// 不可信来源，这里直接判为坏响应回 502。
	if status < 200 || status > 599 {
		vars.Error("GRPC 代理返回非法状态码 %d %s %s: status=%d，按 502 处理", rsp.Status, c.Request.Method, p, status)
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "bad gateway", "code": 502})
		return
	}
	// A-F5：剔除 hop-by-hop 头与 Content-Length，只透传端到端头。
	for k, vs := range rsp.Header {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(status)
	if len(rsp.Body) > 0 {
		if _, err := c.Writer.Write(rsp.Body); err != nil {
			vars.Error("GRPC 代理回写响应体失败 %s %s: %v", c.Request.Method, p, err)
		}
	}
}

// paramsToMap 把 gin 的路径参数切片转为 map（后写覆盖先写）。
func paramsToMap(ps gin.Params) map[string]string {
	if len(ps) == 0 {
		return nil
	}
	m := make(map[string]string, len(ps))
	for _, pp := range ps {
		m[pp.Key] = pp.Value
	}
	return m
}
