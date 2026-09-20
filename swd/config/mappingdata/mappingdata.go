// Package mappingdata 内嵌敏感词检测所需的字符映射数据表，并提供解析能力。
//
// 数据表文件随二进制打包，避免依赖运行目录下的相对路径；
// 解析统一按 rune 处理，因此中文、全角字符等多字节键不会被漏掉。
package mappingdata

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"unicode/utf8"
)

// DirName 是映射表在包内嵌文件系统中的目录名。
const DirName = "mappings"

//go:embed all:mappings
var embedded embed.FS

// FileNames 列出全部映射表文件，缺任意一个即视为解析失败。
var FileNames = []string{
	"fullwidth.txt",
	"numberstyle.txt",
	"pinyin.txt",
	"homophone.txt",
	"similar_chinese.txt",
	"similar_alphanum.txt",
}

// Tables 解析后的全部映射表。
type Tables struct {
	FullWidthToHalf map[rune]rune       // 全角 -> 半角
	NumberStyle     map[rune]rune       // 各类数字样式 -> ASCII 数字
	Pinyin          map[string][]string // 拼音 -> 汉字
	Homophone       map[rune][]rune     // 汉字 -> 同音字
	SimilarShape    map[rune][]rune     // 字符 -> 形近字
}

func newTables() *Tables {
	return &Tables{
		FullWidthToHalf: make(map[rune]rune),
		NumberStyle:     make(map[rune]rune),
		Pinyin:          make(map[string][]string),
		Homophone:       make(map[rune][]rune),
		SimilarShape:    make(map[rune][]rune),
	}
}

// FS 返回内嵌映射表的根（其下直接是各 *.txt 文件）。
func FS() fs.FS {
	// DirName 为编译期常量且必然存在于 embed 中，fs.Sub 不会失败。
	sub, _ := fs.Sub(embedded, DirName)
	return sub
}

var (
	defaultOnce  sync.Once
	defaultTable *Tables
)

// Default 返回内嵌数据表解析结果（进程内只解析一次）。
func Default() *Tables {
	defaultOnce.Do(func() {
		parsed, err := Parse(FS())
		if err != nil {
			panic(fmt.Sprintf("内嵌映射表解析失败: %v", err))
		}
		defaultTable = parsed
	})
	return defaultTable
}

// Parse 从 root 目录解析全部映射表，缺少文件或内容非法都会返回错误。
func Parse(root fs.FS) (*Tables, error) {
	t := newTables()

	var err error
	if t.FullWidthToHalf, err = parseRuneMap(root, "fullwidth.txt"); err != nil {
		return nil, err
	}
	if t.NumberStyle, err = parseRuneMap(root, "numberstyle.txt"); err != nil {
		return nil, err
	}
	if t.Pinyin, err = parseStringListMap(root, "pinyin.txt"); err != nil {
		return nil, err
	}
	if t.Homophone, err = parseRuneListMap(root, "homophone.txt"); err != nil {
		return nil, err
	}

	// 形近字分中文与字母数字两张表，中文表优先，不被后者覆盖。
	chinese, err := parseRuneListMap(root, "similar_chinese.txt")
	if err != nil {
		return nil, err
	}
	alphanum, err := parseRuneListMap(root, "similar_alphanum.txt")
	if err != nil {
		return nil, err
	}
	for k, v := range chinese {
		t.SimilarShape[k] = v
	}
	for k, v := range alphanum {
		if _, exists := t.SimilarShape[k]; !exists {
			t.SimilarShape[k] = v
		}
	}

	return t, nil
}

// parseRuneMap 解析单字符到单字符的映射表。
func parseRuneMap(root fs.FS, file string) (map[rune]rune, error) {
	pairs, err := readPairs(root, file)
	if err != nil {
		return nil, err
	}
	out := make(map[rune]rune, len(pairs))
	for _, p := range pairs {
		key, ok := singleRune(p[0])
		if !ok {
			return nil, fmt.Errorf("映射表 %s 的键 %q 不是单个字符", file, p[0])
		}
		val, ok := singleRune(p[1])
		if !ok {
			return nil, fmt.Errorf("映射表 %s 中 %q 的值 %q 不是单个字符", file, p[0], p[1])
		}
		out[key] = val
	}
	return out, nil
}

// parseRuneListMap 解析单字符到字符列表的映射表，跳过空项与自指并按序去重。
func parseRuneListMap(root fs.FS, file string) (map[rune][]rune, error) {
	pairs, err := readPairs(root, file)
	if err != nil {
		return nil, err
	}
	out := make(map[rune][]rune, len(pairs))
	for _, p := range pairs {
		key, ok := singleRune(p[0])
		if !ok {
			return nil, fmt.Errorf("映射表 %s 的键 %q 不是单个字符", file, p[0])
		}
		list := make([]rune, 0, 4)
		seen := make(map[rune]struct{}, 4)
		for _, token := range strings.Split(p[1], ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			r, ok := singleRune(token)
			if !ok {
				return nil, fmt.Errorf("映射表 %s 中 %q 的值 %q 含非单字符项", file, p[0], token)
			}
			if r == key { // 自指无意义
				continue
			}
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			list = append(list, r)
		}
		if len(list) == 0 {
			continue
		}
		out[key] = list
	}
	return out, nil
}

// parseStringListMap 解析字符串（如拼音）到字符串列表的映射表。
func parseStringListMap(root fs.FS, file string) (map[string][]string, error) {
	pairs, err := readPairs(root, file)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(pairs))
	for _, p := range pairs {
		if p[0] == "" {
			return nil, fmt.Errorf("映射表 %s 存在空键", file)
		}
		list := make([]string, 0, 4)
		seen := make(map[string]struct{}, 4)
		for _, token := range strings.Split(p[1], ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if _, dup := seen[token]; dup {
				continue
			}
			seen[token] = struct{}{}
			list = append(list, token)
		}
		if len(list) == 0 {
			continue
		}
		out[strings.ToLower(p[0])] = list
	}
	return out, nil
}

// readPairs 读取 "键 -> 值" 形式的行，跳过空行与 # 注释。
func readPairs(root fs.FS, file string) ([][2]string, error) {
	data, err := fs.ReadFile(root, file)
	if err != nil {
		return nil, fmt.Errorf("读取映射表 %s 失败: %w", file, err)
	}

	pairs := make([][2]string, 0, 64)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "->")
		if !found {
			return nil, fmt.Errorf("映射表 %s 第 %d 行 %q 缺少分隔符 ->", file, lineNo, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" || val == "" {
			return nil, fmt.Errorf("映射表 %s 第 %d 行 %q 的键或值为空", file, lineNo, line)
		}
		pairs = append(pairs, [2]string{key, val})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取映射表 %s 失败: %w", file, err)
	}
	return pairs, nil
}

// singleRune 判断字符串是否恰好为一个合法 rune。
func singleRune(s string) (rune, bool) {
	if s == "" {
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		return 0, false
	}
	if size != len(s) {
		return 0, false
	}
	return r, true
}
