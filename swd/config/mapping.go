package config

import (
	"sync"

	"touchgocore/swd/config/mappingdata"
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

// NewMappingConfig 创建空的映射配置
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

// NewDefaultMappingConfig 使用内嵌数据表创建映射配置
func NewDefaultMappingConfig() *MappingConfig {
	cfg := NewMappingConfig()
	cfg.UseTables(mappingdata.Default())
	return cfg
}

// UseTables 用解析好的映射表覆盖全部内容
func (c *MappingConfig) UseTables(tables *mappingdata.Tables) {
	reverse := make(map[rune]rune, len(tables.FullWidthToHalf))
	for full, half := range tables.FullWidthToHalf {
		reverse[half] = full
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.fullWidthToHalf = cloneRuneMap(tables.FullWidthToHalf)
	c.halfToFullWidth = reverse
	c.numberStyle = cloneRuneMap(tables.NumberStyle)
	c.homophone = cloneRuneSliceMap(tables.Homophone)
	c.similarShape = cloneRuneSliceMap(tables.SimilarShape)

	c.pinyin = make(map[string][]string, len(tables.Pinyin))
	for key, chars := range tables.Pinyin {
		c.pinyin[key] = append([]string(nil), chars...)
	}
}

func cloneRuneMap(src map[rune]rune) map[rune]rune {
	dst := make(map[rune]rune, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneRuneSliceMap(src map[rune][]rune) map[rune][]rune {
	dst := make(map[rune][]rune, len(src))
	for k, v := range src {
		dst[k] = append([]rune(nil), v...)
	}
	return dst
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
	globalMu      sync.RWMutex
	globalMapping *MappingConfig
)

// GetGlobalMapping 获取全局映射配置实例。
// 首次访问时用内嵌数据表懒加载出默认配置，因此未显式 Set 也能拿到可用映射。
func GetGlobalMapping() *MappingConfig {
	globalMu.RLock()
	cfg := globalMapping
	globalMu.RUnlock()
	if cfg != nil {
		return cfg
	}

	globalMu.Lock()
	defer globalMu.Unlock()
	if globalMapping == nil {
		globalMapping = NewDefaultMappingConfig()
	}
	return globalMapping
}

// SetGlobalMapping 设置全局映射配置。
func SetGlobalMapping(cfg *MappingConfig) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalMapping = cfg
}
