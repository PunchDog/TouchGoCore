package gin

import (
	"net/http"
	"strings"
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
//  3. routerMap 精确命中且方法允许 -> 调用该路由 handler；
//  4. 其余（未注册路径 / 方法不在白名单）-> 404 + vars.Warning。
// ============================================================================

const (
	staticPrefix = "/static"
	wsPrefix     = "/ws"
)

// routeEntry 分发条目：fn 为已构建好的 handler，methods==nil 表示放行所有方法。
type routeEntry struct {
	fn      func(*gin.Context)
	methods map[string]bool
}

// buildDispatchIndex 把 routerMap 快照（key 形如 "/path" 或 "/path|GET&&POST"）
// 归一为「纯净路径 -> routeEntry」的只读索引，供根 handler O(1) 查询。
// 返回的 map 在服务器运行期只读，无需加锁。
func buildDispatchIndex(routes map[string]func(*gin.Context)) map[string]*routeEntry {
	idx := make(map[string]*routeEntry, len(routes))
	for key, fn := range routes {
		path, methods := splitRouteKey(key)
		idx[path] = &routeEntry{fn: fn, methods: methods}
	}
	return idx
}

// splitRouteKey 拆出纯净路径与方法白名单集合。无 "|" 后缀表示放行所有方法。
func splitRouteKey(key string) (string, map[string]bool) {
	i := strings.IndexByte(key, '|')
	if i < 0 {
		return key, nil // 无方法后缀 => Any
	}
	path := key[:i]
	ms := strings.Split(key[i+1:], "&&")
	set := make(map[string]bool, len(ms))
	for _, m := range ms {
		set[strings.TrimSpace(m)] = true
	}
	return path, set
}

// newRootHandler 组装根劫持 handler：静态目录 -> /ws 排除 -> routerMap 精确分发 -> 404+warning。
func newRootHandler(index map[string]*routeEntry, staticDir *string) gin.HandlerFunc {
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
		// 3. routerMap 精确分发
		if e, ok := index[p]; ok && (e.methods == nil || e.methods[c.Request.Method]) {
			vars.Debug("HTTP %s %s -> %v", c.Request.Method, p, e.fn)
			e.fn(c)
			return
		}
		// 4. 未命中 / 方法不允许：404 + warning
		vars.Warning("HTTP 404 未匹配路由: %s %s", c.Request.Method, p)
		c.JSON(http.StatusNotFound, gin.H{"error": "not found", "code": 404})
	}
}
