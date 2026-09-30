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

// RegisterGRPCRoute 把一条远端客户端提供的路由写入分发表，与 RegisterRouter 完全同构：
//   - 含 ":name" 段的注册键走 paramRoutes（paramMu 保护），字面路径走 routerMap（routerMu 保护）；
//   - method 为空落到通配键 methodAny，任意请求方法都可命中；
//   - 重复注册（模板 + 方法两级都命中）按既有约定 vars.Error 并后者覆盖。
//
// clientName 为注册来源客户端名，供断连时 UnregisterGRPCRoutes 按属主回收。
func RegisterGRPCRoute(clientName, path, method string, stream GRPCProxyStream) {
	if path == "" || stream == nil {
		vars.Error("GRPC 路由注册参数非法: client=%s path=%q stream==nil:%v", clientName, path, stream == nil)
		return
	}
	entry := &routeEntry{proxy: stream, owner: clientName}

	if hasParamSegment(path) {
		paramMu.Lock()
		defer paramMu.Unlock()
		pr := paramIndex[path]
		if pr == nil {
			pr = &paramRoute{key: path, segs: strings.Split(path, "/"), byMethod: make(map[string]*routeEntry, 1)}
			paramIndex[path] = pr
			paramRoutes = append(paramRoutes, pr)
		}
		if _, dup := pr.byMethod[method]; dup {
			vars.Error("GRPC 参数路由 %s %q 重复注册（client=%s），本次将覆盖先前 handler", path, method, clientName)
		}
		pr.byMethod[method] = entry
		return
	}

	routerMu.Lock()
	defer routerMu.Unlock()
	byMethod := routerMap[path]
	if byMethod == nil {
		byMethod = make(map[string]*routeEntry, 1)
		routerMap[path] = byMethod
	}
	if _, dup := byMethod[method]; dup {
		vars.Error("GRPC 路由 %s %q 重复注册（client=%s），本次将覆盖先前 handler", path, method, clientName)
	}
	byMethod[method] = entry
}

// UnregisterGRPCRoutes 按属主回收断连客户端注册的所有路由（字面 + 参数两侧），
// 只摘 owner==clientName 的条目，不影响本地路由、其他客户端、以及同一 paramRoute 的其余方法。
func UnregisterGRPCRoutes(clientName string) {
	if clientName == "" {
		return
	}
	routerMu.Lock()
	for path, byMethod := range routerMap {
		for method, e := range byMethod {
			if e.owner == clientName {
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
				if e.owner == clientName {
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

// serveGRPCProxy 把命中的 HTTP 请求经 proxy 转发给远端客户端并回写网页端。
// 读请求体、组装 ProxyRequest、带默认超时调用 Do；失败/超时回 502。
func serveGRPCProxy(c *gin.Context, proxy GRPCProxyStream) {
	p := c.Request.URL.Path
	var body []byte
	if c.Request.Body != nil {
		defer c.Request.Body.Close()
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			vars.Error("GRPC 代理读取请求体失败 %s %s: %v", c.Request.Method, p, err)
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "bad gateway", "code": 502})
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
	for k, vs := range rsp.Header {
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
