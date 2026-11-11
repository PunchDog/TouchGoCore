package dbcache

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// backendKind 标识表所属的数据库后端，用于选择列名解析规则。
type backendKind uint8

const (
	backendMySQL backendKind = iota + 1
	backendPostgres
	backendMongo
)

// fieldNamer 从结构体字段解析列名、是否跳过、是否主键。
type fieldNamer func(reflect.StructField) (column string, skip bool, primary bool)

// fieldMeta 是模型字段的缓存元信息。
type fieldMeta struct {
	Index  int          // 在结构体中的字段下标
	Column string       // Redis hash / DB 列名
	Type   reflect.Type // 字段类型
	IsKey  bool         // 是否参与 Redis 行键
}

// tableMeta 是一个模型类型的完整元信息。
type tableMeta struct {
	Type     reflect.Type
	Fields   []fieldMeta
	ByColumn map[string]int // 列名 → Fields 下标
	KeyCols  []string       // 行键列（有序）
	PKCol    string         // 主键列；无则空
	HasPK    bool
	HasKey   bool
}

// condVal 是条件结构体解析出的一个过滤项：单值等值，多值 IN。
type condVal struct {
	Column string
	Values []any
}

type condMetaKey struct {
	typ  reflect.Type
	kind backendKind
}

var (
	condMetaCache = sync.Map{} // condMetaKey → *tableMeta
	columnNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	timeType      = reflect.TypeOf(time.Time{})
	durationType  = reflect.TypeOf(time.Duration(0))
)

// namerFor 返回指定后端的列名解析器。
func namerFor(kind backendKind) fieldNamer {
	if kind == backendMongo {
		return mongoFieldNamer
	}
	return sqlFieldNamer
}

// sqlFieldNamer 解析 SQL 模型的列名：cache → gorm → bson → json → snake_case。
func sqlFieldNamer(sf reflect.StructField) (string, bool, bool) {
	col, skip, primary := parseCacheTag(sf)
	if skip {
		return "", true, false
	}
	if g, ok := sf.Tag.Lookup("gorm"); ok {
		if g == "-" {
			return "", true, false
		}
		for _, part := range strings.Split(g, ";") {
			part = strings.TrimSpace(part)
			if part == "primaryKey" {
				primary = true
			}
			if strings.HasPrefix(part, "column:") && col == "" {
				col = strings.TrimSpace(strings.TrimPrefix(part, "column:"))
			}
		}
	}
	if col == "" {
		if b, ok := sf.Tag.Lookup("bson"); ok {
			if name := strings.TrimSpace(strings.Split(b, ",")[0]); name != "" && name != "-" {
				col = name
			}
		}
	}
	if col == "" {
		if j, ok := sf.Tag.Lookup("json"); ok {
			if name := strings.TrimSpace(strings.Split(j, ",")[0]); name != "" && name != "-" {
				col = name
			}
		}
	}
	if col == "" {
		col = snakeCase(sf.Name)
	}
	return col, false, primary
}

// mongoFieldNamer 解析 Mongo 模型的列名：cache → bson → 全小写字段名。
//
// mongo-driver 对未标 bson tag 的字段默认使用全小写字段名；多单词字段
// 建议显式写 bson tag，否则缓存列名与集合字段名可能不一致。
func mongoFieldNamer(sf reflect.StructField) (string, bool, bool) {
	col, skip, primary := parseCacheTag(sf)
	if skip {
		return "", true, false
	}
	if b, ok := sf.Tag.Lookup("bson"); ok {
		if b == "-" {
			return "", true, false
		}
		if name := strings.TrimSpace(strings.Split(b, ",")[0]); name != "" {
			col = name
		}
	}
	if col == "" {
		col = strings.ToLower(sf.Name)
	}
	return col, false, primary
}

// parseCacheTag 解析 `cache:"col,pk"`：`-` 跳过，`pk` 标记主键，其余作为列名。
func parseCacheTag(sf reflect.StructField) (col string, skip bool, primary bool) {
	tag, ok := sf.Tag.Lookup("cache")
	if !ok {
		return "", false, false
	}
	for _, tok := range strings.Split(tag, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "":
		case "-":
			return "", true, false
		case "pk":
			primary = true
		default:
			if col == "" {
				col = tok
			}
		}
	}
	return col, false, primary
}

// buildMeta 构建模型元信息；keyCols 为空时默认取主键列。
func buildMeta(typ reflect.Type, keyCols []string, kind backendKind) (*tableMeta, error) {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: %s is not a struct", ErrBadCond, typ)
	}
	namer := namerFor(kind)
	m := &tableMeta{Type: typ, ByColumn: make(map[string]int)}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.PkgPath != "" { // 未导出字段无法读写
			continue
		}
		col, skip, primary := namer(sf)
		if skip || col == "" {
			continue
		}
		if !columnNameRe.MatchString(col) {
			return nil, fmt.Errorf("%w: %q", ErrBadColumn, col)
		}
		if _, dup := m.ByColumn[col]; dup {
			return nil, fmt.Errorf("%w: duplicate %q", ErrBadColumn, col)
		}
		m.ByColumn[col] = len(m.Fields)
		m.Fields = append(m.Fields, fieldMeta{Index: i, Column: col, Type: sf.Type})
		if primary && !m.HasPK {
			m.PKCol = col
			m.HasPK = true
		}
	}
	if !m.HasPK {
		if _, ok := m.ByColumn["id"]; ok {
			m.PKCol = "id"
			m.HasPK = true
		}
	}
	if len(keyCols) == 0 && m.HasPK {
		keyCols = []string{m.PKCol}
	}
	for _, k := range keyCols {
		if _, ok := m.ByColumn[k]; !ok {
			return nil, fmt.Errorf("%w: key column %q not found", ErrBadColumn, k)
		}
	}
	m.KeyCols = append([]string(nil), keyCols...)
	m.HasKey = len(keyCols) > 0
	for i := range m.Fields {
		for _, k := range keyCols {
			if m.Fields[i].Column == k {
				m.Fields[i].IsKey = true
			}
		}
	}
	return m, nil
}

// metaForCond 返回条件结构体的元信息（按类型+后端缓存）。
func metaForCond(typ reflect.Type, kind backendKind) (*tableMeta, error) {
	key := condMetaKey{typ: typ, kind: kind}
	if v, ok := condMetaCache.Load(key); ok {
		return v.(*tableMeta), nil
	}
	m, err := buildMeta(typ, nil, kind)
	if err != nil {
		return nil, err
	}
	actual, _ := condMetaCache.LoadOrStore(key, m)
	return actual.(*tableMeta), nil
}

// parseCond 把条件结构体解析为过滤项：指针字段参与，nil 跳过，指针切片为 IN。
func parseCond(meta *tableMeta, kind backendKind, cond any) ([]condVal, error) {
	if cond == nil {
		return nil, ErrEmptyCondition
	}
	rv := reflect.ValueOf(cond)
	if rv.Kind() != reflect.Pointer {
		return nil, ErrBadCond
	}
	if rv.IsNil() {
		return nil, ErrEmptyCondition
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return nil, ErrBadCond
	}
	cm, err := metaForCond(rv.Type(), kind)
	if err != nil {
		return nil, err
	}
	out := make([]condVal, 0, len(cm.Fields))
	for _, f := range cm.Fields {
		fv := rv.Field(f.Index)
		if fv.Kind() != reflect.Pointer || fv.IsNil() {
			continue
		}
		if _, ok := meta.ByColumn[f.Column]; !ok {
			return nil, fmt.Errorf("%w: condition column %q not in model", ErrBadColumn, f.Column)
		}
		elem := fv.Elem()
		vals := make([]any, 0, 1)
		if elem.Kind() == reflect.Slice {
			for i := 0; i < elem.Len(); i++ {
				vals = append(vals, elem.Index(i).Interface())
			}
		} else {
			vals = append(vals, elem.Interface())
		}
		out = append(out, condVal{Column: f.Column, Values: vals})
	}
	if len(out) == 0 {
		return nil, ErrEmptyCondition
	}
	return out, nil
}

// condIsExactKey 判断条件是否恰好覆盖全部行键列且均为单值等值。
func condIsExactKey(meta *tableMeta, vals []condVal) bool {
	if !meta.HasKey || len(vals) != len(meta.KeyCols) {
		return false
	}
	for _, key := range meta.KeyCols {
		found := false
		for _, v := range vals {
			if v.Column == key {
				if len(v.Values) != 1 {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// keyFromCond 按 KeyCols 顺序把条件值拼成 Redis 行键。
func keyFromCond(meta *tableMeta, vals []condVal) (string, error) {
	parts := make([]string, 0, len(meta.KeyCols))
	for _, col := range meta.KeyCols {
		var value any
		found := false
		for _, v := range vals {
			if v.Column == col && len(v.Values) == 1 {
				value = v.Values[0]
				found = true
				break
			}
		}
		if !found {
			return "", ErrKeyMissing
		}
		s, err := encodeValue(reflect.ValueOf(value))
		if err != nil {
			return "", err
		}
		parts = append(parts, escapeKeySegment(s))
	}
	return strings.Join(parts, ":"), nil
}

// unescapeKeySegment 还原 escapeKeySegment 的转义（顺序与编码相反）。
func unescapeKeySegment(s string) string {
	s = strings.ReplaceAll(s, "%3A", ":")
	s = strings.ReplaceAll(s, "%25", "%")
	return s
}

// valsFromConditions 从结构化条件（db.Condition）推导行键定位值：只取键列上的
// 单值等值（=）条件。condVals 在 o.Cond 缺席时调用它，让 db.Condition("id","=",x)
// 这类「写路径定位条件」能推导行键（与 MatchStruct 等价）；非键列条件不参与键推导，
// 仍由 casInTx/incrementInTx 经 rangesToDescs 当作守卫消费。缺键列或多值由
// condIsExactKey 兜底拒为 ErrPartialKey（fail-closed，绝不退化成全表写）。
func valsFromConditions(meta *tableMeta, ranges []Range) []condVal {
	if !meta.HasKey {
		return nil
	}
	out := make([]condVal, 0, len(meta.KeyCols))
	for _, col := range meta.KeyCols {
		for _, r := range ranges {
			if r.Table == "" && r.Column == col && r.Op == "=" {
				out = append(out, condVal{Column: col, Values: []any{r.Value}})
				break
			}
		}
	}
	return out
}

// valsFromKey 把行键反解成键列条件（异步 sinkDelete 用）：拆分、去转义、按列类型解码。
func valsFromKey(meta *tableMeta, key string) ([]condVal, error) {
	parts := strings.Split(key, ":")
	if len(parts) != len(meta.KeyCols) {
		return nil, fmt.Errorf("%w: key %q", ErrKeyMissing, key)
	}
	out := make([]condVal, 0, len(parts))
	for i, col := range meta.KeyCols {
		raw := unescapeKeySegment(parts[i])
		f := meta.Fields[meta.ByColumn[col]]
		v, err := decodeValue(raw, f.Type)
		if err != nil {
			return nil, err
		}
		out = append(out, condVal{Column: col, Values: []any{v.Interface()}})
	}
	return out, nil
}

// keyFromRow 按 KeyCols 顺序从行对象推导 Redis 行键。
// 零值整数键列合法（如 cut_point_record 抽奖行 participation_id=0），与
// keyFromCond 允许 0 对齐；仅空串 / nil 指针 / nil 接口视为缺失而报错。
func keyFromRow(meta *tableMeta, rv reflect.Value) (string, error) {
	parts := make([]string, 0, len(meta.KeyCols))
	for _, col := range meta.KeyCols {
		pos, ok := meta.ByColumn[col]
		if !ok {
			return "", ErrKeyMissing
		}
		f := meta.Fields[pos]
		fv := rv.Field(f.Index)
		if isMissingKeyValue(fv) {
			return "", fmt.Errorf("%w: key column %s is empty", ErrKeyMissing, col)
		}
		s, err := encodeValue(fv)
		if err != nil {
			return "", err
		}
		parts = append(parts, escapeKeySegment(s))
	}
	return strings.Join(parts, ":"), nil
}

// isMissingKeyValue 判断行键列是否缺失：仅 nil 指针/接口与空串算缺失。
// 整数/浮点零值是合法键，故不复用 isZeroValue——insert.go 的自增取号仍须
// 把整数 0 视为「待分配」，两者语义不同，不能合并。
func isMissingKeyValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.String:
		return v.Len() == 0
	}
	return false
}

// isZeroValue 判断键列是否为空值（空字符串 / 0 / nil 指针）。
func isZeroValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() == 0
	}
	return false
}

// escapeKeySegment 转义复合键中的分隔符，避免不同键拼接后歧义。
func escapeKeySegment(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, ":", "%3A")
	return s
}

// snakeCase 把 Go 字段名转成 snake_case（处理连续大写）。
func snakeCase(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := runes[i-1]
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || (unicode.IsUpper(prev) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
