package similar

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetSimilarChars(t *testing.T) {
	tests := []struct {
		name   string
		char   rune
		expect []rune
	}{
		{
			name:   "test similar chars for 几",
			char:   '几',
			expect: []rune{'幾'},
		},
		{
			name:   "test similar chars for 门",
			char:   '门',
			expect: []rune{'門'},
		},
		{
			name:   "test similar chars for 0",
			char:   '0',
			expect: []rune{'O', 'o', '〇', '零'},
		},
		{
			name:   "test non-existent similar char",
			char:   '测',
			expect: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetSimilarChars(tt.char)
			assert.Equal(t, tt.expect, result)
		})
	}
}

func TestIsSimilar(t *testing.T) {
	tests := []struct {
		name   string
		char1  rune
		char2  rune
		expect bool
	}{
		{
			name:   "几 and 幾 are similar",
			char1:  '几',
			char2:  '幾',
			expect: true,
		},
		{
			name:   "门 and 門 are similar",
			char1:  '门',
			char2:  '門',
			expect: true,
		},
		{
			name:   "0 and O are similar",
			char1:  '0',
			char2:  'O',
			expect: true,
		},
		{
			name:   "测 and 试 are not similar",
			char1:  '测',
			char2:  '试',
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsSimilar(tt.char1, tt.char2)
			assert.Equal(t, tt.expect, result)
		})
	}
}
