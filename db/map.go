package db

import (
	"touchgocore/syncmap"
)

// _DbMap 全局命名库表（fallback）。
var _DbMap *syncmap.MapAny = syncmap.NewAny()

// _dbApp 由 App 绑定的注册表；GetNamed 优先读它。
var _dbApp *syncmap.MapAny

// UseRegistry 绑定 App 的命名数据库表。不替换全局表，GetNamed 先查 App 再 fallback。
func UseRegistry(m *syncmap.MapAny) {
	_dbApp = m
}

// Registry 返回当前优先写入的表（App 已绑定则返回 App 表）。
func Registry() *syncmap.MapAny {
	if _dbApp != nil {
		return _dbApp
	}
	return _DbMap
}

func GetNamed(key any) (any, bool) {
	if _dbApp != nil {
		if v, ok := _dbApp.Load(key); ok {
			return v, true
		}
	}
	if _DbMap == nil {
		return nil, false
	}
	return _DbMap.Load(key)
}

func StoreNamed(key, value any) {
	if _dbApp != nil {
		_dbApp.Store(key, value)
		return
	}
	if _DbMap == nil {
		_DbMap = syncmap.NewAny()
	}
	_DbMap.Store(key, value)
}
