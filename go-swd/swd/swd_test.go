package swd

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
	"touchgocore/util"
)

// TestNew 测试创建SWD实例
func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		factory ComponentFactory
		wantErr bool
	}{
		{
			name:    "nil factory",
			factory: nil,
			wantErr: true,
		},
		{
			name:    "valid factory",
			factory: NewDefaultFactory(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.factory)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got == nil {
				t.Error("New() returned nil but wanted valid SWD instance")
			}
		})
	}
}

// TestSWD_Detect 测试敏感词检测功能
func TestSWD_Detect(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	// 加载默认词库
	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "empty text",
			text: "",
			want: false,
		},
		{
			name: "text with sensitive word (pornography)",
			text: "这是一段包含色情的文本",
			want: true,
		},
		{
			name: "text with sensitive word (gambling)",
			text: "这是一段包含赌博的文本",
			want: true,
		},
		{
			name: "text with sensitive word (drugs)",
			text: "这是一段包含毒品的文本",
			want: true,
		},
		{
			name: "text with sensitive word (scam)",
			text: "这是一段包含诈骗的文本",
			want: true,
		},
		{
			name: "text without sensitive word",
			text: "这是一段正常的文本",
			want: false,
		},
		{
			name: "text with multiple sensitive words",
			text: "这是一段包含色情和暴力的文本",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.Detect(tt.text); got != tt.want {
				t.Errorf("SWD.Detect() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_Replace 测试敏感词替换功能
func TestSWD_Replace(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	// 加载默认词库
	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name        string
		text        string
		replacement rune
		want        string
	}{
		{
			name:        "empty text",
			text:        "",
			replacement: '*',
			want:        "",
		},
		{
			name:        "text with sensitive word (pornography)",
			text:        "这是一段包含色情的文本",
			replacement: '*',
			want:        "这是一段包含**的文本",
		},
		{
			name:        "text with sensitive word (gambling)",
			text:        "这是一段包含赌博的文本",
			replacement: '*',
			want:        "这是一段包含**的文本",
		},
		{
			name:        "text without sensitive word",
			text:        "这是一段正常的文本",
			replacement: '*',
			want:        "这是一段正常的文本",
		},
		{
			name:        "text with multiple sensitive words",
			text:        "这是一段包含色情和暴力的文本",
			replacement: '#',
			want:        "这是一段包含##和##的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.Replace(tt.text, tt.replacement); got != tt.want {
				t.Errorf("SWD.Replace() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_DetectIn 测试指定分类的敏感词检测功能
func TestSWD_DetectIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	// 加载默认词库
	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	// 添加一些测试用的敏感词
	testWords := map[string]category.Category{
		"色情":  category.Pornography,
		"赌博":  category.Gambling,
		"毒品":  category.Drugs,
		"傻逼":  category.Profanity,
		"小日本": category.Discrimination,
		"诈骗":  category.Scam,
		"政府":  category.Political,
	}

	for word, cat := range testWords {
		if err := swd.AddWord(word, cat); err != nil {
			t.Fatalf("Failed to add test word %s: %v", word, err)
		}
	}

	tests := []struct {
		name       string
		text       string
		categories []category.Category
		want       bool
	}{
		{
			name:       "empty text",
			text:       "",
			categories: []category.Category{category.Pornography},
			want:       false,
		},
		{
			name:       "text with pornography word",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Pornography},
			want:       true,
		},
		{
			name:       "text with pornography word",
			text:       "这是一段包含色情的文本，但是分类不正确",
			categories: []category.Category{category.Scam},
			want:       false,
		},
		{
			name:       "text with gambling word",
			text:       "这是一段包含赌博的文本",
			categories: []category.Category{category.Gambling},
			want:       true,
		},
		{
			name:       "text with drugs word",
			text:       "这是一段包含毒品的文本",
			categories: []category.Category{category.Drugs},
			want:       true,
		},
		{
			name:       "text with profanity word",
			text:       "这是一段包含脏话：傻逼的文本",
			categories: []category.Category{category.Profanity},
			want:       true,
		},
		{
			name:       "text with discrimination word",
			text:       "这是一段包含歧视：小日本的文本",
			categories: []category.Category{category.Discrimination},
			want:       true,
		},
		{
			name:       "text with scam word",
			text:       "这是一段包含诈骗的文本",
			categories: []category.Category{category.Scam},
			want:       true,
		},
		{
			name:       "text with wrong category",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Political},
			want:       false,
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和政府的文本",
			categories: []category.Category{category.Pornography, category.Political},
			want:       true,
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和政府的文本",
			categories: []category.Category{category.Political},
			want:       true,
		},
		{
			name:       "text with invalid category",
			text:       "这是一段正常的文本",
			categories: []category.Category{category.Category(1 << 31)},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.DetectIn(tt.text, tt.categories...); got != tt.want {
				t.Errorf("SWD.DetectIn() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCategory_Contains 测试分类包含关系
func TestCategory_Contains(t *testing.T) {
	tests := []struct {
		name     string
		category category.Category
		other    category.Category
		want     bool
	}{
		{
			name:     "same category",
			category: category.Pornography,
			other:    category.Pornography,
			want:     true,
		},
		{
			name:     "different category",
			category: category.Pornography,
			other:    category.Political,
			want:     false,
		},
		{
			name:     "multiple categories contains one",
			category: category.Pornography | category.Political,
			other:    category.Political,
			want:     true,
		},
		{
			name:     "all categories contains one",
			category: category.All,
			other:    category.Pornography,
			want:     true,
		},
		{
			name:     "none category",
			category: category.None,
			other:    category.Pornography,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.category.Contains(tt.other); got != tt.want {
				t.Errorf("Category.Contains() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCategory_String 测试分类名称
func TestCategory_String(t *testing.T) {
	tests := []struct {
		name     string
		category category.Category
		want     string
	}{
		{
			name:     "pornography category",
			category: category.Pornography,
			want:     "涉黄",
		},
		{
			name:     "political category",
			category: category.Political,
			want:     "涉政",
		},
		{
			name:     "none category",
			category: category.None,
			want:     "未分类",
		},
		{
			name:     "invalid category",
			category: category.Category(1 << 31),
			want:     "未知分类",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.category.String(); got != tt.want {
				t.Errorf("Category.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_Concurrent 测试并发操作
func TestSWD_Concurrent(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	const (
		numGoroutines = 10
		numOperations = 100
	)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errChan := make(chan error, numGoroutines*numOperations)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				// 并发检测
				if swd.Detect("这是一段包含色情的文本") != true {
					errChan <- fmt.Errorf("concurrent detect failed")
				}
				// 并发替换
				if swd.Replace("这是一段包含色情的文本", '*') != "这是一段包含**的文本" {
					errChan <- fmt.Errorf("concurrent replace failed")
				}
				// 并发添加和删除
				word := fmt.Sprintf("测试词%d-%d", id, j)
				if err := swd.AddWord(word, category.Custom); err != nil {
					errChan <- fmt.Errorf("concurrent add word failed: %v", err)
				}
				if err := swd.RemoveWord(word); err != nil {
					errChan <- fmt.Errorf("concurrent remove word failed: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("Concurrent test error: %v", err)
	}
}

// TestSWD_Performance 测试性能
func TestSWD_Performance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance test in short mode")
	}

	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	longText := "这是一段很长的文本，包含了多个敏感词：色情、暴力、政府、赌博、毒品、脏话、歧视、诈骗。这些词被重复多次："
	longText += "色情暴力政府赌博毒品脏话歧视诈骗。"
	for i := 0; i < 10; i++ {
		longText += longText
	}

	tests := []struct {
		name     string
		text     string
		maxTime  time.Duration
		numTests int
	}{
		{
			name:     "short text performance",
			text:     "这是一段包含色情的文本",
			maxTime:  time.Millisecond * 100,
			numTests: 10000,
		},
		{
			name:     "long text performance",
			text:     longText,
			maxTime:  time.Second * 10,
			numTests: 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := *util.CurrentTime()
			for i := 0; i < tt.numTests; i++ {
				_ = swd.Detect(tt.text)
			}
			duration := time.Since(start)

			t.Logf("Performance test completed in %v", duration)
			if duration > tt.maxTime {
				t.Errorf("Performance test took too long: %v > %v", duration, tt.maxTime)
			}
		})
	}
}

// TestSWD_LoadCustomWords 测试加载自定义词库
func TestSWD_LoadCustomWords(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	tests := []struct {
		name    string
		words   map[string]category.Category
		wantErr bool
	}{
		{
			name:    "empty words",
			words:   map[string]category.Category{},
			wantErr: false,
		},
		{
			name: "valid words",
			words: map[string]category.Category{
				"测试词1": category.Pornography,
				"测试词2": category.Political,
			},
			wantErr: false,
		},
		{
			name: "invalid category",
			words: map[string]category.Category{
				"测试词": category.Category(1 << 31),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := swd.LoadCustomWords(context.Background(), tt.words)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadCustomWords() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSWD_AddWords 测试批量添加敏感词
func TestSWD_AddWords(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	tests := []struct {
		name    string
		words   map[string]category.Category
		wantErr bool
	}{
		{
			name:    "empty words",
			words:   map[string]category.Category{},
			wantErr: false,
		},
		{
			name: "valid words",
			words: map[string]category.Category{
				"测试词1": category.Pornography,
				"测试词2": category.Political,
			},
			wantErr: false,
		},
		{
			name: "invalid category",
			words: map[string]category.Category{
				"测试词": category.Category(1 << 31),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := swd.AddWords(tt.words)
			if (err != nil) != tt.wantErr {
				t.Errorf("AddWords() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSWD_RemoveWords 测试批量删除敏感词
func TestSWD_RemoveWords(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	// 先添加一些测试词
	testWords := map[string]category.Category{
		"测试词1": category.Pornography,
		"测试词2": category.Political,
	}
	if err := swd.AddWords(testWords); err != nil {
		t.Fatalf("Failed to add test words: %v", err)
	}

	tests := []struct {
		name    string
		words   []string
		wantErr bool
	}{
		{
			name:    "empty words",
			words:   []string{},
			wantErr: false,
		},
		{
			name:    "existing words",
			words:   []string{"测试词1", "测试词2"},
			wantErr: false,
		},
		{
			name:    "non-existing words",
			words:   []string{"不存在的词"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := swd.RemoveWords(tt.words)
			if (err != nil) != tt.wantErr {
				t.Errorf("RemoveWords() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSWD_Match 测试匹配单个敏感词
func TestSWD_Match(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name     string
		text     string
		wantWord string
		wantNil  bool
	}{
		{
			name:    "empty text",
			text:    "",
			wantNil: true,
		},
		{
			name:    "text without sensitive word",
			text:    "这是一段正常的文本",
			wantNil: true,
		},
		{
			name:     "text with sensitive word",
			text:     "这是一段包含色情的文本",
			wantWord: "色情",
			wantNil:  false,
		},
		{
			name:     "text with multiple sensitive words",
			text:     "这是一段包含色情和暴力的文本",
			wantWord: "色情",
			wantNil:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := swd.Match(tt.text)
			if tt.wantNil {
				if got != nil {
					t.Errorf("Match() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Error("Match() = nil, want non-nil")
				return
			}
			if got.Word != tt.wantWord {
				t.Errorf("Match().Word = %v, want %v", got.Word, tt.wantWord)
			}
		})
	}
}

// TestSWD_MatchAll 测试匹配所有敏感词
func TestSWD_MatchAll(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name      string
		text      string
		wantWords []string
	}{
		{
			name:      "empty text",
			text:      "",
			wantWords: nil,
		},
		{
			name:      "text without sensitive word",
			text:      "这是一段正常的文本",
			wantWords: nil,
		},
		{
			name:      "text with single sensitive word",
			text:      "这是一段包含色情的文本",
			wantWords: []string{"色情"},
		},
		{
			name:      "text with multiple sensitive words",
			text:      "这是一段包含色情和暴力的文本",
			wantWords: []string{"色情", "暴力"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := swd.MatchAll(tt.text)
			if len(got) != len(tt.wantWords) {
				t.Errorf("MatchAll() returned %d words, want %d", len(got), len(tt.wantWords))
				return
			}
			for i, word := range tt.wantWords {
				if got[i].Word != word {
					t.Errorf("MatchAll()[%d].Word = %v, want %v", i, got[i].Word, word)
				}
			}
		})
	}
}

// TestSWD_ReplaceWithAsterisk 测试使用星号替换敏感词
func TestSWD_ReplaceWithAsterisk(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "text without sensitive word",
			text: "这是一段正常的文本",
			want: "这是一段正常的文本",
		},
		{
			name: "text with single sensitive word",
			text: "这是一段包含色情的文本",
			want: "这是一段包含**的文本",
		},
		{
			name: "text with multiple sensitive words",
			text: "这是一段包含色情和暴力的文本",
			want: "这是一段包含**和**的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.ReplaceWithAsterisk(tt.text); got != tt.want {
				t.Errorf("ReplaceWithAsterisk() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_ReplaceWithStrategy 测试使用自定义策略替换敏感词
func TestSWD_ReplaceWithStrategy(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	// 自定义替换策略：用 "[REMOVED]" 替换敏感词
	strategy := func(word core.SensitiveWord) string {
		return "[REMOVED]"
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "text without sensitive word",
			text: "这是一段正常的文本",
			want: "这是一段正常的文本",
		},
		{
			name: "text with single sensitive word",
			text: "这是一段包含色情的文本",
			want: "这是一段包含[REMOVED]的文本",
		},
		{
			name: "text with multiple sensitive words",
			text: "这是一段包含色情和暴力的文本",
			want: "这是一段包含[REMOVED]和[REMOVED]的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.ReplaceWithStrategy(tt.text, strategy); got != tt.want {
				t.Errorf("ReplaceWithStrategy() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_MatchIn 测试在指定分类中匹配敏感词
func TestSWD_MatchIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name       string
		text       string
		categories []category.Category
		wantWord   string
		wantNil    bool
	}{
		{
			name:       "empty text",
			text:       "",
			categories: []category.Category{category.Pornography},
			wantNil:    true,
		},
		{
			name:       "text without sensitive word in category",
			text:       "这是一段包含暴力的文本",
			categories: []category.Category{category.Pornography},
			wantNil:    true,
		},
		{
			name:       "text with sensitive word in category",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Pornography},
			wantWord:   "色情",
			wantNil:    false,
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和暴力的文本",
			categories: []category.Category{category.Pornography, category.Violence},
			wantWord:   "色情",
			wantNil:    false,
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和暴力的文本",
			categories: []category.Category{category.Violence},
			wantWord:   "暴力",
			wantNil:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := swd.MatchIn(tt.text, tt.categories...)
			if tt.wantNil {
				if got != nil {
					t.Errorf("MatchIn() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Error("MatchIn() = nil, want non-nil")
				return
			}
			if got.Word != tt.wantWord {
				t.Errorf("MatchIn().Word = %v, want %v", got.Word, tt.wantWord)
			}
		})
	}
}

// TestSWD_MatchAllIn 测试在指定分类中匹配所有敏感词
func TestSWD_MatchAllIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name       string
		text       string
		categories []category.Category
		wantWords  []string
	}{
		{
			name:       "empty text",
			text:       "",
			categories: []category.Category{category.Pornography},
			wantWords:  nil,
		},
		{
			name:       "text without sensitive word in category",
			text:       "这是一段包含暴力的文本",
			categories: []category.Category{category.Pornography},
			wantWords:  nil,
		},
		{
			name:       "text with sensitive word in category",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Pornography},
			wantWords:  []string{"色情"},
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和暴力的文本",
			categories: []category.Category{category.Pornography, category.Violence},
			wantWords:  []string{"色情", "暴力"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := swd.MatchAllIn(tt.text, tt.categories...)
			if len(got) != len(tt.wantWords) {
				t.Errorf("MatchAllIn() returned %d words, want %d", len(got), len(tt.wantWords))
				return
			}
			for i, word := range tt.wantWords {
				if got[i].Word != word {
					t.Errorf("MatchAllIn()[%d].Word = %v, want %v", i, got[i].Word, word)
				}
			}
		})
	}
}

// TestSWD_ReplaceIn 测试在指定分类中替换敏感词
func TestSWD_ReplaceIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name        string
		text        string
		replacement rune
		categories  []category.Category
		want        string
	}{
		{
			name:        "empty text",
			text:        "",
			replacement: '*',
			categories:  []category.Category{category.Pornography},
			want:        "",
		},
		{
			name:        "text without sensitive word in category",
			text:        "这是一段包含暴力的文本",
			replacement: '*',
			categories:  []category.Category{category.Pornography},
			want:        "这是一段包含暴力的文本",
		},
		{
			name:        "text with sensitive word in category",
			text:        "这是一段包含色情的文本",
			replacement: '*',
			categories:  []category.Category{category.Pornography},
			want:        "这是一段包含**的文本",
		},
		{
			name:        "text with multiple categories",
			text:        "这是一段包含色情和暴力的文本",
			replacement: '#',
			categories:  []category.Category{category.Pornography, category.Violence},
			want:        "这是一段包含##和##的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.ReplaceIn(tt.text, tt.replacement, tt.categories...); got != tt.want {
				t.Errorf("ReplaceIn() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_ReplaceWithAsteriskIn 测试在指定分类中使用星号替换敏感词
func TestSWD_ReplaceWithAsteriskIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	tests := []struct {
		name       string
		text       string
		categories []category.Category
		want       string
	}{
		{
			name:       "empty text",
			text:       "",
			categories: []category.Category{category.Pornography},
			want:       "",
		},
		{
			name:       "text without sensitive word in category",
			text:       "这是一段包含暴力的文本",
			categories: []category.Category{category.Pornography},
			want:       "这是一段包含暴力的文本",
		},
		{
			name:       "text with sensitive word in category",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Pornography},
			want:       "这是一段包含**的文本",
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和暴力的文本",
			categories: []category.Category{category.Pornography, category.Violence},
			want:       "这是一段包含**和**的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.ReplaceWithAsteriskIn(tt.text, tt.categories...); got != tt.want {
				t.Errorf("ReplaceWithAsteriskIn() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSWD_ReplaceWithStrategyIn 测试在指定分类中使用自定义策略替换敏感词
func TestSWD_ReplaceWithStrategyIn(t *testing.T) {
	swd, err := New(NewDefaultFactory())
	if err != nil {
		t.Fatalf("Failed to create SWD instance: %v", err)
	}

	if err := swd.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("Failed to load default words: %v", err)
	}

	// 自定义替换策略：用 "[REMOVED]" 替换敏感词
	strategy := func(word core.SensitiveWord) string {
		return "[REMOVED]"
	}

	tests := []struct {
		name       string
		text       string
		categories []category.Category
		want       string
	}{
		{
			name:       "empty text",
			text:       "",
			categories: []category.Category{category.Pornography},
			want:       "",
		},
		{
			name:       "text without sensitive word in category",
			text:       "这是一段包含暴力的文本",
			categories: []category.Category{category.Pornography},
			want:       "这是一段包含暴力的文本",
		},
		{
			name:       "text with sensitive word in category",
			text:       "这是一段包含色情的文本",
			categories: []category.Category{category.Pornography},
			want:       "这是一段包含[REMOVED]的文本",
		},
		{
			name:       "text with multiple categories",
			text:       "这是一段包含色情和暴力的文本",
			categories: []category.Category{category.Pornography, category.Violence},
			want:       "这是一段包含[REMOVED]和[REMOVED]的文本",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swd.ReplaceWithStrategyIn(tt.text, strategy, tt.categories...); got != tt.want {
				t.Errorf("ReplaceWithStrategyIn() = %v, want %v", got, tt.want)
			}
		})
	}
}
