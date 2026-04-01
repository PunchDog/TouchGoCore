package common

import (
	"sync"
)

// StringPool 字符串池，用于减少重复字符串的内存占用
type StringPool struct {
	mu     sync.RWMutex
	strings map[string]string
}

// NewStringPool 创建新的字符串池
func NewStringPool() *StringPool {
	return &StringPool{
		strings: make(map[string]string),
	}
}

// Intern 返回字符串的规范化版本（去重）
// 如果字符串已存在于池中，返回池中的引用；否则添加到池中并返回
func (sp *StringPool) Intern(s string) string {
	// 短字符串直接返回，避免池的开销
	if len(s) < 8 {
		return s
	}

	sp.mu.RLock()
	existing, ok := sp.strings[s]
	sp.mu.RUnlock()

	if ok {
		return existing
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()

	// 双重检查
	if existing, ok := sp.strings[s]; ok {
		return existing
	}

	sp.strings[s] = s
	return s
}

// Clear 清空字符串池，释放内存
func (sp *StringPool) Clear() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.strings = make(map[string]string)
}

// Size 返回字符串池中的字符串数量
func (sp *StringPool) Size() int {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	return len(sp.strings)
}

// Stats 返回字符串池的统计信息
func (sp *StringPool) Stats() StringPoolStats {
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	totalLength := 0
	for s := range sp.strings {
		totalLength += len(s)
	}

	return StringPoolStats{
		Count:      len(sp.strings),
		TotalLength: totalLength,
		AvgLength:  float64(totalLength) / float64(len(sp.strings)),
	}
}

// StringPoolStats 字符串池统计信息
type StringPoolStats struct {
	Count      int     // 字符串数量
	TotalLength int     // 总长度（字节）
	AvgLength  float64 // 平均长度
}
