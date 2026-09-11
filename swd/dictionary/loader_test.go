package dictionary

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"touchgocore/go-swd/types/category"
	"github.com/stretchr/testify/assert"
)

// TestNewLoader 测试创建新的加载器
func TestNewLoader(t *testing.T) {
	loader := NewLoader()
	assert.NotNil(t, loader)
	words := loader.GetWords()
	assert.NotNil(t, words)
	assert.Equal(t, 0, len(words))
}

// TestLoadDefaultWords 测试加载默认词库
func TestLoadDefaultWords(t *testing.T) {
	loader := NewLoader()
	err := loader.LoadDefaultWords(context.Background())
	assert.NoError(t, err)

	words := loader.GetWords()
	assert.NotEmpty(t, words)

	// 验证不同类别的词是否正确加载
	hasPolitical := false
	hasPornography := false
	hasViolence := false
	for _, cat := range words {
		switch cat {
		case category.Political:
			hasPolitical = true
		case category.Pornography:
			hasPornography = true
		case category.Violence:
			hasViolence = true
		}
	}

	assert.True(t, hasPolitical, "应该包含政治类敏感词")
	assert.True(t, hasPornography, "应该包含色情类敏感词")
	assert.True(t, hasViolence, "应该包含暴力类敏感词")
}

// TestLoadCustomWords 测试加载自定义词库
func TestLoadCustomWords(t *testing.T) {
	loader := NewLoader()
	customWords := map[string]category.Category{
		"测试词1": category.Political,
		"测试词2": category.Pornography,
		"测试词3": category.Violence,
	}

	err := loader.LoadCustomWords(context.Background(), customWords)
	assert.NoError(t, err)

	words := loader.GetWords()
	assert.Len(t, words, len(customWords))

	for word, expectedCat := range customWords {
		actualCat, exists := words[word]
		assert.True(t, exists, "词 %s 应该存在", word)
		assert.Equal(t, expectedCat, actualCat, "词 %s 的类别不匹配", word)
	}
}

// TestAddWord 测试添加单个敏感词
func TestAddWord(t *testing.T) {
	loader := NewLoader()

	// 测试添加有效词
	err := loader.AddWord("测试词", category.Political)
	assert.NoError(t, err)
	words := loader.GetWords()
	assert.Len(t, words, 1)
	assert.Equal(t, category.Political, words["测试词"])

	// 测试添加空字符串
	err = loader.AddWord("", category.Political)
	assert.Error(t, err)

	// 测试添加空白字符
	err = loader.AddWord("   ", category.Political)
	assert.Error(t, err)
}

// TestRemoveWord 测试删除敏感词
func TestRemoveWord(t *testing.T) {
	loader := NewLoader()

	// 添加测试词
	_ = loader.AddWord("测试词", category.Political)

	// 测试删除存在的词
	err := loader.RemoveWord("测试词")
	assert.NoError(t, err)
	assert.Equal(t, 0, len(loader.GetWords()))

	// 测试删除不存在的词
	err = loader.RemoveWord("不存在的词")
	assert.NoError(t, err)
}

// TestClear 测试清空词库
func TestClear(t *testing.T) {
	loader := NewLoader()

	// 添加一些测试词
	_ = loader.AddWord("测试词1", category.Political)
	_ = loader.AddWord("测试词2", category.Pornography)

	// 测试清空
	err := loader.Clear()
	assert.NoError(t, err)
	assert.Equal(t, 0, len(loader.GetWords()))
}

// TestConcurrentOperations 测试并发操作
func TestConcurrentOperations(t *testing.T) {
	loader := NewLoader()
	var wg sync.WaitGroup

	// 并发添加
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			word := fmt.Sprintf("测试词%d", i)
			_ = loader.AddWord(word, category.Political)
		}(i)
	}

	// 并发读取
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = loader.GetWords()
		}()
	}

	wg.Wait()
	words := loader.GetWords()
	assert.Len(t, words, 100)
}

// TestLoadFromString 测试从字符串加载
func TestLoadFromString(t *testing.T) {
	loader := NewLoader()
	content := `测试词1
测试词2
# 这是注释
测试词3
`
	err := loader.loadFromString(context.Background(), content, category.Political)
	assert.NoError(t, err)

	words := loader.GetWords()
	assert.Len(t, words, 3)
	assert.Equal(t, category.Political, words["测试词1"])
	assert.Equal(t, category.Political, words["测试词2"])
	assert.Equal(t, category.Political, words["测试词3"])
}

// TestLoadFromReader 测试从Reader加载
func TestLoadFromReader(t *testing.T) {
	loader := NewLoader()
	content := "测试词1\n测试词2\n"
	reader := strings.NewReader(content)

	err := loader.loadFromReader(context.Background(), reader, category.Political)
	assert.NoError(t, err)

	words := loader.GetWords()
	assert.Len(t, words, 2)
	assert.Equal(t, category.Political, words["测试词1"])
	assert.Equal(t, category.Political, words["测试词2"])
}

// BenchmarkLoadDefaultWords 性能测试 - 加载默认词库
func BenchmarkLoadDefaultWords(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		loader := NewLoader()
		_ = loader.LoadDefaultWords(ctx)
	}
}

// BenchmarkAddWord 性能测试 - 添加单词
func BenchmarkAddWord(b *testing.B) {
	loader := NewLoader()
	for i := 0; i < b.N; i++ {
		word := fmt.Sprintf("测试词%d", i)
		_ = loader.AddWord(word, category.Political)
	}
}

// TestLoadDefaultWordsWithCancel 测试加载默认词库时的上下文取消
func TestLoadDefaultWordsWithCancel(t *testing.T) {
	loader := NewLoader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	err := loader.LoadDefaultWords(ctx)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

// TestLoadCustomWordsWithCancel 测试加载自定义词库时的上下文取消
func TestLoadCustomWordsWithCancel(t *testing.T) {
	loader := NewLoader()
	ctx, cancel := context.WithCancel(context.Background())

	customWords := make(map[string]category.Category)
	for i := 0; i < 2000; i++ { // 创建足够多的词以确保触发批处理
		customWords[fmt.Sprintf("测试词%d", i)] = category.Political
	}

	cancel() // 立即取消
	err := loader.LoadCustomWords(ctx, customWords)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

// TestAddWords 测试批量添加敏感词
func TestAddWords(t *testing.T) {
	loader := NewLoader()

	// 测试正常添加
	words := map[string]category.Category{
		"测试词1": category.Political,
		"测试词2": category.Pornography,
		"测试词3": category.Violence,
	}
	err := loader.AddWords(words)
	assert.NoError(t, err)

	loadedWords := loader.GetWords()
	assert.Equal(t, len(words), len(loadedWords))
	for word, cat := range words {
		assert.Equal(t, cat, loadedWords[word])
	}

	// 测试添加空白词
	invalidWords := map[string]category.Category{
		"测试词4": category.Political,
		"   ":  category.Political,
	}
	err = loader.AddWords(invalidWords)
	assert.Error(t, err)
}

// TestRemoveWords 测试批量删除敏感词
func TestRemoveWords(t *testing.T) {
	loader := NewLoader()

	// 添加测试词
	_ = loader.AddWord("测试词1", category.Political)
	_ = loader.AddWord("测试词2", category.Political)
	_ = loader.AddWord("测试词3", category.Political)

	// 测试删除存在的词
	err := loader.RemoveWords([]string{"测试词1", "测试词2"})
	assert.NoError(t, err)

	words := loader.GetWords()
	assert.Len(t, words, 1)
	_, exists := words["测试词3"]
	assert.True(t, exists)

	// 测试删除不存在的词
	err = loader.RemoveWords([]string{"不存在的词"})
	assert.NoError(t, err)
}

// TestNoneCategoryHandling 测试None分类的特殊处理
func TestNoneCategoryHandling(t *testing.T) {
	loader := NewLoader()

	// 先添加一个有效分类的词
	_ = loader.AddWord("测试词", category.Political)

	// 尝试用None分类覆盖已有词
	err := loader.AddWord("测试词", category.None)
	assert.NoError(t, err)

	// 验证原有分类被保留
	words := loader.GetWords()
	assert.Equal(t, category.Political, words["测试词"])
}

// TestLoadFromReaderError 测试从Reader加载时的错误处理
func TestLoadFromReaderError(t *testing.T) {
	loader := NewLoader()

	// 创建一个会产生错误的Reader
	errReader := &ErrorReader{err: fmt.Errorf("read error")}

	err := loader.loadFromReader(context.Background(), errReader, category.Political)
	assert.Error(t, err)
}

// ErrorReader 用于测试Reader错误的辅助类型
type ErrorReader struct {
	err error
}

func (r *ErrorReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}
