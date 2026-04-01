package dictionary

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"touchgocore/go-swd/types/category"
)

// Persistence 词库持久化接口
type Persistence interface {
	SaveToFile(path string) error
	LoadFromFile(path string) error
	SaveToJSON(path string) error
	LoadFromJSON(path string) error
}

// WordData 词库数据结构（用于序列化）
type WordData struct {
	Words     map[string]category.Category `json:"words"`
	Version   string                    `json:"version"`
	Timestamp int64                     `json:"timestamp"`
}

// WordPersistence 词库持久化实现
type WordPersistence struct {
	loader *Loader
	mu     sync.Mutex
}

// NewWordPersistence 创建新的持久化实例
func NewWordPersistence(loader *Loader) *WordPersistence {
	return &WordPersistence{
		loader: loader,
	}
}

// SaveToFile 保存词库到文件（JSON格式）
func (wp *WordPersistence) SaveToFile(path string) error {
	return wp.SaveToJSON(path)
}

// SaveToJSON 保存词库到JSON文件
func (wp *WordPersistence) SaveToJSON(path string) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// 确保目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	// 获取当前词库
	words := wp.loader.GetWords()

	// 构建数据结构
	data := WordData{
		Words:     words,
		Version:    "1.0",
		Timestamp:  0, // 可以添加时间戳
	}

	// 序列化为JSON
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化词库失败: %w", err)
	}

	// 写入文件
	if err := os.WriteFile(path, jsonData, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	return nil
}

// LoadFromFile 从文件加载词库（JSON格式）
func (wp *WordPersistence) LoadFromFile(path string) error {
	return wp.LoadFromJSON(path)
}

// LoadFromJSON 从JSON文件加载词库
func (wp *WordPersistence) LoadFromJSON(path string) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// 读取文件
	jsonData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取文件失败: %w", err)
	}

	// 反序列化
	var data WordData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return fmt.Errorf("反序列化词库失败: %w", err)
	}

	// 清空现有词库
	if err := wp.loader.Clear(); err != nil {
		return fmt.Errorf("清空现有词库失败: %w", err)
	}

	// 加载新词库
	if err := wp.loader.AddWords(data.Words); err != nil {
		return fmt.Errorf("加载词库失败: %w", err)
	}

	return nil
}

// ExportToText 导出词库到纯文本格式（每行一个词）
func (wp *WordPersistence) ExportToText(path string, categories ...category.Category) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// 确保目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	// 获取词库
	allWords := wp.loader.GetWords()

	// 过滤指定分类
	var wordsToExport []string
	if len(categories) == 0 {
		// 导出所有词
		for word := range allWords {
			wordsToExport = append(wordsToExport, word)
		}
	} else {
		// 导出指定分类的词
		for word, cat := range allWords {
			for _, filterCat := range categories {
				if cat.Contains(filterCat) {
					wordsToExport = append(wordsToExport, word)
					break
				}
			}
		}
	}

	// 写入文件
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer file.Close()

	for _, word := range wordsToExport {
		if _, err := file.WriteString(word + "\n"); err != nil {
			return fmt.Errorf("写入文件失败: %w", err)
		}
	}

	return nil
}

// ImportFromText 从纯文本格式导入词库
func (wp *WordPersistence) ImportFromText(path string, defaultCat category.Category) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// 读取文件
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 批量加载
	const batchSize = 1000
	words := make(map[string]category.Category, batchSize)
	count := 0

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		word := scanner.Text()
		word = strings.TrimSpace(word)
		if word != "" {
			words[word] = defaultCat
			count++
			if count >= batchSize {
				if err := wp.loader.AddWords(words); err != nil {
					return err
				}
				words = make(map[string]category.Category, batchSize)
				count = 0
			}
		}
	}

	// 加载剩余的词
	if len(words) > 0 {
		if err := wp.loader.AddWords(words); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// GetStats 获取词库统计信息
func (wp *WordPersistence) GetStats() WordStats {
	words := wp.loader.GetWords()

	stats := WordStats{
		TotalWords: len(words),
		ByCategory: make(map[category.Category]int),
	}

	for _, cat := range words {
		stats.ByCategory[cat]++
	}

	return stats
}

// WordStats 词库统计信息
type WordStats struct {
	TotalWords  int                       // 总词数
	ByCategory  map[category.Category]int // 按分类统计
}
