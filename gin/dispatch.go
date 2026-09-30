package gin

import (
	"net/http"
	"strings"
	"sync"
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
//  5. 其余（未注册路径 / 方法既无精确也无通配条目）-> 404 + vars.Warning。
//
// 含参数段的注册键不进 routerMap（具体请求路径永远撞不上字面键），
// 单独存 paramRoutes；精确匹配仍是第一优先，字面路由零额外开销。
// ============================================================================

const (
	staticPrefix = "/static"
	wsPrefix     = "/ws"
	// methodAny 是 routerMap 二级 key 的通配方法：RouterType 未声明时注册到此键，
	// 任意请求方法都可命中。
	methodAny = ""
)

// routeEntry 分发条目：fn 为已构建好的 handler，方法维度信息由 routerMap 的二级 key 承载。
type routeEntry struct {
	fn func(*gin.Context)
}

// paramRoute 含 ":name" 参数段的注册键：segs 为注册期预切的模式段，
// byMethod 与 routerMap 内层同构（精确方法优先，缺失回退通配键 methodAny）。
type paramRoute struct {
	segs     []string
	byMethod map[string]*routeEntry
}

var (
	// paramRoutes 按注册序扫描匹配；同请求命中多条模式时先注册者胜，
	// 更具体的模式应先注册。
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
// 读表不持锁：键存在即条目已完整写入（routerMu 下整指针替换），
// 与改造前直读 routerMap 的模式同一风险水平。
func lookupRoute(path, method string) *routeEntry {
	byMethod := routerMap[path]
	if byMethod == nil {
		return nil
	}
	if e, ok := byMethod[method]; ok {
		return e
	}
	return byMethod[methodAny]
}

// matchRoute 完整分发匹配：先走 routerMap 精确查表（字面路由零额外开销），
// 未命中再扫描参数路由；命中时把捕获的 ":name" 段写进 c.Params，
// 处理器内 ctx.Param("id") 即可取值。
func matchRoute(c *gin.Context) *routeEntry {
	p := c.Request.URL.Path
	if e := lookupRoute(p, c.Request.Method); e != nil {
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
		return e
	}
	return nil
}

// newRootHandler 组装根劫持 handler：静态目录 -> /ws 排除 -> 精确/参数路由分发 -> 404+warning。
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
			vars.Debug("HTTP %s %s -> %v", c.Request.Method, p, e.fn)
			e.fn(c)
			return
		}
		// 4. 未命中 / 方法未注册：404 + warning
		vars.Warning("HTTP 404 未匹配路由: %s %s", c.Request.Method, p)
		c.JSON(http.StatusNotFound, gin.H{"error": "not found", "code": 404})
	}
}
