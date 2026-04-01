package filter

import (
	"testing"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/detector"
	"touchgocore/go-swd/types/category"
)

// TestFilterCorrectnessAfterOptimization 测试优化后的过滤器正确性
func TestFilterCorrectnessAfterOptimization(t *testing.T) {
	// 测试英文分类名称方法
	t.Run("测试英文分类名称方法", func(t *testing.T) {
		// 测试 Gambling 分类
		gamblingCategory := category.Gambling
		if gamblingCategory.EnglishString() != "Gambling" {
			t.Errorf("期望 Gambling.EnglishString() 返回 'Gambling'，实际返回 '%s'", gamblingCategory.EnglishString())
		} else {
			t.Logf("Gambling.EnglishString() 测试通过: %s", gamblingCategory.EnglishString())
		}

		// 测试 Drugs 分类
		drugsCategory := category.Drugs
		if drugsCategory.EnglishString() != "Drugs" {
			t.Errorf("期望 Drugs.EnglishString() 返回 'Drugs'，实际返回 '%s'", drugsCategory.EnglishString())
		} else {
			t.Logf("Drugs.EnglishString() 测试通过: %s", drugsCategory.EnglishString())
		}

		// 测试其他分类
		politicalCategory := category.Political
		if politicalCategory.EnglishString() != "Political" {
			t.Errorf("期望 Political.EnglishString() 返回 'Political'，实际返回 '%s'", politicalCategory.EnglishString())
		} else {
			t.Logf("Political.EnglishString() 测试通过: %s", politicalCategory.EnglishString())
		}

		violenceCategory := category.Violence
		if violenceCategory.EnglishString() != "Violence" {
			t.Errorf("期望 Violence.EnglishString() 返回 'Violence'，实际返回 '%s'", violenceCategory.EnglishString())
		} else {
			t.Logf("Violence.EnglishString() 测试通过: %s", violenceCategory.EnglishString())
		}
	})
}

// TestFilterWithCategoriesAfterOptimization 测试按分类过滤
func TestFilterWithCategoriesAfterOptimization(t *testing.T) {
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
		text:           "这是一个赌钱和制毒的例子",
		categories:     []category.Category{category.Gambling},
		expectedResult: "这是一个****和制毒的例子",
	}, {
		name:           "只过滤毒品",
		text:           "这是一个赌钱和制毒的例子",
		categories:     []category.Category{category.Drugs},
		expectedResult: "这是一个赌钱和****的例子",
	}, {
		name:           "过滤赌博和毒品",
		text:           "这是一个赌钱和制毒的例子",
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
