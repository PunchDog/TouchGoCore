package gin

// IRouterPath 可选接口：实现它即可为方法指定显式路由路径，
// 覆盖默认的 /{小写类型名}/{小写方法名} 推导规则。
//
//	key   = Go 方法名（大小写与 reflect 返回的方法名完全一致）
//	value = 以 "/" 开头的完整路径（如 /wst/api/m/mobile/sendMessage）。
//	        支持 gin 风格的 ":name" 单段参数（如 .../sendMessage/:id）：
//	        分发时先精确匹配字面路径，未命中再逐段匹配参数路由，
//	        ":name" 段捕获非空单段，处理器内用 ctx.Param("name") 取值；
//	        不支持 "*name" 尾段通配。RouterTimeout 的键仍只认字面路径，
//	        参数路由一律走默认 15s 超时。
//
// 未实现该接口、或某方法名未出现在 map 中时，一律回退到默认推导，
// 保证 proxywork 等现有调用方零改动、行为不变（纯增量、向后兼容）。
//
// 注意：RouterPath 与 RouterType 一样属于「类型方法」，RegisterRouter 会跳过它，
// 不会被注册成路由 handler。
type IRouterPath interface {
	RouterPath() map[string]string
}
