package preprocessor

import (
	"regexp"
	"sync"
	"unicode"

	"touchgocore/go-swd/common"
	"touchgocore/go-swd/config"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/homophone"
	"touchgocore/go-swd/types/pinyin"
	"touchgocore/go-swd/types/similar"
)

// 预编译正则表达式（包级别，只编译一次）
var (
	pinyinRegex     *regexp.Regexp
	pinyinRegexOnce sync.Once
)

// getPinyinRegex 获取预编译的拼音正则表达式
func getPinyinRegex() *regexp.Regexp {
	pinyinRegexOnce.Do(func() {
		pinyinRegex = regexp.MustCompile(`[a-z]+`)
	})
	return pinyinRegex
}

// Preprocessor 文本预处理器
type Preprocessor struct {
	options      *core.SWDOptions
	config       *config.MappingConfig
	processCache *common.LRUCache[string, []string] // LRU 缓存
}

// 预处理器常量
const (
	defaultCacheSize = 1000 // 默认缓存大小
)

// NewPreprocessor 创建新的预处理器实例
func NewPreprocessor(options *core.SWDOptions) *Preprocessor {
	return &Preprocessor{
		options:      options,
		config:       config.NewMappingConfig(),
		processCache: common.NewLRUCache[string, []string](defaultCacheSize),
	}
}

// NewPreprocessorWithConfig 使用自定义配置创建预处理器
func NewPreprocessorWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) *Preprocessor {
	return &Preprocessor{
		options:      options,
		config:       cfg,
		processCache: common.NewLRUCache[string, []string](defaultCacheSize),
	}
}

// SetConfig 设置映射配置
func (p *Preprocessor) SetConfig(cfg *config.MappingConfig) {
	p.config = cfg
}

// ProcessResult 处理结果，包含处理后的文本和位置映射
type ProcessResult struct {
	ProcessedText string
	// 位置映射：处理后的位置 -> 原始位置
	PositionMap []int
}

// Process 处理文本
func (p *Preprocessor) Process(text string) string {
	result := p.ProcessWithPositionMap(text)
	return result.ProcessedText
}

// ProcessWithPositionMap 处理文本并返回位置映射
func (p *Preprocessor) ProcessWithPositionMap(text string) ProcessResult {
	if text == "" {
		return ProcessResult{
			ProcessedText: "",
			PositionMap:   []int{},
		}
	}

	// 获取配置的映射表
	fullWidthMap := p.config.GetFullWidthToHalf()
	numberStyleMap := p.config.GetNumberStyle()

	// 转换为rune切片以正确处理Unicode字符
	runes := []rune(text)
	result := make([]rune, 0, len(runes))
	positionMap := make([]int, 0, len(runes))

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// 形近字检测
		if p.options.EnableSimilarShape {
			if normalized := p.normalizeSimilarShape(r); normalized != r {
				r = normalized
			}
		}

		// 同音字检测（这里只是标记，实际匹配在算法层处理）
		// SkipWhitespace 在同音字处理之前处理
		if p.options.SkipWhitespace && unicode.IsSpace(r) {
			continue
		}

		// 忽略大小写
		if p.options.IgnoreCase {
			r = unicode.ToLower(r)
		}

		// 全角转半角
		if p.options.IgnoreWidth {
			if half, ok := fullWidthMap[r]; ok {
				r = half
			} else if r > 0xFF00 && r < 0xFF5F {
				// 兜底：使用Unicode规则转换
				r = r - 0xFEE0
			}
		}

		// 数字样式统一
		if p.options.IgnoreNumStyle && (unicode.IsNumber(r) || p.isChineseNumber(r)) {
			if normalized, ok := numberStyleMap[r]; ok {
				r = normalized
			}
		}

		result = append(result, r)
		positionMap = append(positionMap, i)
	}

	return ProcessResult{
		ProcessedText: string(result),
		PositionMap:   positionMap,
	}
}

// normalizeSimilarShape 将形近字统一为标准字符
func (p *Preprocessor) normalizeSimilarShape(r rune) rune {
	similarChars := similar.GetSimilarChars(r)
	if similarChars == nil {
		return r
	}
	// 返回第一个相似字作为标准化结果
	// 优先选择简体/常见字符
	for _, s := range similarChars {
		// 简单策略：选择ASCII字符优先，然后选择较小Unicode码点的字符
		if s < 128 {
			return s
		}
	}
	// 如果没有ASCII字符，返回第一个
	return similarChars[0]
}

// NormalizeHomophone 处理同音字（外部调用）
func (p *Preprocessor) NormalizeHomophone(text string) []string {
	if !p.options.EnableHomophone {
		return []string{text}
	}

	runes := []rune(text)
	results := make([]string, 0, 1)

	// 对于每个字符，如果它有同音字，生成所有可能的组合
	// 为了简化实现，这里只处理单个同音字替换的情况
	results = append(results, text)

	for i, r := range runes {
		homophones := homophone.GetHomophones(r)
		if len(homophones) > 0 {
			// 为每个同音字生成新文本
			for _, h := range homophones {
				newRunes := make([]rune, len(runes))
				copy(newRunes, runes)
				newRunes[i] = h
				results = append(results, string(newRunes))
			}
		}
	}

	return results
}

// DetectAndReplacePinyin 检测文本中的拼音并替换为可能的汉字组合
func (p *Preprocessor) DetectAndReplacePinyin(text string) []string {
	if !p.options.EnableZhPYMix {
		return []string{text}
	}

	// 拼音正则表达式：匹配连续的小写英文字母（使用预编译正则）
	re := getPinyinRegex()

	results := make([]string, 0, 1)
	results = append(results, text)

	matches := re.FindAllStringIndex(text, -1)

	// 处理所有拼音匹配
	for _, match := range matches {
		start, end := match[0], match[1]
		pinyinStr := text[start:end]

		// 检查是否是拼音
		chars := pinyin.GetCharsFromPinyin(pinyinStr)
		if len(chars) > 0 {
			// 为每个可能的汉字生成新文本
			for _, char := range chars {
				newText := text[:start] + char + text[end:]
				results = append(results, newText)
			}
		}
	}

	// 处理多音节拼音组合（如 "dupin" -> "毒品"）
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			firstMatch := matches[i]
			secondMatch := matches[j]

			// 检查两个拼音是否相邻（中间可能有特殊字符）
			if secondMatch[0]-firstMatch[1] <= 1 {
				firstPinyin := text[firstMatch[0]:firstMatch[1]]
				secondPinyin := text[secondMatch[0]:secondMatch[1]]

				firstChars := pinyin.GetCharsFromPinyin(firstPinyin)
				secondChars := pinyin.GetCharsFromPinyin(secondPinyin)

				if len(firstChars) > 0 && len(secondChars) > 0 {
					// 为每个可能的汉字组合生成新文本
					for _, firstChar := range firstChars {
						for _, secondChar := range secondChars {
							newText := text[:firstMatch[0]] + firstChar + secondChar + text[secondMatch[1]:]
							results = append(results, newText)
						}
					}
				}
			}
		}
	}

	// 去重
	unique := make(map[string]bool)
	uniqueResults := make([]string, 0)
	for _, result := range results {
		if !unique[result] {
			unique[result] = true
			uniqueResults = append(uniqueResults, result)
		}
	}

	return uniqueResults
}

// ProcessWithPinyin 处理包含拼音混合的文本
func (p *Preprocessor) ProcessWithPinyin(text string) []string {
	// 检查缓存
	if results, ok := p.processCache.Get(text); ok {
		return results
	}

	// 处理文本
	processed := p.Process(text)
	var results []string

	if p.options.EnableZhPYMix {
		results = p.DetectAndReplacePinyin(processed)
	} else {
		results = []string{processed}
	}

	// 更新缓存（LRU 自动管理淘汰）
	p.processCache.Put(text, results)

	return results
}

// isChineseNumber 判断是否是中文数字
func (p *Preprocessor) isChineseNumber(r rune) bool {
	numberStyleMap := p.config.GetNumberStyle()
	_, ok := numberStyleMap[r]
	return ok
}

// normalizeNumber 将各种数字字符统一为ASCII数字
func (p *Preprocessor) normalizeNumber(r rune) rune {
	// 首先尝试从配置的映射表获取
	numberStyleMap := p.config.GetNumberStyle()
	if normalized, ok := numberStyleMap[r]; ok {
		return normalized
	}

	// 如果映射表中没有，使用默认逻辑
	switch {
	case r >= '0' && r <= '9':
		return r
	default:
		return r
	}
}
