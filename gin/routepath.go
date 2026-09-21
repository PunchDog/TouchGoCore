package gin

// IRouterPath 可选接口：实现它即可为方法指定显式路由路径，
// 覆盖默认的 /{小写类型名}/{小写方法名} 推导规则。
//
//	key   = Go 方法名（大小写与 reflect 返回的方法名完全一致）
//	value = 以 "/" 开头的完整路径（gin 语法，路径参数用 ":name"，
//	        如 /wst/api/m/mobile/sendMessage/:id）
//
// 未实现该接口、或某方法名未出现在 map 中时，一律回退到默认推导，
// 保证 proxywork 等现有调用方零改动、行为不变（纯增量、向后兼容）。
//
// 注意：RouterPath 与 RouterType 一样属于「类型方法」，RegisterRouter 会跳过它，
// 不会被注册成路由 handler。
type IRouterPath interface {
	RouterPath() map[string]string
}
