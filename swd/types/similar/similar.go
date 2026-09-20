package similar

import (
	"touchgocore/swd/config"
)

// SetMappingConfig 设置全局映射配置（等价于 config.SetGlobalMapping，保留旧 API）
func SetMappingConfig(cfg *config.MappingConfig) {
	config.SetGlobalMapping(cfg)
}

// getMappingConfig 获取全局映射配置
func getMappingConfig() *config.MappingConfig {
	return config.GetGlobalMapping()
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
