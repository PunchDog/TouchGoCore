package mappingdata

import (
	"fmt"
	"testing"
	"testing/fstest"
)

// TestDefaultEmbedsAllTables 校验内嵌表真的被解析出来了（历史缺陷：多字节键被整表丢弃）。
func TestDefaultEmbedsAllTables(t *testing.T) {
	tables := Default()

	if len(tables.FullWidthToHalf) == 0 {
		t.Fatal("全半角映射为空")
	}
	if len(tables.NumberStyle) == 0 {
		t.Fatal("数字样式映射为空")
	}
	if len(tables.Pinyin) == 0 {
		t.Fatal("拼音映射为空")
	}
	if len(tables.Homophone) == 0 {
		t.Fatal("同音字映射为空")
	}
	if len(tables.SimilarShape) == 0 {
		t.Fatal("形近字映射为空")
	}
}

// TestDefaultContainsMultiByteKeys 专门针对按字节长度判断单字符的历史缺陷：
// 中文、全角等 3 字节键必须存在。
func TestDefaultContainsMultiByteKeys(t *testing.T) {
	tables := Default()

	if got := tables.FullWidthToHalf['Ａ']; got != 'A' {
		t.Errorf("全角 Ａ 应映射到 A，实际 %q", got)
	}
	if got := tables.NumberStyle['零']; got != '0' {
		t.Errorf("中文数字 零 应映射到 0，实际 %q", got)
	}
	if got := tables.NumberStyle['壹']; got != '1' {
		t.Errorf("大写数字 壹 应映射到 1，实际 %q", got)
	}
	if homophones := tables.Homophone['发']; !containsRune(homophones, '法') {
		t.Errorf("发 的同音字应包含 法，实际 %q", string(homophones))
	}
	if similar := tables.SimilarShape['门']; len(similar) == 0 {
		t.Errorf("门 的形近字为空")
	}
	if similar := tables.SimilarShape['0']; !containsRune(similar, 'O') || !containsRune(similar, '〇') {
		t.Errorf("0 的形近字应包含 O 与 〇，实际 %q", string(similar))
	}
	if chars := tables.Pinyin["fa"]; len(chars) == 0 {
		t.Errorf("拼音 fa 未映射到汉字")
	}
}

// TestSimilarShapeChineseWins 中文形近字表不应被字母数字表覆盖。
func TestSimilarShapeChineseWins(t *testing.T) {
	root := fstest.MapFS{
		"fullwidth.txt":        {Data: []byte("Ａ -> A\n")},
		"numberstyle.txt":      {Data: []byte("① -> 1\n")},
		"pinyin.txt":           {Data: []byte("FA -> 发,法\n")},
		"homophone.txt":        {Data: []byte("发 -> 法,法,发\n")},
		"similar_chinese.txt":  {Data: []byte("门 -> 門\n")},
		"similar_alphanum.txt": {Data: []byte("门 -> 们\n0 -> O,o\n")},
	}

	tables, err := Parse(root)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := tables.SimilarShape['门']; !containsRune(got, '門') || containsRune(got, '们') {
		t.Errorf("中文形近字表应优先，实际 %q", string(got))
	}
	// 去重与自指剔除
	if got := tables.Homophone['发']; !containsRune(got, '法') || containsRune(got, '发') {
		t.Errorf("同音字应去重并剔除自指，实际 %q", string(got))
	}
	if got := tables.Pinyin["fa"]; len(got) != 2 {
		t.Errorf("拼音键应统一小写，实际 %v", got)
	}
}

// TestParseMissingFileFails 缺文件必须报错，而不是静默给出空表。
func TestParseMissingFileFails(t *testing.T) {
	root := fstest.MapFS{
		"fullwidth.txt": {Data: []byte("Ａ -> A\n")},
	}
	if _, err := Parse(root); err == nil {
		t.Fatal("缺少映射文件时应当报错")
	}
}

// TestParseMalformedLineFails 非法行必须报错。
func TestParseMalformedLineFails(t *testing.T) {
	cases := map[string]string{
		"missing-arrow": "Ａ B\n",
		"empty-value":   "Ａ -> \n",
		"bad-rune-map":  "ＡＢ -> A\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := fullFilesystem("fullwidth.txt", content)
			if _, err := Parse(root); err == nil {
				t.Fatalf("非法内容 %q 应当报错", content)
			}
		})
	}
}

// fullFilesystem 用 given 覆盖单个文件，其余文件给出合法最小内容。
func fullFilesystem(target, content string) fstest.MapFS {
	defaults := map[string]string{
		"fullwidth.txt":        "Ａ -> A\n",
		"numberstyle.txt":      "① -> 1\n",
		"pinyin.txt":           "fa -> 发\n",
		"homophone.txt":        "发 -> 法\n",
		"similar_chinese.txt":  "门 -> 門\n",
		"similar_alphanum.txt": "0 -> O\n",
	}
	root := make(fstest.MapFS, len(defaults))
	for name, data := range defaults {
		root[name] = &fstest.MapFile{Data: []byte(data)}
	}
	root[target] = &fstest.MapFile{Data: []byte(content)}
	return root
}

func containsRune(list []rune, r rune) bool {
	for _, item := range list {
		if item == r {
			return true
		}
	}
	return false
}

func ExampleParse() {
	tables, err := Parse(FS())
	if err != nil {
		fmt.Println("解析失败:", err)
		return
	}
	fmt.Printf("半角 A 来源全角: %v\n", tables.FullWidthToHalf['Ａ'] == 'A')
	// Output:
	// 半角 A 来源全角: true
}
