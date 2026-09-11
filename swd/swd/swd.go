package swd

import (
	"context"

	"touchgocore/go-swd/config"
	"touchgocore/go-swd/types/category"

	"touchgocore/go-swd/core"
)

// ComponentFactory 定义了创建各种组件的工厂接口
type ComponentFactory interface {
	CreateDetector(options *core.SWDOptions) core.Detector
	CreateDetectorWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) core.Detector
	CreateFilter(detector core.Detector) core.Filter
	CreateLoader() core.Loader
	CreateComponents(options *core.SWDOptions) (core.Detector, core.Filter, core.Loader)
	CreateComponentsWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) (core.Detector, core.Filter, core.Loader)
}

// SWD 敏感词检测与过滤引擎的实现
type SWD struct {
	detector core.Detector
	filter   core.Filter
	loader   core.Loader
	options  *core.SWDOptions
}

// New 创建一个敏感词检测引擎
func New(factory ComponentFactory) (*SWD, error) {
	if factory == nil {
		return nil, ErrNoFactory
	}

	options := &core.SWDOptions{}

	// 使用工厂的CreateComponents方法创建并关联组件
	detector, filter, loader := factory.CreateComponents(options)

	if detector == nil {
		return nil, ErrNoDetector
	}
	if filter == nil {
		return nil, ErrNoFilter
	}
	if loader == nil {
		return nil, ErrNoLoader
	}

	return &SWD{
		detector: detector,
		filter:   filter,
		loader:   loader,
		options:  options,
	}, nil
}

// LoadDefaultWords 加载默认词库
func (swd *SWD) LoadDefaultWords(ctx context.Context) error {
	return swd.loader.LoadDefaultWords(ctx)
}

// LoadCustomWords 加载自定义词库
func (swd *SWD) LoadCustomWords(ctx context.Context, words map[string]category.Category) error {
	return swd.loader.LoadCustomWords(ctx, words)
}

// AddWord 添加单个敏感词
func (swd *SWD) AddWord(word string, category category.Category) error {
	return swd.loader.AddWord(word, category)
}

// AddWords 批量添加敏感词
func (swd *SWD) AddWords(words map[string]category.Category) error {
	return swd.loader.AddWords(words)
}

// RemoveWord 移除敏感词
func (swd *SWD) RemoveWord(word string) error {
	return swd.loader.RemoveWord(word)
}

// RemoveWords 批量移除敏感词
func (swd *SWD) RemoveWords(words []string) error {
	return swd.loader.RemoveWords(words)
}

// Clear 清空所有敏感词
func (swd *SWD) Clear() error {
	return swd.loader.Clear()
}

// Detect 检查文本是否包含敏感词
func (swd *SWD) Detect(text string) bool {
	return swd.detector.Detect(text)
}

// DetectIn 检查文本是否包含指定分类的敏感词
func (swd *SWD) DetectIn(text string, categories ...category.Category) bool {
	return swd.detector.DetectIn(text, categories...)
}

// Match 返回文本中找到的第一个敏感词
func (swd *SWD) Match(text string) *core.SensitiveWord {
	if text == "" {
		return nil
	}
	return swd.detector.Match(text)
}

// MatchIn 返回文本中第一个指定分类的敏感词
func (swd *SWD) MatchIn(text string, categories ...category.Category) *core.SensitiveWord {
	return swd.detector.MatchIn(text, categories...)
}

// MatchAll 返回文本中所有敏感词
func (swd *SWD) MatchAll(text string) []core.SensitiveWord {
	return swd.detector.MatchAll(text)
}

// MatchAllIn 返回文本中所有指定分类的敏感词
func (swd *SWD) MatchAllIn(text string, categories ...category.Category) []core.SensitiveWord {
	return swd.detector.MatchAllIn(text, categories...)
}

// EnableSecondaryAlgorithm 启用次要算法（默认 Trie）
// 可以在需要时调用以启用 Trie 算法进行特定场景的优化
func (swd *SWD) EnableSecondaryAlgorithm() error {
	return swd.detector.EnableSecondaryAlgorithm()
}

// DisableSecondaryAlgorithm 禁用次要算法并释放内存
func (swd *SWD) DisableSecondaryAlgorithm() {
	swd.detector.DisableSecondaryAlgorithm()
}

// Replace 使用指定的替换字符替换敏感词
func (swd *SWD) Replace(text string, replacement rune) string {
	return swd.filter.Replace(text, replacement)
}

// ReplaceIn 使用指定的替换字符替换指定分类的敏感词
func (swd *SWD) ReplaceIn(text string, replacement rune, categories ...category.Category) string {
	return swd.filter.ReplaceIn(text, replacement, categories...)
}

// ReplaceWithAsterisk 使用星号替换敏感词
func (swd *SWD) ReplaceWithAsterisk(text string) string {
	return swd.filter.ReplaceWithAsterisk(text)
}

// ReplaceWithAsteriskIn 使用星号替换指定分类的敏感词
func (swd *SWD) ReplaceWithAsteriskIn(text string, categories ...category.Category) string {
	return swd.filter.ReplaceWithAsteriskIn(text, categories...)
}

// ReplaceWithStrategy 使用自定义策略替换敏感词
func (swd *SWD) ReplaceWithStrategy(text string, strategy func(word core.SensitiveWord) string) string {
	return swd.filter.ReplaceWithStrategy(text, strategy)
}

// ReplaceWithStrategyIn 使用自定义策略替换指定分类的敏感词
func (swd *SWD) ReplaceWithStrategyIn(text string, strategy func(word core.SensitiveWord) string, categories ...category.Category) string {
	return swd.filter.ReplaceWithStrategyIn(text, strategy, categories...)
}

// RegisterObserver 注册状态观察者
func (swd *SWD) RegisterObserver(observer core.Observer) {
	if sm, ok := swd.loader.(core.StateManager); ok {
		sm.RegisterObserver(observer)
	}
}

// RemoveObserver 移除状态观察者
func (swd *SWD) RemoveObserver(observer core.Observer) {
	if sm, ok := swd.loader.(core.StateManager); ok {
		sm.RemoveObserver(observer)
	}
}

// NotifyObservers 通知所有观察者
func (swd *SWD) NotifyObservers() {
	if sm, ok := swd.loader.(core.StateManager); ok {
		sm.NotifyObservers()
	}
}

// GetWords 获取所有已加载的敏感词
func (swd *SWD) GetWords() map[string]category.Category {
	return swd.loader.GetWords()
}

// UpdateAlgoCache 更新算法选择缓存
func (swd *SWD) UpdateAlgoCache() {
	if swd.detector != nil {
		swd.detector.UpdateAlgoCache()
	}
}
