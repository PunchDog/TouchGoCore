package gin

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 根劫持 + 手动分发。
//
// Run() 不再向 gin 路由树注册任何具体路由，只挂一个 NoRoute 根 handler：
// 因为没有任何路由命中，所有请求（任意方法、任意路径）都会进入这里，
// 由 handler 按请求路径查 routerMap 手动分发。这样既「劫持所有类型 HTTP 协议」，
// 又彻底规避 catch-all 通配与 /static、/wst 等首段的路由树冲突。
//
// 分发顺序：
//  1. /static 前缀 + 配置了静态目录 -> 交 http.FileServer 服务；
//  2. /ws 前缀 -> 静默 404（不查表、不打业务 warning；ws 实际跑在独立端口）；
//  3. routerMap 按「路径 + 方法」精确命中（精确方法缺失时回退通配条目）-> 调用该路由 handler；
//  4. 精确未命中时逐段匹配参数路由（含 ":name" 段的注册键走这里）：段数相等、
//     字面段全等、":name" 段捕获非空值并回填 c.Params，处理器内 ctx.Param("name") 可读；
//  5. 路径存在（字面或参数模式）但请求方法未注册 -> 405 + Allow 头；
//  6. 路径完全未注册 -> 404 + vars.Debug（扫描器流量不刷 Warning）。
//
// 含参数段的注册键不进 routerMap（具体请求路径永远撞不上字面键），
// 单独存 paramRoutes；精确匹配仍是第一优先，字面路由零额外开销。
//
// 局限（A-F9，审查后维持）：参数路由命中优先级 = 注册序，不是 gin 原生的
// 特异性优先；详见 newRootHandler 注释与 paramRoutes 声明处说明。
// ============================================================================

const (
	staticPrefix = "/static"
	wsPrefix     = "/ws"
	// methodAny 是 routerMap 二级 key 的通配方法：RouterType 未声明时注册到此键，
	// 任意请求方法都可命中。
	methodAny = ""
)

// routeEntry 分发条目：
//   - fn 非 nil：本地注册的 handler（RegisterRouter 口径），方法维度由 routerMap 二级 key 承载。
//   - proxy 非 nil：该路由由远端 gRPC 客户端提供（fn 为 nil），命中时经 proxy 转发；
//     owner 为注册来源客户端名，session 为该客户端本次连接的会话代际 id
//     （rpc 侧传入，gin 不感知其含义、只做等值比较），断连时按 owner+session
//     双键回收——同名重连后旧会话的延迟回收不得抹掉新会话刚注册的条目。
type routeEntry struct {
	fn      func(*gin.Context)
	proxy   GRPCProxyStream
	owner   string
	session uint64
}

// paramRoute 含 ":name" 参数段的注册键：segs 为注册期预切的模式段，
// byMethod 与 routerMap 内层同构（精确方法优先，缺失回退通配键 methodAny）。
type paramRoute struct {
	key      string // 注册键字面（含 ":name" 原样），供 MatchedRoute 回查
	segs     []string
	byMethod map[string]*routeEntry
}

// matchedRouteKey 是命中路由的注册模板在 gin 上下文中的存放键。根劫持下
// c.FullPath() 恒为空（没有任何 gin 路由被命中），而下游中间件（验签、
// 限流、审计）需要「这条请求撞的是哪条注册路由」——框架知道答案，
// 就必须在分发时替他记下来，而不是让每个下游各自反推 URL。
const matchedRouteKey = "touchgo:matched_route"

// MatchedRoute 返回本次请求命中的注册模板路径（含 ":name" 段的原样返回，
// 字面路由即路径本身）；未命中任何路由时返回空串。
func MatchedRoute(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(matchedRouteKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

var (
	// paramRoutes 按注册序扫描匹配；同请求命中多条模式时先注册者胜，
	// 更具体的模式应先注册。注意（A-F9 既定局限）：这是「优先级=注册序」而非
	// gin 原生的特异性优先；gRPC 反向代理下注册序等于客户端连接顺序，不可控，
	// 同段数的参数模式互相遮蔽时结果依赖注册先后，属分发器设计取舍，勿依赖。
	paramRoutes []*paramRoute
	paramIndex  = make(map[string]*paramRoute) // 注册键字面 -> 条目，重复注册去重
	// paramMu 保护上面两者：注册可能在 Run 之后动态发生，分发读表不持锁的
	// 老模式对切片不适用，这里用 RWMutex：服务侧并发读，注册侧短写。
	paramMu sync.RWMutex
)

// hasParamSegment 判断注册路径是否含 ":name" 参数段（段首冒号且后跟至少一个字符）。
func hasParamSegment(path string) bool {
	for _, s := range strings.Split(path, "/") {
		if len(s) > 1 && s[0] == ':' {
			return true
		}
	}
	return false
}

// match 逐段比对：字面段必须全等，":name" 段捕获非空单段；段数不等直接不命中。
func (pr *paramRoute) match(reqSegs []string) (gin.Params, bool) {
	if len(pr.segs) != len(reqSegs) {
		return nil, false
	}
	var ps gin.Params
	for i, seg := range pr.segs {
		if len(seg) > 1 && seg[0] == ':' {
			if reqSegs[i] == "" {
				return nil, false // 参数段不可为空（如尾斜杠 /seg/）
			}
			ps = append(ps, gin.Param{Key: seg[1:], Value: reqSegs[i]})
			continue
		}
		if seg != reqSegs[i] {
			return nil, false
		}
	}
	return ps, true
}

// lookupRoute 按「路径 + 方法」查注册表：精确方法优先，缺失时回退通配键。
// 必须持 routerMu 读锁：gRPC 客户端连接/断连会在运行期写 routerMap
// （RegisterGRPCRoute / UnregisterGRPCRoutes），无锁读与并发写会触发
// Go runtime 的 fatal concurrent map read/write（不可被 Recovery 捕获）。
// 返回的 *routeEntry 一旦写入即不可变（替换走整指针新建），锁外使用安全。
func lookupRoute(path, method string) *routeEntry {
	routerMu.RLock()
	defer routerMu.RUnlock()
	byMethod := routerMap[path]
	if byMethod == nil {
		return nil
	}
	if e, ok := byMethod[method]; ok {
		return e
	}
	return byMethod[methodAny]
}

// allowedMethods 收集一条请求路径上已注册的全部 HTTP 方法（字面表 + 能匹配该
// 路径的参数模式），供 405 响应的 Allow 头使用。返回 nil 表示路径根本未注册
// （应走 404 而非 405）。发现通配键（methodAny）时同样返回 nil：通配条目在
// lookupRoute/matchParamRoute 里必然已命中，走到这里说明只剩并发窗口内的
// 陈旧快照，按未注册处理避免误报 405。
func allowedMethods(p string) []string {
	seen := make(map[string]struct{})
	wildcard := false
	collect := func(byMethod map[string]*routeEntry) {
		for m := range byMethod {
			if m == methodAny {
				wildcard = true
				return
			}
			seen[m] = struct{}{}
		}
	}

	routerMu.RLock()
	if byMethod := routerMap[p]; byMethod != nil {
		collect(byMethod)
	}
	routerMu.RUnlock()

	// 两把锁分开持有，不嵌套：全仓没有 routerMu 与 paramMu 的嵌套加锁点，
	// 这里也不引入，避免锁序问题。
	if !wildcard {
		paramMu.RLock()
		var reqSegs []string
		for _, pr := range paramRoutes {
			if _, ok := pr.match(splitPathCached(&reqSegs, p)); !ok {
				continue
			}
			collect(pr.byMethod)
			if wildcard {
				break
			}
		}
		paramMu.RUnlock()
	}

	if wildcard || len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// splitPathCached 惰性切分请求路径（只在确有参数路由需要匹配时切一次）。
func splitPathCached(cache *[]string, p string) []string {
	if *cache == nil {
		*cache = strings.Split(p, "/")
	}
	return *cache
}

// matchRoute 完整分发匹配：先走 routerMap 精确查表（字面路由零额外开销），
// 未命中再扫描参数路由；命中时把捕获的 ":name" 段写进 c.Params，
// 处理器内 ctx.Param("id") 即可取值；同时把命中的注册模板记进上下文，
// 下游中间件用 MatchedRoute 取（根劫持下 c.FullPath() 恒空，这是唯一真源）。
func matchRoute(c *gin.Context) *routeEntry {
	p := c.Request.URL.Path
	if e := lookupRoute(p, c.Request.Method); e != nil {
		c.Set(matchedRouteKey, p)
		return e
	}
	return matchParamRoute(c, p)
}

// matchParamRoute 扫描参数路由。paramRoutes 数量极少（参数化注册是罕见能力），
// 读锁下线性扫描即够；命中才写 c.Params，未命中不污染上下文。
func matchParamRoute(c *gin.Context, p string) *routeEntry {
	paramMu.RLock()
	defer paramMu.RUnlock()
	if len(paramRoutes) == 0 {
		return nil
	}
	reqSegs := strings.Split(p, "/")
	for _, pr := range paramRoutes {
		// match 内部先比段数，不等即短路，无需外层预筛。
		ps, ok := pr.match(reqSegs)
		if !ok {
			continue
		}
		// 方法门控与精确查表同口径：精确方法优先，再通配。
		e, ok := pr.byMethod[c.Request.Method]
		if !ok {
			e = pr.byMethod[methodAny]
		}
		if e == nil {
			continue
		}
		c.Params = append(c.Params, ps...)
		c.Set(matchedRouteKey, pr.key)
		return e
	}
	return nil
}

// newRootHandler 组装根劫持 handler：静态目录 -> /ws 排除 -> 精确/参数路由分发 -> 405/404。
//
// 设计取舍（A-F8/A-F9，审查后维持的既定语义）：
//   - 尾斜杠重定向（gin 原生 RedirectTrailingSlash）**明确不恢复**：根劫持下没有任何
//     gin 路由树节点，自动重定向需要另建一套「±尾斜杠是否存在注册键」的探测，收益低、
//     且 301/307 语义与代理转发的方法保持问题纠缠，宁可对 /foo/ 与 /foo 视作两条路径。
//   - 参数路由优先级 = 注册序（paramRoutes 按注册先后线性扫描，先注册者胜），**不是**
//     gin 原生的「特异性优先」（静态段多于参数段者优先）。gRPC 反向代理下注册序等于
//     客户端连接顺序，不可控：/user/:id 与 /user/new 同时存在时，谁先注册谁吃掉请求。
//     运维上应避免同段数的参数模式与字面路径互相遮蔽（字面路径永远第一优先，见
//     matchRoute 的查表顺序，此点与 gin 一致）。
func newRootHandler(staticDir *string) gin.HandlerFunc {
	var fileServer http.Handler
	if staticDir != nil {
		fileServer = http.StripPrefix(staticPrefix+"/", http.FileServer(http.Dir(*staticDir)))
	}
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		// 1. 静态目录
		if fileServer != nil && (p == staticPrefix || strings.HasPrefix(p, staticPrefix+"/")) {
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		// 2. /ws：静默 404，不查表、不打业务 warning（ws 实际跑在独立端口）
		if p == wsPrefix || strings.HasPrefix(p, wsPrefix+"/") {
			c.Status(http.StatusNotFound)
			return
		}

		// 3. 精确分发，未命中再试参数路由（命中时已回填 c.Params）
		if e := matchRoute(c); e != nil {
			// 代理型条目：远端 gRPC 客户端提供的路由，经流转发并等待响应。
			if e.proxy != nil {
				serveGRPCProxy(c, e.proxy)
				return
			}
			vars.Debug("HTTP %s %s -> %v", c.Request.Method, p, e.fn)
			e.fn(c)
			return
		}
		// 4. 路径已注册但方法不匹配：405 + Allow（恢复 gin 原生 MethodNotAllowed 语义）。
		//    含通配条目时 allowedMethods 返回 nil（通配必然已在上面命中），不会误报 405。
		if allow := allowedMethods(p); len(allow) > 0 {
			c.Header("Allow", strings.Join(allow, ", "))
			vars.Debug("HTTP 405 方法未注册: %s %s (允许: %v)", c.Request.Method, p, allow)
			//如果 DefaultCallFunc 包含 CallGin，则调用 CallGin
			if util.DefaultCallFunc.Do(util.CallGin, c) {
				return
			}
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed", "code": 405})
			return
		}
		// 5. 路径完全未注册：404。日志用 Debug 级——未命中多来自外网扫描器，
		//    逐条 Warning 会把日志刷爆；排障时开 Debug 即可看到明细。
		vars.Debug("HTTP 404 未匹配路由: %s %s", c.Request.Method, p)
		c.JSON(http.StatusNotFound, gin.H{"error": "not found", "code": 404})
	}
}
