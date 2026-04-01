package detector

import (
	"testing"

	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
)

func BenchmarkDetector_Detect_Simple(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情的测试文本"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Detect(text)
	}
}

func BenchmarkDetector_Detect_Complex(b *testing.B) {
	options := &core.SWDOptions{
		IgnoreCase:         true,
		IgnoreWidth:        true,
		IgnoreNumStyle:     true,
		EnableHomophone:     true,
		EnableSimilarShape:   true,
		EnableZhPYMix:       true,
		MaxDistance:        2,
		SkipWhitespace:     true,
	}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	d.UpdateAlgoCache()

	text := "这是一段包含色情、暴力、政府、赌博、毒品的复杂测试文本"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Detect(text)
	}
}

func BenchmarkDetector_Match(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情的测试文本"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Match(text)
	}
}

func BenchmarkDetector_MatchAll(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情、暴力、政府、赌博、毒品的测试文本，这些敏感词会重复出现"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.MatchAll(text)
	}
}

func BenchmarkDetector_MatchAll_WithDistance(b *testing.B) {
	options := &core.SWDOptions{
		MaxDistance: 2,
	}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情的测试文本，中间可能有特殊字符"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.MatchAll(text)
	}
}

func BenchmarkDetector_DetectIn(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情的测试文本"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.DetectIn(text, category.Pornography)
	}
}

func BenchmarkDetector_MatchAllIn(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情、暴力、政府、赌博、毒品的测试文本"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.MatchAllIn(text, category.Pornography, category.Violence)
	}
}

func BenchmarkDetector_Evasion_AllFeatures(b *testing.B) {
	options := &core.SWDOptions{
		IgnoreCase:         true,
		IgnoreWidth:        true,
		IgnoreNumStyle:     true,
		EnableHomophone:     true,
		EnableSimilarShape:   true,
		EnableZhPYMix:       true,
		MaxDistance:        2,
		SkipWhitespace:     true,
	}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	d.UpdateAlgoCache()

	// 测试各种规避手段
	tests := []string{
		"这是一段GAMBLING文本",
		"ｇａｍｂｌｉｎｇ全角",
		"赌*博中间加星号",
		"fa票 (发票的拼音)",
		"堵博 (赌博的同音字)",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, text := range tests {
			d.Detect(text)
		}
	}
}

func BenchmarkDetector_Concurrent(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "这是一段包含色情的测试文本"
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			d.Detect(text)
		}
	})
}

func BenchmarkDetector_LongText(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	// 构建长文本
	text := "这是一段测试文本，包含敏感词色情和暴力。"
	for i := 0; i < 100; i++ {
		text += text
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Detect(text)
	}
}

func BenchmarkDetector_ShortText(b *testing.B) {
	options := &core.SWDOptions{}
	d, err := NewDetector(options)
	if err != nil {
		b.Fatal(err)
	}

	text := "色情"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Detect(text)
	}
}
