package db

import (
	"touchgocore/syncmap"
)

// _DbMap 存储 GORM 数据库连接池实例
// key: 数据库连接字符串, value: *gorm.DB
var _DbMap *syncmap.MapAny = syncmap.NewAny()

// Registry 返回命名数据库实例表（优先由此读取，保留全局 fallback）。
func Registry() *syncmap.MapAny {
	return _DbMap
}

func GetNamed(key any) (any, bool) {
	if _DbMap == nil {
		return nil, false
	}
	return _DbMap.Load(key)
}

func StoreNamed(key, value any) {
	if _DbMap == nil {
		_DbMap = syncmap.NewAny()
	}
	_DbMap.Store(key, value)
}
