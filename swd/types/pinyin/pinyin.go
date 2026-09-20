package pinyin

import (
	"strings"

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

// GetCharsFromPinyin 根据拼音获取对应的汉字列表
func GetCharsFromPinyin(pinyinStr string) []string {
	cfg := getMappingConfig()
	pinyinMap := cfg.GetPinyin()
	if chars, ok := pinyinMap[strings.ToLower(pinyinStr)]; ok {
		return chars
	}
	return nil
}

// ContainsPinyin 检查文本中是否包含指定的拼音
func ContainsPinyin(text, pinyinStr string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(pinyinStr))
}
