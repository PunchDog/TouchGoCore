package homophone

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetHomophones(t *testing.T) {
	tests := []struct {
		name  string
		char  rune
		expect []rune
	}{
		{
			name:  "test homophones for 发",
			char:  '发',
			expect: []rune{'法', '罚', '乏', '伐', '筏'},
		},
		{
			name:  "test homophones for 赌",
			char:  '赌',
			expect: []rune{'毒', '度', '读', '独', '堵'},
		},
		{
			name:  "test non-existent homophone",
			char:  '测',
			expect: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetHomophones(tt.char)
			assert.Equal(t, tt.expect, result)
		})
	}
}

func TestIsHomophone(t *testing.T) {
	tests := []struct {
		name   string
		char1  rune
		char2  rune
		expect bool
	}{
		{
			name:   "发 and 法 are homophones",
			char1:  '发',
			char2:  '法',
			expect: true,
		},
		{
			name:   "赌 and 毒 are homophones",
			char1:  '赌',
			char2:  '毒',
			expect: true,
		},
		{
			name:   "测 and 试 are not homophones",
			char1:  '测',
			char2:  '试',
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsHomophone(tt.char1, tt.char2)
			assert.Equal(t, tt.expect, result)
		})
	}
}
