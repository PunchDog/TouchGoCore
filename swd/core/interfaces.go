package core

import (
	"context"

	"touchgocore/go-swd/types/category"
)

// SensitiveWord 敏感词匹配结果
type SensitiveWord struct {
	Word     string
	StartPos int
	EndPos   int
	Category category.Category
}

// Observer 状态变更观察者接口
type Observer interface {
	// OnWordsChanged 词库变更时的回调
	OnWordsChanged(words map[string]category.Category)
}

// Detector 敏感词检测器
type Detector interface {
	// Detect 检查文本是否包含敏感词
	Detect(text string) bool

	// DetectIn 检查文本是否包含指定分类的敏感词
	DetectIn(text string, categories ...category.Category) bool

	// Match 返回文本中第一个敏感词
	Match(text string) *SensitiveWord

	// MatchIn 返回文本中第一个指定分类的敏感词
	MatchIn(text string, categories ...category.Category) *SensitiveWord

	// MatchAll 返回文本中所有敏感词
	MatchAll(text string) []SensitiveWord

	// MatchAllIn 返回文本中所有指定分类的敏感词
	MatchAllIn(text string, categories ...category.Category) []SensitiveWord

	// EnableSecondaryAlgorithm 启用次要算法（默认 Trie）
	// 可以在需要时调用以启用 Trie 算法进行特定场景的优化
	EnableSecondaryAlgorithm() error

	// DisableSecondaryAlgorithm 禁用次要算法并释放内存
	DisableSecondaryAlgorithm()

	// UpdateAlgoCache 更新算法选择缓存
	UpdateAlgoCache()
}

// Filter 敏感词过滤器
type Filter interface {
	// Replace 使用指定的替换字符替换敏感词
	Replace(text string, replacement rune) string

	// ReplaceIn 使用指定的替换字符替换指定分类的敏感词
	ReplaceIn(text string, replacement rune, categories ...category.Category) string

	// ReplaceWithAsterisk 使用 * 号替换敏感词
	ReplaceWithAsterisk(text string) string

	// ReplaceWithAsteriskIn 使用 * 号替换指定分类的敏感词
	ReplaceWithAsteriskIn(text string, categories ...category.Category) string

	// ReplaceWithStrategy 使用自定义替换策略替换敏感词
	ReplaceWithStrategy(text string, strategy func(word SensitiveWord) string) string

	// ReplaceWithStrategyIn 使用自定义替换策略替换指定分类的敏感词
	ReplaceWithStrategyIn(text string, strategy func(word SensitiveWord) string, categories ...category.Category) string
}

// WordManager 敏感词管理接口
type WordManager interface {
	// AddWord 添加单个敏感词
	AddWord(word string, category category.Category) error

	// AddWords 批量添加敏感词
	AddWords(words map[string]category.Category) error

	// RemoveWord 移除单个敏感词
	RemoveWord(word string) error

	// RemoveWords 批量移除敏感词
	RemoveWords(words []string) error

	// Clear 清空所有敏感词
	Clear() error
}

// Loader 敏感词加载接口
type Loader interface {
	WordManager
	// LoadDefaultWords 加载默认词库
	LoadDefaultWords(ctx context.Context) error

	// LoadCustomWords 加载自定义词库
	LoadCustomWords(ctx context.Context, words map[string]category.Category) error

	// GetWords 获取所有已加载的敏感词
	GetWords() map[string]category.Category
}

// StateManager 状态管理接口
type StateManager interface {
	// RegisterObserver 注册状态观察者
	RegisterObserver(observer Observer)

	// RemoveObserver 移除状态观察者
	RemoveObserver(observer Observer)

	// NotifyObservers 通知所有观察者
	NotifyObservers()
}

// SWD 主接口
type SWD interface {
	Detector
	Filter
	Loader
	StateManager

	// 配置选项方法
	WithOptions(options *SWDOptions) SWD
	WithSkipWhitespace(skip bool) SWD
	WithIgnoreCase(ignore bool) SWD
	WithIgnoreWidth(ignore bool) SWD
	WithMaxDistance(distance int) SWD
	WithIgnoreNumStyle(ignore bool) SWD

	// 功能启用/禁用方法
	EnablePinyin() SWD
	DisablePinyin() SWD
	EnableHomophone() SWD
	DisableHomophone() SWD
	EnableSimilarShape() SWD
	DisableSimilarShape() SWD
	EnableZhPYMix() SWD
	DisableZhPYMix() SWD
	EnableNumCheck() SWD
	DisableNumCheck() SWD
	EnableURLCheck() SWD
	DisableURLCheck() SWD
	EnableEmailCheck() SWD
	DisableEmailCheck() SWD

	// UpdateAlgoCache 更新算法选择缓存
	UpdateAlgoCache()
}

// SWDOptions 定义引擎的配置选项
type SWDOptions struct {
	IgnoreCase         bool // 忽略大小写
	IgnoreWidth        bool // 忽略全角和半角字符差异
	IgnoreNumStyle     bool // 忽略数字样式差异
	EnableNumCheck     bool // 启用对连续数字的检测
	EnableURLCheck     bool // 启用对 URL 的检测
	EnableEmailCheck   bool // 启用对 Email 的检测
	SkipWhitespace     bool // 忽略空白字符
	MaxDistance        int  // 字符间最大距离（防止 f*u*c*k）
	EnablePinyin       bool // 启用拼音检测
	EnableHomophone    bool // 启用同音字检测
	EnableSimilarShape bool // 启用形近字检测（如：幾/几）
	EnableVariantForm  bool // 启用异体字检测（如：门/門）
	EnableZhPYMix      bool // 启用中文拼音混合检测（如：fa票）
}
