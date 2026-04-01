package preprocessor

import (
	"testing"

	"touchgocore/go-swd/core"
	"github.com/stretchr/testify/assert"
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
