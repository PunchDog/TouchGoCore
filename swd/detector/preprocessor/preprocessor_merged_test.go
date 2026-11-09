package preprocessor

import (
	"github.com/stretchr/testify/assert"
	"testing"
	"touchgocore/swd/core"
)

func TestPreprocessor_IgnoreCase(t *testing.T) {
	options := core.SWDOptions{
		IgnoreCase: true,
	}
	p := NewPreprocessor(&options)

	text := "FuCk 混淆大小写"
	result := p.Process(text)

	assert.Contains(t, result, "fuck")
}

func TestPreprocessor_IgnoreWidth(t *testing.T) {
	options := core.SWDOptions{
		IgnoreWidth: true,
	}
	p := NewPreprocessor(&options)

	text := "ｆｕｃｋ 全角字符"
	result := p.Process(text)

	assert.Contains(t, result, "fuck")
}

func TestPreprocessor_IgnoreNumStyle(t *testing.T) {
	options := core.SWDOptions{
		IgnoreNumStyle: true,
	}
	p := NewPreprocessor(&options)

	tests := []struct {
		name     string
		input    string
		contains string
	}{
		{
			name:     "fullwidth numbers",
			input:    "１２３４５",
			contains: "12345",
		},
		{
			name:     "circled numbers",
			input:    "①②③",
			contains: "123",
		},
		{
			name:     "chinese numbers",
			input:    "一二三",
			contains: "123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := p.Process(tt.input)
			assert.Contains(t, result, tt.contains)
		})
	}
}

func TestPreprocessor_EnableSimilarShape(t *testing.T) {
	options := core.SWDOptions{
		EnableSimilarShape: true,
	}
	p := NewPreprocessor(&options)

	text := "門 几 幾"
	result := p.Process(text)

	// 形近字应该被转换
	assert.NotContains(t, result, "門")
}

func TestPreprocessor_EnableHomophone(t *testing.T) {
	options := core.SWDOptions{
		EnableHomophone: true,
	}
	p := NewPreprocessor(&options)

	text := "发法"
	result := p.NormalizeHomophone(text)

	// 应该生成同音字变体
	assert.Greater(t, len(result), 1)
	assert.Contains(t, result, text)
}

func TestPreprocessor_EnableZhPYMix(t *testing.T) {
	options := core.SWDOptions{
		EnableZhPYMix: true,
	}
	p := NewPreprocessor(&options)

	text := "fa票"
	result := p.ProcessWithPinyin(text)

	// 应该生成拼音到汉字的变体
	assert.Greater(t, len(result), 1)
}

func TestPreprocessor_Process(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		options  core.SWDOptions
		expected string
	}{
		{
			name: "空文本处理",
			text: "",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: "",
		},
		{
			name: "大小写转换",
			text: "Hello World",
			options: core.SWDOptions{
				IgnoreCase: true,
			},
			expected: "hello world",
		},
		{
			name: "空白字符处理",
			text: "Hello  World",
			options: core.SWDOptions{
				SkipWhitespace: true,
			},
			expected: "HelloWorld",
		},
		{
			name: "全角半角转换",
			text: "Ｈello",
			options: core.SWDOptions{
				IgnoreWidth: true,
			},
			expected: "Hello",
		},
		{
			name: "数字样式统一",
			text: "123４５六七",
			options: core.SWDOptions{
				IgnoreNumStyle: true,
			},
			expected: "1234567",
		},
		{
			name: "组合处理",
			text: "Ｈello  123４５六七",
			options: core.SWDOptions{
				IgnoreCase:     true,
				SkipWhitespace: true,
				IgnoreWidth:    true,
				IgnoreNumStyle: true,
			},
			expected: "hello1234567",
		},
		{
			name: "中文数字处理",
			text: "零一二三四五六七八九",
			options: core.SWDOptions{
				IgnoreNumStyle: true,
			},
			expected: "0123456789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPreprocessor(&tt.options)
			got := p.Process(tt.text)
			if got != tt.expected {
				t.Errorf("Process() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}

func TestPreprocessor_normalizeNumber(t *testing.T) {
	tests := []struct {
		name     string
		input    rune
		expected rune
	}{
		{
			name:     "ASCII数字",
			input:    '9',
			expected: '9',
		},
		{
			name:     "全角数字",
			input:    '９',
			expected: '9',
		},
		{
			name:     "带圈数字",
			input:    '⑨',
			expected: '9',
		},
		{
			name:     "中文数字-零",
			input:    '零',
			expected: '0',
		},
		{
			name:     "中文数字-一",
			input:    '一',
			expected: '1',
		},
		{
			name:     "中文数字-九",
			input:    '九',
			expected: '9',
		},
		{
			name:     "非数字字符",
			input:    'A',
			expected: 'A',
		},
	}

	p := NewPreprocessor(&core.SWDOptions{IgnoreNumStyle: true})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.normalizeNumber(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeNumber() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}

func TestIsChineseNumber(t *testing.T) {
	tests := []struct {
		name     string
		input    rune
		expected bool
	}{
		{
			name:     "中文数字-零",
			input:    '零',
			expected: true,
		},
		{
			name:     "中文数字-一",
			input:    '一',
			expected: true,
		},
		{
			name:     "中文数字-九",
			input:    '九',
			expected: true,
		},
		{
			name:     "中文数字-十",
			input:    '十',
			expected: true,
		},
		{
			name:     "非中文数字",
			input:    'A',
			expected: false,
		},
		{
			name:     "ASCII数字",
			input:    '9',
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPreprocessor(&core.SWDOptions{})
			got := p.isChineseNumber(tt.input)
			if got != tt.expected {
				t.Errorf("isChineseNumber() = %v, 期望 %v", got, tt.expected)
			}
		})
	}
}
