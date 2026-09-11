package algorithm

import (
	"reflect"
	"testing"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

// algorithmTest 定义测试用例结构
type algorithmTest struct {
	name     string
	words    map[string]category.Category
	text     string
	expected interface{}
}

// getAlgorithms 返回所有需要测试的算法实现
func getAlgorithms(t *testing.T) []core.Algorithm {
	return []core.Algorithm{
		NewTrie(),
		NewAhoCorasick(),
	}
}

// TestType 测试算法类型
func TestType(t *testing.T) {
	tests := []struct {
		algorithm core.Algorithm
		expected  core.AlgorithmType
	}{
		{NewTrie(), core.AlgorithmTrie},
		{NewAhoCorasick(), core.AlgorithmAhoCorasick},
	}

	for _, tt := range tests {
		t.Run(string(tt.expected), func(t *testing.T) {
			if got := tt.algorithm.Type(); got != tt.expected {
				t.Errorf("Type() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestBuild 测试构建词库
func TestBuild(t *testing.T) {
	words := map[string]category.Category{
		"Pornography": category.Pornography,
		"Political":   category.Political,
	}

	for _, alg := range getAlgorithms(t) {
		t.Run(string(alg.Type()), func(t *testing.T) {
			if err := alg.Build(words); err != nil {
				t.Errorf("Build() error = %v", err)
			}
		})
	}
}

// TestDetect 测试敏感词检测
func TestDetect(t *testing.T) {
	tests := []algorithmTest{
		{
			name: "包含敏感词",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text:     "这是一段包含敏感词的文本",
			expected: true,
		},
		{
			name: "不包含敏感词",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text:     "这是一段正常的文本",
			expected: false,
		},
	}

	for _, alg := range getAlgorithms(t) {
		for _, tt := range tests {
			t.Run(string(alg.Type())+"_"+tt.name, func(t *testing.T) {
				if err := alg.Build(tt.words); err != nil {
					t.Fatalf("Build() error = %v", err)
				}
				if got := alg.Detect(tt.text); got != tt.expected {
					t.Errorf("Detect() = %v, want %v", got, tt.expected)
				}
			})
		}
	}
}

// TestMatch 测试单个敏感词匹配
func TestMatch(t *testing.T) {
	tests := []algorithmTest{
		{
			name: "匹配单个敏感词",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text: "这是一段包含敏感词的文本",
			expected: &core.SensitiveWord{
				Word:     "敏感",
				StartPos: 6,
				EndPos:   8,
				Category: category.Pornography,
			},
		},
		{
			name: "无匹配",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text:     "这是一段正常的文本",
			expected: (*core.SensitiveWord)(nil),
		},
	}

	for _, alg := range getAlgorithms(t) {
		for _, tt := range tests {
			t.Run(string(alg.Type())+"_"+tt.name, func(t *testing.T) {
				if err := alg.Build(tt.words); err != nil {
					t.Fatalf("Build() error = %v", err)
				}
				got := alg.Match(tt.text)
				if !reflect.DeepEqual(got, tt.expected) {
					t.Errorf("Match() = %v, want %v", got, tt.expected)
				}
			})
		}
	}
}

// TestMatchAll 测试多个敏感词匹配
func TestMatchAll(t *testing.T) {
	tests := []algorithmTest{
		{
			name: "匹配多个敏感词",
			words: map[string]category.Category{
				"敏感": category.Pornography,
				"词":  category.Political,
			},
			text: "这是一段包含敏感词的文本",
			expected: []core.SensitiveWord{
				{
					Word:     "敏感",
					StartPos: 6,
					EndPos:   8,
					Category: category.Pornography,
				},
				{
					Word:     "词",
					StartPos: 8,
					EndPos:   9,
					Category: category.Political,
				},
			},
		},
		{
			name: "无匹配",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text:     "这是一段正常的文本",
			expected: []core.SensitiveWord{},
		},
	}

	for _, alg := range getAlgorithms(t) {
		for _, tt := range tests {
			t.Run(string(alg.Type())+"_"+tt.name, func(t *testing.T) {
				if err := alg.Build(tt.words); err != nil {
					t.Fatalf("Build() error = %v", err)
				}
				got := alg.MatchAll(tt.text)
				if expected, ok := tt.expected.([]core.SensitiveWord); ok {
					if len(got) == 0 && len(expected) == 0 {
						return // 两者都为空，测试通过
					}
					if !reflect.DeepEqual(got, expected) {
						t.Errorf("MatchAll() = %v, want %v", got, expected)
					}
				} else {
					t.Fatalf("tt.expected is not of type []core.SensitiveWord: %v", tt.expected)
				}
			})
		}
	}
}

// TestReplace 测试敏感词替换
func TestReplace(t *testing.T) {
	tests := []algorithmTest{
		{
			name: "替换敏感词",
			words: map[string]category.Category{
				"敏感": category.Pornography,
				"词":  category.Political,
			},
			text:     "这是一段包含敏感词的文本",
			expected: "这是一段包含***的文本",
		},
		{
			name: "无需替换",
			words: map[string]category.Category{
				"敏感": category.Pornography,
			},
			text:     "这是一段正常的文本",
			expected: "这是一段正常的文本",
		},
	}

	for _, alg := range getAlgorithms(t) {
		for _, tt := range tests {
			t.Run(string(alg.Type())+"_"+tt.name, func(t *testing.T) {
				if err := alg.Build(tt.words); err != nil {
					t.Fatalf("Build() error = %v", err)
				}
				if got := alg.Replace(tt.text, '*'); got != tt.expected {
					t.Errorf("Replace() = %v, want %v", got, tt.expected)
				}
			})
		}
	}
}
