package pinyin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetCharsFromPinyin(t *testing.T) {
	tests := []struct {
		name   string
		pinyin string
		expect []string
	}{
		{
			name:   "test fa pinyin",
			pinyin: "fa",
			expect: []string{"发", "法", "罚", "乏", "伐", "筏"},
		},
		{
			name:   "test piao pinyin",
			pinyin: "piao",
			expect: []string{"票", "飘", "飘", "瓢", "嫖"},
		},
		{
			name:   "test non-existent pinyin",
			pinyin: "xyz",
			expect: nil,
		},
		{
			name:   "test empty pinyin",
			pinyin: "",
			expect: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetCharsFromPinyin(tt.pinyin)
			assert.Equal(t, tt.expect, result)
		})
	}
}

func TestContainsPinyin(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		pinyin string
		expect bool
	}{
		{
			name:   "text contains pinyin",
			text:   "fa票",
			pinyin: "fa",
			expect: true,
		},
		{
			name:   "text does not contain pinyin",
			text:   "测试文本",
			pinyin: "fa",
			expect: false,
		},
		{
			name:   "case insensitive",
			text:   "FA票",
			pinyin: "fa",
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ContainsPinyin(tt.text, tt.pinyin)
			assert.Equal(t, tt.expect, result)
		})
	}
}
