package detector

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"unicode"

	"touchgocore/go-swd/algorithm"
	"touchgocore/go-swd/config"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/detector/preprocessor"
	"touchgocore/go-swd/dictionary"
	"touchgocore/go-swd/types/category"
	"touchgocore/go-swd/types/pinyin"
	"touchgocore/go-swd/types/similar"
)

// detector 实现敏感词检测器接口
type detector struct {
	primary          core.Algorithm     // 主要算法（始终构建，默认 AC 自动机）
	secondary        core.Algorithm     // 次要算法（可选，默认禁用）
	secondaryEnabled atomic.Bool        // 是否启用次要算法
	secondaryBuilt   atomic.Bool        // 次要算法是否已构建
	secondaryType    core.AlgorithmType // 次要算法类型（默认 Trie）
	preprocess       *preprocessor.Preprocessor
	mu               sync.RWMutex
	options          *core.SWDOptions
	config           *config.MappingConfig
	wordCount        int                          // 词库规模
	avgWordLength    float64                      // 平均词长，用于预分配

	// Rune 缓存：减少重复的 []rune(text) 转换
	runeCache sync.Map // map[string][]rune

	// 词库快照缓存：用于读操作，提升并发性能（读写分离）
	wordsCache atomic.Value // map[string]category.Category

	// 算法选择缓存（线程安全）
	algoCache atomic.Value // map[byte]core.Algorithm
	cacheUpdated atomic.Bool
}

// NewDetector 创建一个新的检测器实例
func NewDetector(options *core.SWDOptions) (core.Detector, error) {
	return NewDetectorWithConfig(options, config.NewMappingConfig())
}

// NewDetectorWithConfig 使用自定义配置创建检测器
func NewDetectorWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) (core.Detector, error) {
	loader := dictionary.NewLoader()

	// 加载词典
	if err := loader.LoadDefaultWords(context.Background()); err != nil {
		return nil, fmt.Errorf("加载默认词典失败: %w", err)
	}

	// 加载映射文件
	mappingLoader := dictionary.NewMappingLoader("")
	if err := mappingLoader.LoadFromFiles(); err != nil {
		// 映射文件加载失败不影响主功能，只记录错误
		log.Printf("加载映射文件失败: %v", err)
	} else {
		// 使用加载的映射配置
		cfg = mappingLoader.GetConfig()
		// 设置到拼音包和形近字包的全局配置
		pinyin.SetMappingConfig(cfg)
		similar.SetMappingConfig(cfg)
	}

	// 获取词典内容
	words := loader.GetWords()
	if len(words) == 0 {
		return nil, fmt.Errorf("词典内容为空")
	}

	wordCount := len(words)

	// 默认使用 AC 自动机作为主要算法
	primaryAlgo := algorithm.NewAhoCorasick()
	if err := primaryAlgo.Build(words); err != nil {
		return nil, fmt.Errorf("构建 AC 自动机失败: %w", err)
	}

	// 计算平均词长，用于预分配切片
	avgWordLength := 0.0
	for word := range words {
		avgWordLength += float64(len([]rune(word)))
	}
	if wordCount > 0 {
		avgWordLength /= float64(wordCount)
	}

	d := &detector{
		primary:          primaryAlgo,
		secondary:        nil,
		secondaryEnabled: atomic.Bool{},
		secondaryBuilt:   atomic.Bool{},
		secondaryType:    core.AlgorithmTrie, // 默认次要算法为 Trie
		wordCount:        wordCount,
		avgWordLength:    avgWordLength,
		preprocess:       preprocessor.NewPreprocessorWithConfig(options, cfg),
		options:          options,
		config:           cfg,
		cacheUpdated:     atomic.Bool{},
	}

	// 初始化词库缓存
	d.updateWordsCache(words)

	// 初始化算法选择缓存
	d.updateAlgoCache()

	log.Printf("词库规模: %d, 主要算法: AC 自动机, 次要算法: 未启用", wordCount)

	// 初始化算法选择缓存
	d.updateAlgoCache()

	// 注册为观察者
	loader.AddObserver(d)

	return d, nil
}

// OnWordsChanged 实现Observer接口,当词库变更时重建算法
func (d *detector) OnWordsChanged(words map[string]category.Category) {
	d.mu.Lock()
	defer d.mu.Unlock()

	wordCount := len(words)
	d.wordCount = wordCount

	// 计算新的平均词长
	avgWordLength := 0.0
	for word := range words {
		avgWordLength += float64(len([]rune(word)))
	}
	if wordCount > 0 {
		avgWordLength /= float64(wordCount)
	}
	d.avgWordLength = avgWordLength

	// 标记次要算法需要重建
	d.secondaryBuilt.Store(false)

	// 重建主要算法
	if err := d.primary.Build(words); err != nil {
		log.Printf("重建主要算法失败: %v", err)
	}

	// 更新词库缓存
	d.updateWordsCacheUnlocked(words)
}

// updateWordsCache 更新词库快照缓存（需要在锁外调用）
func (d *detector) updateWordsCache(words map[string]category.Category) {
	snapshot := make(map[string]category.Category, len(words))
	for k, v := range words {
		snapshot[k] = v
	}
	d.wordsCache.Store(snapshot)
}

// updateWordsCacheUnlocked 更新词库快照缓存（需要在锁内调用）
func (d *detector) updateWordsCacheUnlocked(words map[string]category.Category) {
	snapshot := make(map[string]category.Category, len(words))
	for k, v := range words {
		snapshot[k] = v
	}
	d.wordsCache.Store(snapshot)
}

// getWordsCache 获取词库快照（无锁读取）
func (d *detector) getWordsCache() map[string]category.Category {
	return d.wordsCache.Load().(map[string]category.Category)
}

// EnableSecondaryAlgorithm 启用次要算法（默认 Trie）
// 这是一个显式调用，允许用户在需要时启用 Trie
func (d *detector) EnableSecondaryAlgorithm() error {
	if d.secondaryEnabled.Load() {
		return nil // 已经启用
	}

	// 先标记为启用
	d.secondaryEnabled.Store(true)

	// 然后构建次要算法
	_, err := d.getSecondaryAlgorithm()
	if err != nil {
		// 如果构建失败，标记为禁用
		d.secondaryEnabled.Store(false)
		return err
	}

	log.Printf("已启用次要算法: %s", d.secondaryType)
	return nil
}

// DisableSecondaryAlgorithm 禁用次要算法并释放内存
func (d *detector) DisableSecondaryAlgorithm() {
	d.secondaryBuilt.Store(false)
	d.secondaryEnabled.Store(false)
	d.secondary = nil
	log.Printf("已禁用次要算法")
}

// getSecondaryAlgorithm 获取或构建次要算法（懒加载）
func (d *detector) getSecondaryAlgorithm() (core.Algorithm, error) {
	if !d.secondaryEnabled.Load() {
		return nil, fmt.Errorf("次要算法未启用")
	}

	if d.secondaryBuilt.Load() {
		return d.secondary, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// 双重检查
	if d.secondaryBuilt.Load() {
		return d.secondary, nil
	}

	// 构建次要算法
	var newAlgo core.Algorithm
	switch d.secondaryType {
	case core.AlgorithmTrie:
		newAlgo = algorithm.NewTrie()
	case core.AlgorithmAhoCorasick:
		newAlgo = algorithm.NewAhoCorasick()
	default:
		return nil, fmt.Errorf("未知的次要算法类型: %s", d.secondaryType)
	}

	// 从缓存获取词库（无锁读取）
	words := d.getWordsCache()
	if err := newAlgo.Build(words); err != nil {
		return nil, fmt.Errorf("构建次要算法失败: %w", err)
	}

	d.secondary = newAlgo
	d.secondaryBuilt.Store(true)

	log.Printf("懒加载构建次要算法: %s", d.secondaryType)
	return d.secondary, nil
}

// getRunes 获取文本的 rune 切片，使用缓存优化
func (d *detector) getRunes(text string) []rune {
	// 短文本直接转换，避免缓存开销
	if len(text) < textLengthThreshold {
		return []rune(text)
	}

	// 尝试从缓存获取
	if cached, ok := d.runeCache.Load(text); ok {
		return cached.([]rune)
	}

	// 转换并缓存
	runes := []rune(text)
	d.runeCache.Store(text, runes)
	return runes
}

// updateAlgoCache 更新算法选择缓存
func (d *detector) updateAlgoCache() {
	// 根据配置确定算法选择
	hasPinyin := d.options.EnablePinyin || d.options.EnableZhPYMix
	maxDistance := d.options.MaxDistance
	secondaryEnabled := d.secondaryEnabled.Load()

	// 创建新缓存
	newCache := make(map[byte]core.Algorithm, algoCacheSize)

	// 预分配变量，减少循环内的内存分配
	var algo core.Algorithm
	var textLen int

	// 根据配置填充缓存
	for len := byte(0); len < byte(algoCacheSize); len++ {
		textLen = int(len) * lengthInterval

		// 判断规则
		needSecondary := (textLen < shortTextLength && hasPinyin) || (maxDistance > 0)
		useSecondary := secondaryEnabled && needSecondary

		if useSecondary {
			// 尝试获取次要算法，如果失败则用主要算法
			if secondary, err := d.getSecondaryAlgorithm(); err == nil {
				algo = secondary
			} else {
				algo = d.primary
			}
		} else {
			algo = d.primary
		}

		newCache[len] = algo
	}

	// 原子更新缓存
	d.algoCache.Store(newCache)
	d.cacheUpdated.Store(true)
	log.Printf("已更新算法选择缓存（拼音:%v, 距离:%d, 次要:%v）",
		hasPinyin, maxDistance, secondaryEnabled)
}

// UpdateAlgoCache 更新算法选择缓存（实现 Detector 接口）
func (d *detector) UpdateAlgoCache() {
	d.updateAlgoCache()
}

// selectAlgorithmForText 根据文本特征选择最优算法（使用缓存）
func (d *detector) selectAlgorithmForText(text string) core.Algorithm {
	// 快速路径：未启用次要算法
	if !d.secondaryEnabled.Load() {
		return d.primary
	}

	// 使用缓存：根据文本长度区间快速选择
	textLen := len(d.getRunes(text))
	if textLen >= textLengthThreshold {
		cache := d.algoCache.Load().(map[byte]core.Algorithm)
		return cache[maxCacheIndex] // 长文本直接使用主要算法
	}

	// 根据长度选择缓存区间
	cacheIndex := byte(textLen / lengthInterval)
	if cacheIndex > maxCacheIndex {
		cacheIndex = maxCacheIndex
	}

	cache := d.algoCache.Load().(map[byte]core.Algorithm)
	return cache[cacheIndex]
}

// Detect 检查文本是否包含任何敏感词
func (d *detector) Detect(text string) bool {
	if text == "" {
		return false
	}

	// 选择算法（在锁外执行，避免死锁）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本（支持拼音混合）
	textVariations := d.preprocess.ProcessWithPinyin(text)

	// 进行检测
	for _, processedText := range textVariations {
		var match *core.SensitiveWord
		if d.options.MaxDistance > 0 {
			match = algo.MatchWithDistance(processedText, d.options.MaxDistance)
		} else {
			match = algo.Match(processedText)
		}

		// 如果没找到，尝试同音字检测
		if match == nil && d.options.EnableHomophone {
			homophoneVariations := d.preprocess.NormalizeHomophone(processedText)
			for _, variation := range homophoneVariations {
				if d.options.MaxDistance > 0 {
					match = algo.MatchWithDistance(variation, d.options.MaxDistance)
				} else {
					match = algo.Match(variation)
				}
				if match != nil {
					break
				}
			}
		}

		if match != nil {
			return true
		}
	}

	return false
}

// DetectIn 检查文本是否包含指定分类的敏感词
func (d *detector) DetectIn(text string, categories ...category.Category) bool {
	if text == "" || len(categories) == 0 {
		return false
	}

	// 选择算法（在锁外执行）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本
	processedText := d.preprocess.Process(text)

	// 进行检测
	var matches []core.SensitiveWord
	if d.options.MaxDistance > 0 {
		matches = algo.MatchAllWithDistance(processedText, d.options.MaxDistance)
	} else {
		matches = algo.MatchAll(processedText)
	}

	// 检查是否有任何匹配的分类
	for _, match := range matches {
		for _, cat := range categories {
			if cat.Contains(match.Category) {
				return true
			}
		}
	}

	return false
}

// Match 返回文本中找到的第一个敏感词
func (d *detector) Match(text string) *core.SensitiveWord {
	if text == "" {
		return nil
	}

	// 选择算法（在锁外执行）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本（支持拼音混合）
	textVariations := d.preprocess.ProcessWithPinyin(text)

	// 预分配变量，减少内存分配
	var match *core.SensitiveWord
	var homophoneVariations []string
	var processResult preprocessor.ProcessResult
	var positionMap []int
	var runes []rune
	var charCount int
	var endPos int
	var wordRunes []rune

	for _, processedText := range textVariations {
		if d.options.MaxDistance > 0 {
			match = algo.MatchWithDistance(processedText, d.options.MaxDistance)
		} else {
			match = algo.Match(processedText)
		}

		// 如果没找到，尝试同音字检测
		if match == nil && d.options.EnableHomophone {
			homophoneVariations = d.preprocess.NormalizeHomophone(processedText)
			for _, variation := range homophoneVariations {
				if d.options.MaxDistance > 0 {
					match = algo.MatchWithDistance(variation, d.options.MaxDistance)
				} else {
					match = algo.Match(variation)
				}
				if match != nil {
					break
				}
			}
		}

		if match != nil {
			// 预处理原始文本以获取位置映射
			processResult = d.preprocess.ProcessWithPositionMap(text)
			positionMap = processResult.PositionMap
			// 将匹配位置映射回原始文本
			if match.StartPos >= 0 && match.StartPos < len(positionMap) {
				match.StartPos = positionMap[match.StartPos]
			}

			// 映射结束位置：使用原始敏感词的长度，而不是匹配的长度
			// 这样可以确保只替换敏感词本身的字符
			runes = d.getRunes(text)
			charCount = 0
			endPos = match.StartPos
			wordRunes = []rune(match.Word)
			wordLen := len(wordRunes)
			for j := match.StartPos; j < len(runes) && charCount < wordLen; j++ {
				if !d.options.SkipWhitespace || !unicode.IsSpace(runes[j]) {
					charCount++
					if charCount == wordLen {
						endPos = j
						break
					}
				}
			}
			match.EndPos = endPos + 1

			return match
		}
	}

	return nil
}

// MatchIn 返回文本中找到的第一个指定分类的敏感词
func (d *detector) MatchIn(text string, categories ...category.Category) *core.SensitiveWord {
	if text == "" || len(categories) == 0 {
		return nil
	}

	// 选择算法（在锁外执行）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本并获取位置映射
	processResult := d.preprocess.ProcessWithPositionMap(text)
	processedText := processResult.ProcessedText
	positionMap := processResult.PositionMap

	// 进行检测
	var matches []core.SensitiveWord
	if d.options.MaxDistance > 0 {
		matches = algo.MatchAllWithDistance(processedText, d.options.MaxDistance)
	} else {
		matches = algo.MatchAll(processedText)
	}

	// 将匹配位置映射回原始文本
	for i := range matches {
		if matches[i].StartPos >= 0 && matches[i].StartPos < len(positionMap) {
			matches[i].StartPos = positionMap[matches[i].StartPos]
		}
		if matches[i].EndPos > 0 && matches[i].EndPos <= len(positionMap) {
			matches[i].EndPos = positionMap[matches[i].EndPos-1] + 1
		}
	}

	// 返回第一个匹配的分类
	for _, match := range matches {
		for _, cat := range categories {
			if cat.Contains(match.Category) {
				result := match
				return &result
			}
		}
	}

	return nil
}

// MatchAll 返回文本中找到的所有敏感词
func (d *detector) MatchAll(text string) []core.SensitiveWord {
	if text == "" {
		return nil
	}

	// 选择算法（在锁外执行）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本并获取位置映射
	processResult := d.preprocess.ProcessWithPositionMap(text)
	processedText := processResult.ProcessedText
	positionMap := processResult.PositionMap

	// 进行检测
	var matches []core.SensitiveWord
	if d.options.MaxDistance > 0 {
		matches = algo.MatchAllWithDistance(processedText, d.options.MaxDistance)
	} else {
		matches = algo.MatchAll(processedText)
	}

	// 预分配过滤后的匹配结果切片，使用平均词长估算
	estimatedCount := len(processedText) / int(d.avgWordLength)
	if estimatedCount < len(matches) {
		estimatedCount = len(matches)
	}
	filteredMatches := make([]core.SensitiveWord, 0, estimatedCount)
	for _, match := range matches {
		// 检查匹配的文本是否与敏感词相同（忽略大小写等）
		if len(match.Word) > 0 {
			filteredMatches = append(filteredMatches, match)
		}
	}

	// 使用公共方法映射位置
	return d.mapPositionsToOriginal(filteredMatches, text, positionMap)
}

// MatchAllIn 返回文本中找到的所有指定分类的敏感词
func (d *detector) MatchAllIn(text string, categories ...category.Category) []core.SensitiveWord {
	if text == "" || len(categories) == 0 {
		return nil
	}

	// 选择算法（在锁外执行）
	algo := d.selectAlgorithmForText(text)

	// 预处理文本并获取位置映射
	processResult := d.preprocess.ProcessWithPositionMap(text)
	processedText := processResult.ProcessedText
	positionMap := processResult.PositionMap

	// 进行检测
	var matches []core.SensitiveWord
	if d.options.MaxDistance > 0 {
		matches = algo.MatchAllWithDistance(processedText, d.options.MaxDistance)
	} else {
		matches = algo.MatchAll(processedText)
	}

	// 过滤匹配结果，只保留完整的敏感词匹配
	var filteredMatches []core.SensitiveWord
	for _, match := range matches {
		// 检查匹配的文本是否与敏感词相同（忽略大小写等）
		if len(match.Word) > 0 {
			filteredMatches = append(filteredMatches, match)
		}
	}

	// 使用公共方法映射位置
	filteredMatches = d.mapPositionsToOriginal(filteredMatches, text, positionMap)

	// 过滤出指定分类的敏感词
	var result []core.SensitiveWord
	for _, match := range filteredMatches {
		for _, cat := range categories {
			if cat.Contains(match.Category) {
				result = append(result, match)
				break // 避免同一个敏感词被多个分类匹配而重复添加
			}
		}
	}

	return result
}

// mapPositionsToOriginal 将匹配位置映射回原始文本
// 这是一个公共方法，用于消除重复的位置映射代码
func (d *detector) mapPositionsToOriginal(matches []core.SensitiveWord, text string, positionMap []int) []core.SensitiveWord {
	if len(matches) == 0 {
		return matches
	}

	// 预分配变量，减少内存分配
	var runes []rune
	var charCount int
	var endPos int
	var wordLen int

	// 将匹配位置映射回原始文本
	for i := range matches {
		// 映射开始位置
		if matches[i].StartPos >= 0 && matches[i].StartPos < len(positionMap) {
			matches[i].StartPos = positionMap[matches[i].StartPos]
		}

		// 映射结束位置：使用原始敏感词的长度，而不是匹配的长度
		// 这样可以确保只替换敏感词本身的字符
		wordLen = len([]rune(matches[i].Word))
		if matches[i].StartPos >= 0 && wordLen > 0 {
			// 计算原始文本中敏感词的结束位置
			// 例如，对于 "f U c K"，我们需要找到第4个非空格字符的位置
			runes = d.getRunes(text)
			charCount = 0
			endPos = matches[i].StartPos
			for j := matches[i].StartPos; j < len(runes); j++ {
				if !d.options.SkipWhitespace || !unicode.IsSpace(runes[j]) {
					charCount++
					if charCount == wordLen {
						endPos = j
						break
					}
				}
			}
			matches[i].EndPos = endPos + 1
		} else if matches[i].EndPos > 0 && matches[i].EndPos <= len(positionMap) {
			matches[i].EndPos = positionMap[matches[i].EndPos-1] + 1
		}
	}

	return matches
}
