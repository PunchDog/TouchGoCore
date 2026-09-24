// Package dbmap 提供跨 db/* 子包共享的全局连接注册表 + App 级覆盖机制。
//
// db 包和 db/mysql、db/redis、db/mongo 等子包都依赖它来共享已建立的 socket 连接，
// 避免每次 NewClient/NewRedis/NewMongoDB 都重新拨号。
//
// 命名约定：未指定 App 注册表时，所有读/写走 Global；
// 调 UseAppRegistry 绑定后，Get 优先查 appRegistry，否则 fallback Global。
package dbmap

import "touchgocore/syncmap"

// Global 是全局唯一的连接/客户端注册表，键为连接标识（DSN / poolKey / URL），
// 值为对应驱动客户端（*sql.DB、*gorm.DB、*redis.Client、*mongo.Client 等）。
//
// 任何子包都可安全地 Load / Store / Delete 该 map。
var Global = syncmap.NewAny()

// appRegistry 由 App 绑定的注册表；Get 优先读它。
var appRegistry *syncmap.MapAny

// UseAppRegistry 绑定 App 的命名数据库表。不替换全局表，Get 先查 App 再 fallback。
func UseAppRegistry(m *syncmap.MapAny) {
	appRegistry = m
}

// Registry 返回当前优先写入的表（App 已绑定则返回 App 表）。
func Registry() *syncmap.MapAny {
	if appRegistry != nil {
		return appRegistry
	}
	return Global
}

// Get 从注册表读取值（App 优先，全局 fallback）。
func Get(key any) (any, bool) {
	if appRegistry != nil {
		if v, ok := appRegistry.Load(key); ok {
			return v, true
		}
	}
	return Global.Load(key)
}

// Store 写入注册表（App 优先，全局 fallback）。
func Store(key, value any) {
	if appRegistry != nil {
		appRegistry.Store(key, value)
		return
	}
	Global.Store(key, value)
}

// Delete 从注册表删除值（App + 全局）。
func Delete(key any) {
	if appRegistry != nil {
		appRegistry.Delete(key)
	}
	Global.Delete(key)
}
