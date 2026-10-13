package postgres

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件提供 PostgreSQL 特有类型的 GORM Scanner/Valuer 适配，让业务模型可以
// 直接把 jsonb / 数组 / hstore 列映射到 Go 字段。所有 Value 输出都是 PG 能
// 直接吃下的文本字面量，配合列本身声明为对应类型（jsonb / text[] / hstore）即可。

// ==================== JSONB ====================

// JSONB 映射 PostgreSQL jsonb/json 列。空值以 SQL NULL 存储。
type JSONB json.RawMessage

// Value 实现 driver.Valuer：输出原始 JSON 字节。
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return []byte(j), nil
}

// Scan 实现 sql.Scanner：接受 []byte 或 string。
func (j *JSONB) Scan(src any) error {
	if src == nil {
		*j = nil
		return nil
	}
	switch v := src.(type) {
	case []byte:
		*j = append((*j)[0:0], v...)
		return nil
	case string:
		*j = JSONB(v)
		return nil
	default:
		return fmt.Errorf("postgres.JSONB: 不支持的源类型 %T", src)
	}
}

// MarshalJSON 直接透传原始 JSON。
func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return []byte(j), nil
}

// UnmarshalJSON 原样接收 JSON 片段。
func (j *JSONB) UnmarshalJSON(data []byte) error {
	if j == nil {
		return fmt.Errorf("postgres.JSONB: nil 接收者")
	}
	*j = append((*j)[0:0], data...)
	return nil
}

// ==================== 文本数组 ====================

// StringArray 映射 PostgreSQL text[]/varchar[] 列。
type StringArray []string

// Value 输出 PG 数组字面量 {"a","b"}，对内部引号与反斜杠转义。
func (a StringArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(escapeArrayElem(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan 解析 PG 数组字面量或驱动返回的 []byte/string。
func (a *StringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var raw string
	switch v := src.(type) {
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		return fmt.Errorf("postgres.StringArray: 不支持的源类型 %T", src)
	}
	elems, err := parseArrayLiteral(raw)
	if err != nil {
		return err
	}
	out := make(StringArray, len(elems))
	copy(out, elems)
	*a = out
	return nil
}

// ==================== 整型数组 ====================

// Int64Array 映射 PostgreSQL bigint[]/integer[] 列。
type Int64Array []int64

// Value 输出 PG 数组字面量 {1,2,3}。
func (a Int64Array) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(n, 10))
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan 解析 PG 数组字面量。
func (a *Int64Array) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var raw string
	switch v := src.(type) {
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		return fmt.Errorf("postgres.Int64Array: 不支持的源类型 %T", src)
	}
	elems, err := parseArrayLiteral(raw)
	if err != nil {
		return err
	}
	out := make(Int64Array, 0, len(elems))
	for _, e := range elems {
		if e == "" {
			continue
		}
		n, err := strconv.ParseInt(e, 10, 64)
		if err != nil {
			return fmt.Errorf("postgres.Int64Array: 解析 %q 失败: %w", e, err)
		}
		out = append(out, n)
	}
	*a = out
	return nil
}

// ==================== HSTORE ====================

// HStore 映射 PostgreSQL hstore 列（需服务端启用 hstore 扩展）。
type HStore map[string]string

// Value 输出 hstore 字面量 "k"=>"v", ...。
func (h HStore) Value() (driver.Value, error) {
	if h == nil {
		return nil, nil
	}
	parts := make([]string, 0, len(h))
	for k, v := range h {
		parts = append(parts, fmt.Sprintf("%q=>%q", k, v))
	}
	return strings.Join(parts, ", "), nil
}

// Scan 解析 hstore 字面量。仅覆盖常见的 "k"=>"v" 形式。
func (h *HStore) Scan(src any) error {
	if src == nil {
		*h = nil
		return nil
	}
	var raw string
	switch v := src.(type) {
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		return fmt.Errorf("postgres.HStore: 不支持的源类型 %T", src)
	}
	out := make(HStore)
	for _, pair := range splitHStore(raw) {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		sep := strings.Index(pair, "=>")
		if sep < 0 {
			return fmt.Errorf("postgres.HStore: 无法解析片段 %q", pair)
		}
		k, err := strconv.Unquote(strings.TrimSpace(pair[:sep]))
		if err != nil {
			k = strings.Trim(strings.TrimSpace(pair[:sep]), `"`)
		}
		vs := strings.TrimSpace(pair[sep+2:])
		if vs == "NULL" {
			out[k] = ""
			continue
		}
		v, err := strconv.Unquote(vs)
		if err != nil {
			v = strings.Trim(vs, `"`)
		}
		out[k] = v
	}
	*h = out
	return nil
}

// ==================== 数组字面量解析 ====================

// escapeArrayElem 转义 PG 数组元素中的反斜杠与双引号。
func escapeArrayElem(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// parseArrayLiteral 解析 PG 数组字面量 {a,"b,c",NULL} → []string。
// NULL 元素被跳过；带引号元素按 Go 反引号规则去引号。仅支持一维数组。
func parseArrayLiteral(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("postgres: 非法数组字面量 %q", s)
	}
	body := s[1 : len(s)-1]
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	var out []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case ch == '\\' && inQuotes:
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case ch == ',' && !inQuotes:
			out = append(out, decodeElem(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, decodeElem(cur.String()))
	return out, nil
}

// decodeElem 去掉元素两端引号并把 NULL 归一为空串（调用方按需忽略空串）。
func decodeElem(s string) string {
	s = strings.TrimSpace(s)
	if s == "NULL" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

// splitHStore 按顶层逗号切分 hstore 字面量（引号内的逗号不切）。
func splitHStore(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case ch == '\\' && inQuotes:
			cur.WriteByte(ch)
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
			cur.WriteByte(ch)
		case ch == ',' && !inQuotes:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}
