package homophone

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
