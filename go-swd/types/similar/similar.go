package similar

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

// GetSimilarChars 获取指定字符的形近字列表
func GetSimilarChars(char rune) []rune {
	cfg := getMappingConfig()
	similarMap := cfg.GetSimilarShape()
	if similar, ok := similarMap[char]; ok {
		return similar
	}
	return nil
}

// IsSimilar 检查两个字符是否是形近字
func IsSimilar(char1, char2 rune) bool {
	similarChars := GetSimilarChars(char1)
	if similarChars == nil {
		return false
	}
	for _, s := range similarChars {
		if s == char2 {
			return true
		}
	}
	return false
}
