package algorithm

import (
	"testing"

	"touchgocore/swd/core"
	"touchgocore/swd/types/category"
)

// QA-GL: AC 自动机后缀词（dictionary suffix link）漏检回归。
//
// 旧 buildOutputList 只在 failLink 本身 isEnd 时追加输出，failLink 非词尾但其
// 更深 fail 链上有词尾时整段丢失。构造：词表 {"abcd","bcx","c"}，
// fail(abc)=bc（非词尾），fail(bc)=c（词尾）——旧实现 output(abc)=空，
// 遍历到 abc 节点时漏检 "c"。

func matchSet(words map[string]category.Category, text string, distance int) map[string]bool {
	ac := NewAhoCorasick()
	if err := ac.Build(words); err != nil {
		panic(err)
	}
	got := map[string]bool{}
	var matches []core.SensitiveWord
	if distance > 0 {
		matches = ac.MatchAllWithDistance(text, distance)
	} else {
		matches = ac.MatchAll(text)
	}
	for _, m := range matches {
		got[m.Word] = true
	}
	return got
}

func TestQaGL_ACFailChainSuffixNotMissed(t *testing.T) {
	words := map[string]category.Category{
		"abcd": category.None,
		"bcx":  category.None,
		"c":    category.None,
	}

	ac := NewAhoCorasick()
	if err := ac.Build(words); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Match：走到 abc 节点（非词尾）时，output 列表必须带回 fail 链深处的 "c"。
	// 旧实现在此返回 nil（漏检）。
	m := ac.Match("zabcz")
	if m == nil {
		t.Fatalf("Match(\"zabcz\") = nil, 期望命中后缀词 \"c\"（旧实现漏检复现点）")
	}
	if m.Word != "c" {
		t.Errorf("Match(\"zabcz\") 命中 %q, 期望 \"c\"", m.Word)
	}

	// 距离匹配路径同样走预计算 output 列表
	got := matchSet(words, "zabcz", 1)
	if !got["c"] {
		t.Errorf("MatchAllWithDistance(\"zabcz\",1) 漏检 \"c\", got=%v", got)
	}
}

func TestQaGL_ACUshersAllSuffixHits(t *testing.T) {
	words := map[string]category.Category{
		"she":  category.None,
		"he":   category.None,
		"hers": category.None,
	}

	// 既有语义回归：全部命中路径不漏较短后缀词
	got := matchSet(words, "ushers", 0)
	for _, want := range []string{"she", "he", "hers"} {
		if !got[want] {
			t.Errorf("MatchAll(\"ushers\") 漏检 %q, got=%v", want, got)
		}
	}

	got = matchSet(words, "ushers", 1)
	for _, want := range []string{"she", "he", "hers"} {
		if !got[want] {
			t.Errorf("MatchAllWithDistance(\"ushers\",1) 漏检 %q, got=%v", want, got)
		}
	}
}
