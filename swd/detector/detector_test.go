package detector

import (
	"testing"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

func TestDetector_Detect(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		options  core.SWDOptions
		expected bool
	}{
		{
			name: "空文本检测",
			text: "",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name: "基本敏感词检测",
			text: "这是一段包含色情的文本",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
		{
			name: "不包含敏感词",
			text: "这是一段正常的文本",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name: "大小写混合检测",
			text: "这是一段包含SeQiNg的文本",
			options: core.SWDOptions{
				IgnoreCase:     true,
				SkipWhitespace: true,
			},
			expected: true,
		},
		{
			name: "全角半角混合检测",
			text: "这是一段包含ｓｅｑｉｎｇ的文本",
			options: core.SWDOptions{
				IgnoreCase:     true,
				SkipWhitespace: true,
				IgnoreWidth:    true,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.Detect(tt.text)
			if got != tt.expected {
				t.Errorf("Detect() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}

func TestDetector_DetectIn(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		categories []category.Category
		options    core.SWDOptions
		expected   bool
	}{
		{
			name: "涉黄分类检测",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
		{
			name: "涉政分类检测",
			text: "这是一段包含政府的文本",
			categories: []category.Category{
				category.Political,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
		{
			name: "错误分类检测",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.Political,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name: "All分类-检测单个分类",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
		{
			name: "All分类-检测多个分类",
			text: "这是一段包含色情和政府的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
		{
			name: "All分类-检测不包含敏感词",
			text: "这是一段正常的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name: "All分类-检测空文本",
			text: "",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.DetectIn(tt.text, tt.categories...)
			if got != tt.expected {
				t.Errorf("DetectIn() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}

func TestDetector_Match(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		options  core.SWDOptions
		expected *core.SensitiveWord
	}{
		{
			name: "空文本匹配",
			text: "",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name: "基本敏感词匹配",
			text: "这是一段包含色情的文本",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: &core.SensitiveWord{
				Word:     "色情",
				Category: category.Pornography,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.Match(tt.text)
			if (got == nil) != (tt.expected == nil) {
				t.Errorf("Match() = %v, 期望 %v", got, tt.expected)
			}
			if got != nil && tt.expected != nil {
				if got.Word != tt.expected.Word || got.Category != tt.expected.Category {
					t.Errorf("Match() = {Word: %v, Category: %v}, 期望 {Word: %v, Category: %v}",
						got.Word, got.Category, tt.expected.Word, tt.expected.Category)
				}
			}
		})
	}
}

func TestDetector_MatchIn(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		categories []category.Category
		options    core.SWDOptions
		expected   *core.SensitiveWord
	}{
		{
			name: "空文本匹配",
			text: "",
			categories: []category.Category{
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name: "基本敏感词匹配",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: &core.SensitiveWord{
				Word:     "色情",
				Category: category.Pornography,
			},
		},
		{
			name: "错误分类匹配",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.Political,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name: "All分类-匹配单个分类",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: &core.SensitiveWord{
				Word:     "色情",
				Category: category.Pornography,
			},
		},
		{
			name: "All分类-匹配多个分类",
			text: "这是一段包含色情和政府的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: &core.SensitiveWord{
				Word:     "色情",
				Category: category.Pornography,
			},
		},
		{
			name: "All分类-匹配不包含敏感词",
			text: "这是一段正常的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.MatchIn(tt.text, tt.categories...)
			if (got == nil) != (tt.expected == nil) {
				t.Errorf("MatchIn() = %v, 期望 %v", got, tt.expected)
			}
			if got != nil && tt.expected != nil {
				if got.Word != tt.expected.Word || got.Category != tt.expected.Category {
					t.Errorf("MatchIn() = {Word: %v, Category: %v}, 期望 {Word: %v, Category: %v}",
						got.Word, got.Category, tt.expected.Word, tt.expected.Category)
				}
			}
		})
	}
}

func TestDetector_MatchAll(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		options  core.SWDOptions
		expected []core.SensitiveWord
	}{
		{
			name: "空文本匹配",
			text: "",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name: "单个敏感词匹配",
			text: "这是一段包含色情的文本",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: []core.SensitiveWord{
				{
					Word:     "色情",
					Category: category.Pornography,
				},
			},
		},
		{
			name: "多个敏感词匹配",
			text: "这是一段包含色情和政府的文本",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: []core.SensitiveWord{
				{
					Word:     "色情",
					Category: category.Pornography,
				},
				{
					Word:     "政府",
					Category: category.Political,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.MatchAll(tt.text)
			if len(got) != len(tt.expected) {
				t.Errorf("MatchAll() 返回长度 = %v, 期望长度 %v", len(got), len(tt.expected))
				return
			}

			for i := range got {
				if got[i].Word != tt.expected[i].Word || got[i].Category != tt.expected[i].Category {
					t.Errorf("MatchAll()[%d] = {Word: %v, Category: %v}, 期望 {Word: %v, Category: %v}",
						i, got[i].Word, got[i].Category, tt.expected[i].Word, tt.expected[i].Category)
				}
			}
		})
	}
}

func TestDetector_MatchAllIn(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		categories []category.Category
		options    core.SWDOptions
		expected   []core.SensitiveWord
	}{
		{
			name: "空文本匹配",
			text: "",
			categories: []category.Category{
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name:       "空分类匹配",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
		{
			name: "单个分类多个敏感词匹配",
			text: "这是一段包含seqing和色情的文本",
			categories: []category.Category{
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: []core.SensitiveWord{
				{
					Word:     "seqing",
					Category: category.Pornography,
				},
				{
					Word:     "色情",
					Category: category.Pornography,
				},
			},
		},
		{
			name: "All分类-匹配单个分类多个词",
			text: "这是一段包含seqing和色情的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: []core.SensitiveWord{
				{
					Word:     "seqing",
					Category: category.Pornography,
				},
				{
					Word:     "色情",
					Category: category.Pornography,
				},
			},
		},
		{
			name: "All分类-匹配多个分类",
			text: "这是一段包含色情和政府的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: []core.SensitiveWord{
				{
					Word:     "色情",
					Category: category.Pornography,
				},
				{
					Word:     "政府",
					Category: category.Political,
				},
			},
		},
		{
			name: "All分类-匹配不包含敏感词",
			text: "这是一段正常的文本",
			categories: []category.Category{
				category.All,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.MatchAllIn(tt.text, tt.categories...)
			if len(got) != len(tt.expected) {
				t.Errorf("MatchAllIn() 返回长度 = %v, 期望长度 %v", len(got), len(tt.expected))
				return
			}

			for i := range got {
				if got[i].Word != tt.expected[i].Word || got[i].Category != tt.expected[i].Category {
					t.Errorf("MatchAllIn()[%d] = {Word: %v, Category: %v}, 期望 {Word: %v, Category: %v}",
						i, got[i].Word, got[i].Category, tt.expected[i].Word, tt.expected[i].Category)
				}
			}
		})
	}
}

func TestDetector_DetectIn_EdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		categories []category.Category
		options    core.SWDOptions
		expected   bool
	}{
		{
			name:       "空分类检测",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name:       "nil分类检测",
			text:       "这是一段包含色情的文本",
			categories: nil,
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: false,
		},
		{
			name: "多个分类包含目标分类",
			text: "这是一段包含色情的文本",
			categories: []category.Category{
				category.Political,
				category.Pornography,
			},
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDetector(&tt.options)
			if err != nil {
				t.Fatalf("创建检测器失败: %v", err)
			}

			got := d.DetectIn(tt.text, tt.categories...)
			if got != tt.expected {
				t.Errorf("DetectIn() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}
