package dbcache

// ============================================================================
// 原子写操作描述：Redis 权威写路径的唯一输入形状。
//
// Go 侧把一次写编码成 writeOp JSON 交给 Lua（writeScript / txScript）原子执行；
// semantics.go 的纯 Go参考实现消费同一结构、逐条同构，fakeStore 委托它，
// 真 Redis 差分测试用同一断言表对拍两套实现，防漂移。
//
// 校验失败零副作用：条件（conds/anyOf/expect）对 live 旧行求值，任一不满足
// 整 op 不应用（affected=0）；唯一守卫冲突在应用前检出，整 op 回滚。
// ============================================================================

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// writeOp 一次原子写。Op 取值 save/insert/increment/max/cas/delete。
type writeOp struct {
	Op      string                `json:"op"`               // save|insert|increment|max|cas|delete
	Key     string                `json:"key"`              // 行键（脏账 member）
	Fields  map[string]string     `json:"fields,omitempty"` // save/insert：全字段（已编码）
	Secret  map[string]string     `json:"secret,omitempty"` // 秘密列：只写 :s:，永不进 :r:
	Set     map[string]string     `json:"set,omitempty"`    // cas：写入列
	Expect  map[string]expectDesc `json:"expect,omitempty"` // cas：期望值
	Conds   []condDesc            `json:"conds,omitempty"`  // 结构化条件（对 live 行校验）
	AnyOf   [][]condDesc          `json:"anyOf,omitempty"`  // OR 组：组内并、组间交
	Column  string                `json:"column,omitempty"` // increment/max 列
	Delta   int64                 `json:"delta,omitempty"`  // increment 增量（可负）
	Max     string                `json:"max,omitempty"`    // max 候选值（十进制串，写回选中原始串）
	Due     int64                 `json:"due"`              // 脏账到期 ms
	TombTTL int64                 `json:"tombTTL"`          // 墓碑 PX ms
	NX      bool                  `json:"nx,omitempty"`     // save：已有行或墓碑存活时不覆盖（回填防复活）
	Sink    bool                  `json:"sink,omitempty"`   // delete：入 dirtyDel 异步删 sink
	NoTomb  bool                  `json:"noTomb,omitempty"` // delete：纯缓存失效（eviction），不立删除墓碑
	Idx     []idxDesc             `json:"idx,omitempty"`    // 索引声明（与 KEYS[12..] 同序）
	UQ      [][]string            `json:"uq,omitempty"`     // 唯一列组
}

// idxDesc 一个二级索引声明。Kind=num 用 score；lex 用定宽串 member。
type idxDesc struct {
	Key  string `json:"key"`  // 完整索引键（同时在 KEYS 里）
	Col  string `json:"col"`  // 列名
	Kind string `json:"kind"` // num|lex
}

// condDesc 一个结构化条件。Type=num 走数值比较，str 走规范化串比较
// （时间列定宽 UTC 编码，字典序=时间序）。
type condDesc struct {
	Col  string   `json:"col"`
	Op   string   `json:"op"` // = <> > >= < <= LIKE IN "IS NULL" "IS NOT NULL"
	Val  string   `json:"val,omitempty"`
	Vals []string `json:"vals,omitempty"`
	Type string   `json:"type"` // num|str
}

// expectDesc CAS 期望值。Mode=null → 缺失或 ""（SQL NULL 哨兵）；in → 任一；eq → 相等。
type expectDesc struct {
	Mode string   `json:"mode"` // null|in|eq
	Val  string   `json:"val,omitempty"`
	Vals []string `json:"vals,omitempty"`
	Type string   `json:"type"`
}

// writeResult Lua/参考实现的统一返回。Err=duplicate|lock_lost 时零副作用。
type writeResult struct {
	Affected int64             `json:"affected"`
	Ver      int64             `json:"ver"`
	Row      map[string]string `json:"row,omitempty"`
	Err      string            `json:"err,omitempty"`
}

// txPayload 一次原子批的输入：txid + 按序 writeOp 列表。KEYS 是各 op 键块
// （11+#idx 个）按序拼接，与 ops 一一对应。
type txPayload struct {
	Txid string     `json:"txid"`
	Ops  []*writeOp `json:"ops"`
}

// txResult 原子批返回：成功带逐 op results；失败带 err 与首个失败 op 下标。
type txResult struct {
	Results []*writeResult `json:"results,omitempty"`
	Err     string         `json:"err,omitempty"`
	Index   int            `json:"index,omitempty"`
}

// writeKeys 一次写涉及的全部键（固定布局，Lua KEYS 同序）。
// 全部同 hash tag；lock 不用时填 row 占位（txid="" 跳过校验）。
type writeKeys struct {
	row      string   // KEYS[1]
	tomb     string   // KEYS[2]
	dirty    string   // KEYS[3]
	retry    string   // KEYS[4]
	keySet   string   // KEYS[5]
	secret   string   // KEYS[6]
	seq      string   // KEYS[7]
	dirtyDel string   // KEYS[8]
	uq       string   // KEYS[9]
	lock     string   // KEYS[10]
	inflight string   // KEYS[11]
	ix       []string // KEYS[12..]
}

// redisKeys 展开成 EVAL 的 KEYS 切片。
func (k writeKeys) redisKeys() []string {
	out := make([]string, 0, 11+len(k.ix))
	out = append(out, k.row, k.tomb, k.dirty, k.retry, k.keySet, k.secret, k.seq, k.dirtyDel, k.uq, k.lock, k.inflight)
	return append(out, k.ix...)
}

// versionField 是行 hash 的保留版本列：每次应用写 +1，陈旧 flush 据此跳过。
const versionField = "__ver"

// colTypeFor 把字段类型归一成条件比较类型：整型/浮点/布尔/duration → num，其余 → str。
func colTypeFor(t reflect.Type) string {
	if t == durationType {
		return "num"
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Bool:
		return "num"
	}
	return "str"
}

// condDescFromRange 把读形状 Range 翻译成写路径条件（列名/ op 白名单校验）。
func condDescFromRange(b *binding, r Range) (condDesc, error) {
	if err := validateColumn(b, r.Column); err != nil {
		return condDesc{}, err
	}
	c := condDesc{Col: r.Column, Op: r.Op, Type: colTypeFor(b.meta.Fields[b.meta.ByColumn[r.Column]].Type)}
	switch r.Op {
	case "IS NULL", "IS NOT NULL":
		return c, nil
	case "=", "<>", ">", ">=", "<", "<=", "LIKE":
		s, err := encodeValue(reflect.ValueOf(r.Value))
		if err != nil {
			return condDesc{}, err
		}
		c.Val = s
		return c, nil
	case "IN":
		rv := reflect.ValueOf(r.Value)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return condDesc{}, fmt.Errorf("%w: IN needs slice", ErrBadCond)
		}
		for i := 0; i < rv.Len(); i++ {
			s, err := encodeValue(rv.Index(i))
			if err != nil {
				return condDesc{}, err
			}
			c.Vals = append(c.Vals, s)
		}
		return c, nil
	}
	return condDesc{}, fmt.Errorf("%w: op %q", ErrBadColumn, r.Op)
}

// expectDescFrom 把 CAS 期望值翻译成 expectDesc：nil→null，切片→in，其余→eq。
func expectDescFrom(b *binding, col string, v any) (expectDesc, error) {
	if err := validateColumn(b, col); err != nil {
		return expectDesc{}, err
	}
	e := expectDesc{Type: colTypeFor(b.meta.Fields[b.meta.ByColumn[col]].Type)}
	if v == nil {
		e.Mode = "null"
		return e, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		e.Mode = "in"
		for i := 0; i < rv.Len(); i++ {
			s, err := encodeValue(rv.Index(i))
			if err != nil {
				return expectDesc{}, err
			}
			e.Vals = append(e.Vals, s)
		}
		return e, nil
	}
	s, err := encodeValue(rv)
	if err != nil {
		return expectDesc{}, err
	}
	e.Mode = "eq"
	e.Val = s
	return e, nil
}

// encodeFieldMap 把任意值映射编码成 hash 字符串映射（CAS set / SecretSet 用）。
func encodeFieldMap(m map[string]any) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, err := encodeValue(reflect.ValueOf(v))
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// guardField 唯一守卫的 hash field：列名与值都长度前缀化，防拼接歧义。
// 任一键列值缺失或 "" → 返回 ""（SQL NULL 不参与唯一约束，跳过该组）。
// Lua 侧 guardField 逐字同构。
func guardField(cols []string, m map[string]string) string {
	var b strings.Builder
	for _, col := range cols {
		v := m[col]
		if v == "" {
			return ""
		}
		b.WriteString(strconv.Itoa(len(col)))
		b.WriteByte(':')
		b.WriteString(col)
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}
