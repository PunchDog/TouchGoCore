package pinyin

import (
	"strings"
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
