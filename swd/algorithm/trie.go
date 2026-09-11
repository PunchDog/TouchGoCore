package algorithm

import (
	"log"

	"touchgocore/go-swd/common"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

// TrieNode Trie树节点
type TrieNode struct {
	children map[rune]*TrieNode // 子节点映射
	isEnd    bool               // 是否是单词结尾
	word     string             // 如果是结尾节点，存储完整词
	category category.Category  // 敏感词分类
}

// newTrieNode 创建新的Trie节点
func newTrieNode() *TrieNode {
	return &TrieNode{
		children: make(map[rune]*TrieNode),
	}
}

// Trie 字典树实现
type Trie struct {
	root    *TrieNode
	strPool *common.StringPool // 字符串池，用于去重
}

// NewTrie 创建新的Trie树
func NewTrie() *Trie {
	return &Trie{
		root:    newTrieNode(),
		strPool: common.NewStringPool(),
	}
}

// Type 返回算法类型
func (t *Trie) Type() core.AlgorithmType {
	return core.AlgorithmTrie
}

// Build 构建敏感词字典树
func (t *Trie) Build(words map[string]category.Category) error {
	t.root = newTrieNode()
	for word, category := range words {
		current := t.root
		for _, char := range word {
			if _, exists := current.children[char]; !exists {
				current.children[char] = newTrieNode()
			}
			current = current.children[char]
		}
		current.isEnd = true
		current.word = t.strPool.Intern(word) // 使用字符串池去重
		current.category = category
	}
	return nil
}

// Match 返回文本中第一个敏感词
func (t *Trie) Match(text string) *core.SensitiveWord {
	runes := []rune(text)
	for i := range runes {
		if match := t.matchFromPosition(text, i); match != nil {
			return match
		}
	}
	return nil
}

// matchFromPosition 从指定位置开始匹配
func (t *Trie) matchFromPosition(text string, start int) *core.SensitiveWord {
	current := t.root
	runes := []rune(text)
	if start >= len(runes) {
		return nil
	}

	for i, char := range runes[start:] {
		next, exists := current.children[char]
		if !exists {
			break
		}
		current = next
		if current.isEnd {
			return &core.SensitiveWord{
				Word:     current.word,
				StartPos: start,
				EndPos:   start + i + 1,
				Category: current.category,
			}
		}
	}
	return nil
}

// MatchAll 返回文本中所有敏感词
func (t *Trie) MatchAll(text string) []core.SensitiveWord {
	var matches []core.SensitiveWord
	runes := []rune(text)
	for i := range runes {
		if match := t.matchFromPosition(text, i); match != nil {
			matches = append(matches, *match)
		}
	}
	return matches
}

// Replace 替换敏感词
func (t *Trie) Replace(text string, replacement rune) string {
	matches := t.MatchAll(text)
	if len(matches) == 0 {
		return text
	}

	runes := []rune(text)
	for _, match := range matches {
		for i := match.StartPos; i < match.EndPos; i++ {
			runes[i] = replacement
		}
	}
	return string(runes)
}

// Detect 检查文本是否包含敏感词
func (t *Trie) Detect(text string) bool {
	return t.Match(text) != nil
}

// MatchWithDistance 使用最大距离返回文本中第一个敏感词
func (t *Trie) MatchWithDistance(text string, maxDistance int) *core.SensitiveWord {
	if maxDistance <= 0 {
		return t.Match(text)
	}

	runes := []rune(text)
	n := len(runes)

	// 优化：使用单次遍历，避免重复转换和内存分配
	for startPos := 0; startPos < n; startPos++ {
		distanceCount := 0
		current := t.root

		for endPos := startPos; endPos < n; endPos++ {
			char := runes[endPos]
			next, exists := current.children[char]

			if !exists {
				distanceCount++
				if distanceCount > maxDistance {
					break
				}
				continue
			}

			current = next
			distanceCount = 0 // 匹配成功，重置距离计数器

			if current.isEnd {
				return &core.SensitiveWord{
					Word:     current.word,
					StartPos: startPos,
					EndPos:   endPos + 1,
					Category: current.category,
				}
			}
		}
	}
	return nil
}

// MatchAllWithDistance 使用最大距离返回文本中所有敏感词
func (t *Trie) MatchAllWithDistance(text string, maxDistance int) []core.SensitiveWord {
	if maxDistance <= 0 {
		return t.MatchAll(text)
	}

	// 预分配匹配结果切片，减少动态扩容
	runes := []rune(text)
	n := len(runes)
	estimatedMatches := n / 3 // 粗略估计：平均每3个字符一个匹配
	matches := make([]core.SensitiveWord, 0, estimatedMatches)

	for startPos := 0; startPos < n; startPos++ {
		distanceCount := 0
		current := t.root

		for endPos := startPos; endPos < n; endPos++ {
			char := runes[endPos]
			next, exists := current.children[char]

			if !exists {
				distanceCount++
				if distanceCount > maxDistance {
					break
				}
				continue
			}

			current = next
			distanceCount = 0 // 匹配成功，重置距离计数器

			if current.isEnd {
				matches = append(matches, core.SensitiveWord{
					Word:     current.word,
					StartPos: startPos,
					EndPos:   endPos + 1,
					Category: current.category,
				})
			}
		}
	}
	return matches
}

// OnWordsChanged 实现 Observer 接口,当词库变更时重建算法
func (t *Trie) OnWordsChanged(words map[string]category.Category) {
	if err := t.Build(words); err != nil {
		// 这里只能记录错误,因为是回调方法
		log.Printf("重建算法失败: %v", err)
	}
}
