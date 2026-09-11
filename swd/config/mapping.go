package config

import (
	"sync"
)

// MappingConfig 映射配置，存储所有字符映射表
type MappingConfig struct {
	mu sync.RWMutex

	// 全角转半角映射
	fullWidthToHalf map[rune]rune

	// 半角转全角映射
	halfToFullWidth map[rune]rune

	// 数字样式映射（如：①->1）
	numberStyle map[rune]rune

	// 拼音到汉字映射
	pinyin map[string][]string

	// 同音字映射
	homophone map[rune][]rune

	// 形近字映射
	similarShape map[rune][]rune
}

// NewMappingConfig 创建新的映射配置
func NewMappingConfig() *MappingConfig {
	return &MappingConfig{
		fullWidthToHalf: make(map[rune]rune),
		halfToFullWidth: make(map[rune]rune),
		numberStyle:     make(map[rune]rune),
		pinyin:          make(map[string][]string),
		homophone:       make(map[rune][]rune),
		similarShape:    make(map[rune][]rune),
	}
}

// GetFullWidthToHalf 获取全角转半角映射
func (c *MappingConfig) GetFullWidthToHalf() map[rune]rune {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fullWidthToHalf
}

// SetFullWidthToHalf 设置全角转半角映射
func (c *MappingConfig) SetFullWidthToHalf(mapping map[rune]rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fullWidthToHalf = mapping
}

// GetHalfToFullWidth 获取半角转全角映射
func (c *MappingConfig) GetHalfToFullWidth() map[rune]rune {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.halfToFullWidth
}

// SetHalfToFullWidth 设置半角转全角映射
func (c *MappingConfig) SetHalfToFullWidth(mapping map[rune]rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.halfToFullWidth = mapping
}

// GetNumberStyle 获取数字样式映射
func (c *MappingConfig) GetNumberStyle() map[rune]rune {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.numberStyle
}

// SetNumberStyle 设置数字样式映射
func (c *MappingConfig) SetNumberStyle(mapping map[rune]rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.numberStyle = mapping
}

// GetPinyin 获取拼音映射
func (c *MappingConfig) GetPinyin() map[string][]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pinyin
}

// SetPinyin 设置拼音映射
func (c *MappingConfig) SetPinyin(mapping map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinyin = mapping
}

// GetHomophone 获取同音字映射
func (c *MappingConfig) GetHomophone() map[rune][]rune {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.homophone
}

// SetHomophone 设置同音字映射
func (c *MappingConfig) SetHomophone(mapping map[rune][]rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.homophone = mapping
}

// GetSimilarShape 获取形近字映射
func (c *MappingConfig) GetSimilarShape() map[rune][]rune {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.similarShape
}

// SetSimilarShape 设置形近字映射
func (c *MappingConfig) SetSimilarShape(mapping map[rune][]rune) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.similarShape = mapping
}

var (
	globalMapping     *MappingConfig
	globalMappingOnce sync.Once
)

// GetGlobalMapping 获取全局映射配置实例（单例模式）
func GetGlobalMapping() *MappingConfig {
	globalMappingOnce.Do(func() {
		globalMapping = NewMappingConfig()
	})
	return globalMapping
}

// SetGlobalMapping 设置全局映射配置
func SetGlobalMapping(cfg *MappingConfig) {
	globalMapping = cfg
}
