// Package swd 提供了敏感词检测和过滤功能
// 这是一个顶层包，导出了 pkg/swd 和其他相关包的类型和函数
package swd

import (
	"touchgocore/go-swd/config"
	"touchgocore/go-swd/core"
	pkgswd "touchgocore/go-swd/swd"
	"touchgocore/go-swd/types/category"
)

// 导出核心类型
type (
	// SensitiveWord 表示一个敏感词及其相关信息
	SensitiveWord = core.SensitiveWord
	// Category 表示敏感词的分类
	Category = category.Category
	// SWD 是敏感词检测引擎的主要实现
	SWD = core.SWD
	// SWDOptions 定义引擎的配置选项
	SWDOptions = core.SWDOptions
)

// 导出分类常量
const (
	None           = category.None           // 未分类
	Pornography    = category.Pornography    // 涉黄
	Political      = category.Political      // 涉政
	Violence       = category.Violence       // 暴力
	Gambling       = category.Gambling       // 赌博
	Drugs          = category.Drugs          // 毒品
	Profanity      = category.Profanity      // 脏话
	Discrimination = category.Discrimination // 歧视
	Scam           = category.Scam           // 诈骗
	Custom         = category.Custom         // 自定义
)

// All 所有预定义分类的组合
var All = category.All

// New 创建一个新的敏感词检测引擎
func New() (SWD, error) {
	return pkgswd.New(pkgswd.NewDefaultFactory())
}

// NewWithFactory 使用自定义工厂创建敏感词检测引擎
func NewWithFactory(factory pkgswd.ComponentFactory) (SWD, error) {
	return pkgswd.New(factory)
}

// NewWithMappingDir 使用指定映射目录创建敏感词检测引擎
func NewWithMappingDir(mappingDir string) (SWD, error) {
	return pkgswd.New(pkgswd.NewDefaultFactoryWithMappingDir(mappingDir))
}

// NewDefaultFactory 创建默认工厂实例
func NewDefaultFactory() pkgswd.ComponentFactory {
	return pkgswd.NewDefaultFactory()
}

// NewDefaultFactoryWithMappingDir 使用指定映射目录创建工厂
func NewDefaultFactoryWithMappingDir(mappingDir string) pkgswd.ComponentFactory {
	return pkgswd.NewDefaultFactoryWithMappingDir(mappingDir)
}

// GetGlobalMapping 获取全局映射配置实例
func GetGlobalMapping() *config.MappingConfig {
	return config.GetGlobalMapping()
}

// SetGlobalMapping 设置全局映射配置
func SetGlobalMapping(cfg *config.MappingConfig) {
	config.SetGlobalMapping(cfg)
}
