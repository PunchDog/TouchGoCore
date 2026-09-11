package algorithm

import (
	"log"

	"touchgocore/go-swd/common"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

// AhoCorasickNode Aho-Corasick算法节点
type AhoCorasickNode struct {
	children map[rune]*AhoCorasickNode // 子节点映射
	failLink *AhoCorasickNode          // 失败指针
	isEnd    bool                      // 是否是单词结尾
	word     string                    // 如果是结尾节点，存储完整词
	category category.Category         // 敏感词分类
	parent   *AhoCorasickNode          // 父节点 (用于重建单词)
	depth    int                       // 在字典树中的深度
	output   []*AhoCorasickNode        // 输出列表：所有通过失败链可到达的结束节点
}

// newAhoCorasickNode 创建新的Aho-Corasick算法节点
func newAhoCorasickNode() *AhoCorasickNode {
	return &AhoCorasickNode{
		children: make(map[rune]*AhoCorasickNode),
	}
}

// AhoCorasick Aho-Corasick算法实现
type AhoCorasick struct {
	root    *AhoCorasickNode
	built   bool               // 是否已构建失败指针
	strPool *common.StringPool // 字符串池，用于去重
}

// NewAhoCorasick 创建新的Aho-Corasick算法实例
func NewAhoCorasick() *AhoCorasick {
	return &AhoCorasick{
		root:    newAhoCorasickNode(),
		strPool: common.NewStringPool(),
	}
}

// Type 返回算法类型
func (ac *AhoCorasick) Type() core.AlgorithmType {
	return core.AlgorithmAhoCorasick
}

// Build 构建Aho-Corasick算法词库
func (ac *AhoCorasick) Build(words map[string]category.Category) error {
	ac.root = newAhoCorasickNode()
	for word, category := range words {
		ac.insert(word, category)
	}

	ac.buildFailureLinks()
	return nil
}

// insert 向自动机中添加一个词
func (ac *AhoCorasick) insert(word string, category category.Category) {
	if word == "" {
		return
	}

	current := ac.root
	for i, char := range word {
		if _, exists := current.children[char]; !exists {
			current.children[char] = newAhoCorasickNode()
			current.children[char].parent = current
			current.children[char].depth = i + 1
		}
		current = current.children[char]
	}

	current.isEnd = true
	current.word = ac.strPool.Intern(word) // 使用字符串池去重
	current.category = category
	ac.built = false // 需要重新构建失败指针
}

// buildFailureLinks 构建失败指针和输出列表
func (ac *AhoCorasick) buildFailureLinks() {
	if ac.built {
		return
	}

	// 使用BFS构建失败指针和输出列表
	queue := make([]*AhoCorasickNode, 0)

	// 先处理根节点的子节点
	for _, child := range ac.root.children {
		child.failLink = ac.root
		// 预计算输出列表（根节点的子节点）
		ac.buildOutputList(child)
		queue = append(queue, child)
	}

	// 处理剩余节点
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for char, child := range current.children {
			queue = append(queue, child)

			// 寻找失败指针
			failNode := current.failLink
			for failNode != nil {
				if next, exists := failNode.children[char]; exists {
					child.failLink = next
					break
				}
				failNode = failNode.failLink
			}
			if failNode == nil {
				child.failLink = ac.root
			}

			// 预计算输出列表
			ac.buildOutputList(child)
		}
	}

	ac.built = true
}

// buildOutputList 预计算节点的输出列表（所有通过失败链可到达的结束节点）
func (ac *AhoCorasick) buildOutputList(node *AhoCorasickNode) {
	if node.failLink != nil && node.failLink.isEnd {
		// 将失败链上的结束节点添加到输出列表
		node.output = append(node.output, node.failLink)
		// 递归添加更深层的输出
		for _, failNode := range node.failLink.output {
			node.output = append(node.output, failNode)
		}
	}
}

// Match 查找文本中的第一个匹配
func (ac *AhoCorasick) Match(text string) *core.SensitiveWord {
	if !ac.built {
		ac.buildFailureLinks()
	}

	current := ac.root
	runes := []rune(text)

	for pos, char := range runes {
		// 查找下一个状态
		for current != ac.root && current.children[char] == nil {
			current = current.failLink
		}

		if next, exists := current.children[char]; exists {
			current = next
		} else {
			continue
		}

		// 检查当前节点和预计算的输出列表
		if current.isEnd {
			wordRunes := []rune(current.word)
			startPos := pos - len(wordRunes) + 1
			return &core.SensitiveWord{
				Word:     current.word,
				StartPos: startPos,
				EndPos:   pos + 1,
				Category: current.category,
			}
		}

		for _, outputNode := range current.output {
			if outputNode.isEnd {
				wordRunes := []rune(outputNode.word)
				startPos := pos - len(wordRunes) + 1
				return &core.SensitiveWord{
					Word:     outputNode.word,
					StartPos: startPos,
					EndPos:   pos + 1,
					Category: outputNode.category,
				}
			}
		}
	}

	return nil
}

// MatchWithDistance 使用最大距离查找文本中的第一个匹配（支持特殊字符插入检测）
func (ac *AhoCorasick) MatchWithDistance(text string, maxDistance int) *core.SensitiveWord {
	if !ac.built {
		ac.buildFailureLinks()
	}

	if maxDistance <= 0 {
		return ac.Match(text)
	}

	runes := []rune(text)

	// 使用滑动窗口优化：记录每个位置的匹配状态
	current := ac.root
	distanceCount := 0

	for pos, char := range runes {
		// 尝试直接匹配
		if next, exists := current.children[char]; exists {
			current = next
			distanceCount = 0 // 匹配成功，重置距离计数器
		} else {
			// 不匹配，增加距离计数器
			distanceCount++
			if distanceCount > maxDistance {
				// 重置到根节点，继续
				current = ac.root
				distanceCount = 0
				continue
			}
			// 保持当前状态，继续下一个字符
		}

		// 检查当前节点和失败链上的所有匹配
		for node := current; node != ac.root; node = node.failLink {
			if node.isEnd {
				wordRunes := []rune(node.word)
				startPos := pos - len(wordRunes) + 1
				if startPos >= 0 {
					return &core.SensitiveWord{
						Word:     node.word,
						StartPos: startPos,
						EndPos:   pos + 1,
						Category: node.category,
					}
				}
			}
		}
	}

	return nil
}

// MatchAll 返回文本中所有敏感词
func (ac *AhoCorasick) MatchAll(text string) []core.SensitiveWord {
	return ac.MatchAllWithDistance(text, 0)
}

// MatchAllWithDistance 使用最大距离返回文本中所有敏感词
func (ac *AhoCorasick) MatchAllWithDistance(text string, maxDistance int) []core.SensitiveWord {
	if !ac.built {
		ac.buildFailureLinks()
	}

	if maxDistance <= 0 {
		// 使用原始匹配方法
		//// 预分配匹配结果切片，减少动态扩容
		runes := []rune(text)
		matches := make([]core.SensitiveWord, 0, len(runes)/10) // 预估每10个字符一个匹配
		current := ac.root
		var startPos int
		var wordRunes []rune

		for pos, char := range runes {
			// 查找下一个状态
			for current != ac.root && current.children[char] == nil {
				current = current.failLink
			}

			if next, exists := current.children[char]; exists {
				current = next
			} else {
				continue
			}

			// 检查当前节点的所有匹配
			for node := current; node != ac.root; node = node.failLink {
				if node.isEnd {
					wordRunes = []rune(node.word)
					startPos = pos - len(wordRunes) + 1
					matches = append(matches, core.SensitiveWord{
						Word:     node.word,
						StartPos: startPos,
						EndPos:   pos + 1,
						Category: node.category,
					})
				}
			}
		}
		return matches
	}

	// 使用距离匹配方法
	// 预分配匹配结果切片，减少动态扩容
	runes := []rune(text)
	n := len(runes)
	matches := make([]core.SensitiveWord, 0, n/10) // 预估每10个字符一个匹配

	// 预分配变量，减少循环内的内存分配
	var current *AhoCorasickNode
	var distanceCount int
	var char rune

	// 遍历文本
	for startPos := 0; startPos < n; startPos++ {
		// 从当前位置开始，尝试匹配所有可能的敏感词
		current = ac.root
		distanceCount = 0

		for endPos := startPos; endPos < n; endPos++ {
			char = runes[endPos]

			// 查找下一个状态
			for current != ac.root && current.children[char] == nil {
				// 失败指针查找不消耗距离配额
				current = current.failLink
			}

			if next, exists := current.children[char]; exists {
				current = next
				// 匹配成功，重置distance计数器
				distanceCount = 0
			} else {
				// 不匹配，增加distance计数器
				distanceCount++
				if distanceCount > maxDistance {
					break
				}
				continue
			}

			// 检查当前节点和预计算的输出列表
			if current.isEnd {
				wordLen := len([]rune(current.word))
				actualStartPos := endPos - wordLen + 1
				if actualStartPos < 0 {
					actualStartPos = 0
				}
				matches = append(matches, core.SensitiveWord{
					Word:     current.word,
					StartPos: actualStartPos,
					EndPos:   endPos + 1,
					Category: current.category,
				})
			}

			for _, outputNode := range current.output {
				if outputNode.isEnd {
					wordLen := len([]rune(outputNode.word))
					actualStartPos := endPos - wordLen + 1
					if actualStartPos < 0 {
						actualStartPos = 0
					}
					matches = append(matches, core.SensitiveWord{
						Word:     outputNode.word,
						StartPos: actualStartPos,
						EndPos:   endPos + 1,
						Category: outputNode.category,
					})
				}
			}
		}
	}

	return matches
}

// Replace 替换敏感词
func (ac *AhoCorasick) Replace(text string, replacement rune) string {
	matches := ac.MatchAll(text)
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
func (ac *AhoCorasick) Detect(text string) bool {
	return ac.Match(text) != nil
}

// OnWordsChanged 实现 Observer 接口,当词库变更时重建算法
func (ac *AhoCorasick) OnWordsChanged(words map[string]category.Category) {
	if err := ac.Build(words); err != nil {
		// 这里只能记录错误,因为是回调方法
		log.Printf("重建算法失败: %v", err)
	}
}
