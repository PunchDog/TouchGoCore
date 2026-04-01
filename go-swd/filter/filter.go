package filter

import (
	"sort"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

// filter 实现了敏感词过滤器接口
type filter struct {
	detector core.Detector
}

// NewFilter 创建一个新的过滤器实例
func NewFilter(detector core.Detector) core.Filter {
	return &filter{
		detector: detector,
	}
}

// Replace 使用指定的替换字符替换敏感词
func (f *filter) Replace(text string, replacement rune) string {
	if text == "" {
		return text
	}

	// 获取所有匹配的敏感词
	matches := f.detector.MatchAll(text)
	if len(matches) == 0 {
		return text
	}

	// 去重并保留最长的匹配结果
	uniqueMatches := f.removeOverlappingMatches(matches)

	// 按照位置排序，从后向前替换
	sort.Slice(uniqueMatches, func(i, j int) bool {
		return uniqueMatches[i].StartPos > uniqueMatches[j].StartPos
	})

	// 转换为rune数组以便处理中文
	runes := []rune(text)
	for _, match := range uniqueMatches {
		// 确保匹配位置有效
		if match.StartPos < 0 || match.EndPos > len(runes) {
			continue
		}
		// 替换敏感词，替换整个匹配范围，包括中间的空格
		for i := match.StartPos; i < match.EndPos; i++ {
			runes[i] = replacement
		}
	}

	return string(runes)
}

// removeOverlappingMatches 去重并保留最长的匹配结果
func (f *filter) removeOverlappingMatches(matches []core.SensitiveWord) []core.SensitiveWord {
	if len(matches) <= 1 {
		return matches
	}

	// 按照长度排序，长的在前；长度相同的按起始位置排序，小的在前
	sort.Slice(matches, func(i, j int) bool {
		lenI := matches[i].EndPos - matches[i].StartPos
		lenJ := matches[j].EndPos - matches[j].StartPos
		if lenI != lenJ {
			return lenI > lenJ
		}
		return matches[i].StartPos < matches[j].StartPos
	})

	var uniqueMatches []core.SensitiveWord
	for _, match := range matches {
		// 检查是否与已选择的匹配重叠
		overlap := false
		for _, existing := range uniqueMatches {
			// 如果当前匹配与已存在的匹配有重叠，则跳过
			if !(match.EndPos <= existing.StartPos || match.StartPos >= existing.EndPos) {
				overlap = true
				break
			}
		}
		if !overlap {
			uniqueMatches = append(uniqueMatches, match)
		}
	}

	return uniqueMatches
}

// ReplaceIn 使用指定的替换字符替换指定分类的敏感词
func (f *filter) ReplaceIn(text string, replacement rune, categories ...category.Category) string {
	if text == "" || len(categories) == 0 {
		return text
	}

	// 获取所有匹配的敏感词
	matches := f.detector.MatchAllIn(text, categories...)
	if len(matches) == 0 {
		return text
	}

	// 按照位置排序，从后向前替换
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].StartPos > matches[j].StartPos
	})

	// 转换为rune数组以便处理中文
	runes := []rune(text)
	for _, match := range matches {
		// 替换敏感词
		for i := match.StartPos; i < match.EndPos; i++ {
			runes[i] = replacement
		}
	}

	return string(runes)
}

// ReplaceWithAsterisk 使用 * 号替换敏感词
func (f *filter) ReplaceWithAsterisk(text string) string {
	return f.Replace(text, '*')
}

// ReplaceWithAsteriskIn 使用 * 号替换指定分类的敏感词
func (f *filter) ReplaceWithAsteriskIn(text string, categories ...category.Category) string {
	return f.ReplaceIn(text, '*', categories...)
}

// ReplaceWithStrategy 使用自定义替换策略替换敏感词
func (f *filter) ReplaceWithStrategy(text string, strategy func(word core.SensitiveWord) string) string {
	if text == "" || strategy == nil {
		return text
	}

	// 获取所有匹配的敏感词
	matches := f.detector.MatchAll(text)
	if len(matches) == 0 {
		return text
	}

	// 去重并保留最长的匹配结果
	uniqueMatches := f.removeOverlappingMatches(matches)

	// 按照位置排序，从后向前替换
	sort.Slice(uniqueMatches, func(i, j int) bool {
		return uniqueMatches[i].StartPos > uniqueMatches[j].StartPos
	})

	// 转换为rune数组以便处理中文
	result := []rune(text)
	for _, match := range uniqueMatches {
		// 确保匹配位置有效
		if match.StartPos < 0 || match.EndPos > len(result) {
			continue
		}
		// 获取替换文本
		replacement := []rune(strategy(match))
		// 计算需要替换的长度
		replaceLen := match.EndPos - match.StartPos
		if len(replacement) != replaceLen {
			// 如果替换文本长度不同，需要调整结果数组
			newResult := make([]rune, len(result)+(len(replacement)-replaceLen))
			copy(newResult, result[:match.StartPos])
			copy(newResult[match.StartPos:], replacement)
			copy(newResult[match.StartPos+len(replacement):], result[match.EndPos:])
			result = newResult
		} else {
			// 长度相同，直接替换
			copy(result[match.StartPos:], replacement)
		}
	}

	return string(result)
}

// ReplaceWithStrategyIn 使用自定义策略替换指定分类的敏感词
func (f *filter) ReplaceWithStrategyIn(text string, strategy func(word core.SensitiveWord) string, categories ...category.Category) string {
	if text == "" || len(categories) == 0 || strategy == nil {
		return text
	}

	// 获取所有匹配的敏感词
	matches := f.detector.MatchAllIn(text, categories...)
	if len(matches) == 0 {
		return text
	}

	// 按照位置排序，从后向前替换
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].StartPos > matches[j].StartPos
	})

	// 转换为rune数组以便处理中文
	result := []rune(text)
	for _, match := range matches {
		// 获取替换文本
		replacement := []rune(strategy(match))
		// 计算需要替换的长度
		replaceLen := match.EndPos - match.StartPos
		if len(replacement) != replaceLen {
			// 如果替换文本长度不同，需要调整结果数组
			newResult := make([]rune, len(result)+(len(replacement)-replaceLen))
			copy(newResult, result[:match.StartPos])
			copy(newResult[match.StartPos:], replacement)
			copy(newResult[match.StartPos+len(replacement):], result[match.EndPos:])
			result = newResult
		} else {
			// 长度相同，直接替换
			copy(result[match.StartPos:], replacement)
		}
	}

	return string(result)
}

// replaceWords 替换文本中的敏感词
func (f *filter) replaceWords(text string, matches []core.SensitiveWord, strategy func(word core.SensitiveWord) string) string {
	if len(matches) == 0 {
		return text
	}

	// 按照 StartPos 排序，相同位置的取长度较长的
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].StartPos == matches[j].StartPos {
			return matches[i].EndPos > matches[j].EndPos
		}
		return matches[i].StartPos < matches[j].StartPos
	})

	runes := []rune(text)
	result := make([]rune, 0, len(runes))
	lastPos := 0

	for _, match := range matches {
		// 确保匹配位置有效
		if match.StartPos < 0 || match.EndPos > len(runes) {
			continue
		}
		// 如果当前匹配的起始位置在上一个匹配的结束位置之前，说明是重叠词，跳过
		if match.StartPos < lastPos {
			continue
		}
		// 添加敏感词前的文本
		result = append(result, runes[lastPos:match.StartPos]...)
		// 添加替换后的文本
		replacement := []rune(strategy(match))
		result = append(result, replacement...)
		lastPos = match.EndPos
	}

	// 添加最后一个敏感词后的文本
	if lastPos < len(runes) {
		result = append(result, runes[lastPos:]...)
	}

	return string(result)
}
