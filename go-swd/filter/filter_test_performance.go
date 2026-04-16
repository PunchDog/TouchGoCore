package filter

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/detector"
	"touchgocore/go-swd/types/category"
	"touchgocore/util"
)

// TestFilterCorrectness 测试过滤器的正确性
func TestFilterCorrectness(t *testing.T) {
	// 创建检测器
	d, err := detector.NewDetector(&core.SWDOptions{
		IgnoreCase:     true,
		IgnoreWidth:    true,
		IgnoreNumStyle: true,
		MaxDistance:    2,
	})
	if err != nil {
		t.Fatalf("创建检测器失败: %v", err)
	}

	// 创建过滤器
	f := NewFilter(d)

	// 测试数据
	testCases := []struct {
		name           string
		text           string
		expectedResult string
	}{{
		name:           "基本替换",
		text:           "这是一个赌博的例子",
		expectedResult: "这是一个****的例子",
	}, {
		name:           "大小写不敏感",
		text:           "这是一个GAMBLING的例子",
		expectedResult: "这是一个********的例子",
	}, {
		name:           "全半角不敏感",
		text:           "这是一个ｇａｍｂｌｉｎｇ的例子",
		expectedResult: "这是一个**********的例子",
	}, {
		name:           "特殊字符插入",
		text:           "这是一个赌*博的例子",
		expectedResult: "这是一个****的例子",
	}, {
		name:           "多个敏感词",
		text:           "这是一个赌博和毒品的例子",
		expectedResult: "这是一个****和****的例子",
	}, {
		name:           "重叠敏感词",
		text:           "这是一个赌博博彩的例子",
		expectedResult: "这是一个********的例子",
	}, {
		name:           "自定义替换策略",
		text:           "这是一个赌博的例子",
		expectedResult: "这是一个[Gambling]的例子",
	}}

	// 执行测试
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var result string
			if tc.name == "自定义替换策略" {
				result = f.ReplaceWithStrategy(tc.text, func(word core.SensitiveWord) string {
					return fmt.Sprintf("[%s]", word.Category)
				})
			} else {
				result = f.ReplaceWithAsterisk(tc.text)
			}

			if result != tc.expectedResult {
				t.Errorf("替换失败: 期望 %q, 实际 %q", tc.expectedResult, result)
			}
		})
	}
}

// TestFilterPerformance 测试过滤器的性能
func TestFilterPerformance(t *testing.T) {
	// 创建检测器
	d, err := detector.NewDetector(&core.SWDOptions{
		IgnoreCase:     true,
		IgnoreWidth:    true,
		IgnoreNumStyle: true,
		MaxDistance:    2,
	})
	if err != nil {
		t.Fatalf("创建检测器失败: %v", err)
	}

	// 创建过滤器
	f := NewFilter(d)

	// 测试不同长度的文本
	textLengths := []int{100, 1000, 10000}
	for _, length := range textLengths {
		t.Run(fmt.Sprintf("文本长度%d", length), func(t *testing.T) {
			// 生成测试文本
			text := generateTestText(length)

			// 测量替换时间
			start := util.CurrentTime()
			result := f.ReplaceWithAsterisk(text)
			elapsed := time.Since(start)

			t.Logf("处理长度为%d的文本，耗时: %v", length, elapsed)
			t.Logf("替换前长度: %d, 替换后长度: %d", len(text), len(result))
		})
	}

	// 测试并发性能
	t.Run("并发性能", func(t *testing.T) {
		concurrency := 10
		text := generateTestText(1000)

		start := util.CurrentTime()
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					f.ReplaceWithAsterisk(text)
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)

		t.Logf("%d个并发处理100次，总耗时: %v", concurrency, elapsed)
		t.Logf("平均每次处理耗时: %v", elapsed/time.Duration(concurrency*100))
	})
}

// generateTestText 生成指定长度的测试文本
func generateTestText(length int) string {
	baseText := "这是一个包含赌博和毒品的测试文本，用于测试敏感词过滤性能。"
	result := ""
	for len(result) < length {
		result += baseText
	}
	if len(result) > length {
		result = result[:length]
	}
	return result
}

// TestFilterWithCategories 测试按分类过滤
func TestFilterWithCategories(t *testing.T) {
	// 创建检测器
	d, err := detector.NewDetector(&core.SWDOptions{
		IgnoreCase:     true,
		IgnoreWidth:    true,
		IgnoreNumStyle: true,
		MaxDistance:    2,
	})
	if err != nil {
		t.Fatalf("创建检测器失败: %v", err)
	}

	// 创建过滤器
	f := NewFilter(d)

	// 测试数据
	testCases := []struct {
		name           string
		text           string
		categories     []category.Category
		expectedResult string
	}{{
		name:           "只过滤赌博",
		text:           "这是一个赌博和毒品的例子",
		categories:     []category.Category{category.Gambling},
		expectedResult: "这是一个****和毒品的例子",
	}, {
		name:           "只过滤毒品",
		text:           "这是一个赌博和毒品的例子",
		categories:     []category.Category{category.Drugs},
		expectedResult: "这是一个赌博和****的例子",
	}, {
		name:           "过滤赌博和毒品",
		text:           "这是一个赌博和毒品的例子",
		categories:     []category.Category{category.Gambling, category.Drugs},
		expectedResult: "这是一个****和****的例子",
	}}

	// 执行测试
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := f.ReplaceWithAsteriskIn(tc.text, tc.categories...)
			if result != tc.expectedResult {
				t.Errorf("按分类过滤失败: 期望 %q, 实际 %q", tc.expectedResult, result)
			}
		})
	}
}
