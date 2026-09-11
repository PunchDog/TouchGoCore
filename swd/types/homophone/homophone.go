package homophone

import (
	"sync"

	"touchgocore/go-swd/config"
)

var (
	// 全局配置映射
	mappingConfig *config.MappingConfig
	once          sync.Once
)

// SetMappingConfig 设置全局映射配置
func SetMappingConfig(cfg *config.MappingConfig) {
	mappingConfig = cfg
}

// getMappingConfig 获取全局映射配置（懒加载）
func getMappingConfig() *config.MappingConfig {
	once.Do(func() {
		if mappingConfig == nil {
			mappingConfig = config.NewMappingConfig()
		}
	})
	return mappingConfig
}

// GetHomophones 获取指定字符的同音字列表
func GetHomophones(char rune) []rune {
	cfg := getMappingConfig()
	homophoneMap := cfg.GetHomophone()
	if homophones, ok := homophoneMap[char]; ok {
		return homophones
	}
	return nil
}

// IsHomophone 检查两个字符是否是同音字
func IsHomophone(char1, char2 rune) bool {
	homophones := GetHomophones(char1)
	if homophones == nil {
		return false
	}
	for _, h := range homophones {
		if h == char2 {
			return true
		}
	}
	return false
}
