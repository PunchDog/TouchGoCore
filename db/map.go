package db

import (
	"touchgocore/syncmap"
)

// _DbMap 存储 GORM 数据库连接池实例
// key: 数据库连接字符串, value: *gorm.DB
var _DbMap *syncmap.Map = &syncmap.Map{}
