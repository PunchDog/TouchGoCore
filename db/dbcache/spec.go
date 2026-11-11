package dbcache

// ============================================================================
// P4：读形状扩展（范围/排序/分页/分组/聚合/JOIN/投影）的 Redis 权威引擎。
//
// 替换原 gorm 后端：键集扫描取行（MaxScanRows fail-closed）→ 内存 JOIN →
// evalConds 过滤 → 分组聚合 / 逐行投影（Go 表达式求值）→ 排序分页 → 按列名
// 扫描进任意目标结构（buildMeta+decodeRow，与 gorm Scan 同口径）。Redis 是唯一
// 真源，本文件零 SQL import；表达式/条件只复用写路径已有的 Go 求值语义。
// ============================================================================

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"touchgocore/metrics"
)

// specJoin 一条已校验的 JOIN：别名、左右列、左表别名、类型。
type specJoin struct {
	typ         string
	alias       string
	bind        *binding
	leftAlias   string
	leftColumn  string
	rightColumn string
}

// querySpec 执行读形状扩展，全程 Redis 权威（无数据库后端）。
func querySpec[T any](e *Engine, ctx context.Context, table string, cond any, out any, o *Options) error {
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	if o.Agg == nil && len(o.Selects) == 0 && !o.AllowAnyDest && b.typ != typeOf[T]() {
		return ErrTypeMismatch
	}
	resolver := map[string]*binding{}
	baseAlias := o.BaseAlias
	baseRef := b.table
	if baseAlias != "" {
		if !columnNameRe.MatchString(baseAlias) {
			return ErrBadColumn
		}
		resolver[baseAlias] = b
		baseRef = baseAlias
	} else {
		resolver[b.table] = b
	}
	joins := make([]specJoin, 0, len(o.Joins))
	for _, j := range o.Joins {
		typ := strings.ToUpper(j.Type)
		if typ == "" {
			typ = "LEFT"
		}
		if typ != "LEFT" && typ != "INNER" {
			return ErrBadColumn
		}
		jb, err := e.lookupTable(j.Table)
		if err != nil {
			return err
		}
		alias := j.Alias
		if alias == "" {
			alias = j.Table
		}
		if !columnNameRe.MatchString(alias) {
			return ErrBadColumn
		}
		leftAlias := j.LeftTable
		if leftAlias == "" {
			leftAlias = baseRef
		}
		lb, ok := resolver[leftAlias]
		if !ok || lb == nil {
			return ErrBadColumn
		}
		if validateColumn(lb, j.LeftColumn) != nil || validateColumn(jb, j.RightColumn) != nil {
			return ErrBadColumn
		}
		resolver[alias] = jb
		joins = append(joins, specJoin{typ: typ, alias: alias, bind: jb, leftAlias: leftAlias, leftColumn: j.LeftColumn, rightColumn: j.RightColumn})
	}
	var vals []condVal
	if cond != nil {
		vals, err = parseCond(b.meta, b.kind, cond)
		if err != nil {
			return err
		}
	}
	if e.cfg.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.QueryTimeout)
		defer cancel()
	}
	// FromSource 且无秘密投影：基表与 JOIN 表都以源库为真值，读后尽力回填。
	// WithSecrets 时源行拿不到 :s: 秘密列，回退 Redis 权威读，避免登录摘要被抹空。
	fromSource := o.FromSource && !o.WithSecrets
	baseRows, err := e.loadSpecBaseRows(ctx, b, vals, o.WithSecrets, fromSource, o.IncludeTombstoned)
	if err != nil {
		return err
	}
	combined, err := e.joinSpecRows(ctx, b, baseRef, joins, baseRows, fromSource)
	if err != nil {
		return err
	}
	conds, anyOf, err := specConds(b, resolver, vals, o)
	if err != nil {
		return err
	}
	filtered := make([]map[string]string, 0, len(combined))
	for _, row := range combined {
		if evalConds(row, conds, anyOf) {
			filtered = append(filtered, row)
		}
	}
	ev := &specEval{base: b, resolver: resolver}
	var result []map[string]string
	if specIsAggregate(o) {
		result, err = specAggregate(ev, filtered, o)
		if err != nil {
			return err
		}
		sortSpecRows(result, o.Orders)
	} else {
		sortSpecRows(filtered, o.Orders)
		result, err = specProject(ev, b, filtered, o)
		if err != nil {
			return err
		}
	}
	result = specPage(result, o)
	return scanSpecResult(out, result, o)
}

// loadSpecBaseRows 按读模式加载 spec 基表行：FromSource 走源库并回填，否则读
// Redis 键集。
func (e *Engine) loadSpecBaseRows(ctx context.Context, b *binding, vals []condVal, withSecrets, fromSource, includeTombstoned bool) ([]map[string]string, error) {
	if fromSource && b.srcFindList != nil {
		return e.loadSpecRowsFromSource(ctx, b, vals, includeTombstoned)
	}
	return e.loadSpecRows(ctx, b, withSecrets)
}

// loadSpecRowsFromSource 从源表读取行并按条件求值前的基础行：逐行做墓碑门控
// （已删除但 sink 尚未落库的行不得复活），再尽力回填 Redis（save NX，不覆盖
// 更新值）。返回值始终是源库行，保证列表/聚合读到 DB 真值。
func (e *Engine) loadSpecRowsFromSource(ctx context.Context, b *binding, vals []condVal, includeTombstoned bool) ([]map[string]string, error) {
	rowsAny, err := b.srcFindList(ctx, vals)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(rowsAny))
	for _, rowAny := range rowsAny {
		key, kerr := b.keyFromRow(rowAny)
		if kerr != nil {
			return nil, kerr
		}
		tomb, terr := e.store.TombAlive(ctx, b.keys.tombPrefix+key)
		if terr != nil {
			return nil, terr
		}
		if tomb && !includeTombstoned {
			continue
		}
		fields, ferr := b.encodeFields(rowAny)
		if ferr != nil {
			return nil, ferr
		}
		// 列表/聚合消费的是本次源库结果；不逐行回填 Redis：大表全量扫描会给脏账
		// 制造上万条 NX 写，定时 sink 与显式 flush 都被拖住。需要写穿的场景走
		// 刷新入口（refreshRow）按主键单行 save。
		out = append(out, fieldsToMap(fields))
	}
	return out, nil
}

// loadSpecRows 键集扫描 + 逐行 HGetAll 取一张表的全部存活行（MaxScanRows fail-closed）。
// withSecrets 为真时把该行 :s: 秘密 hash 的白名单列并入行 map（仅基表调用开启；
// JOIN 表恒 false，秘密列不跨表泄漏）。
func (e *Engine) loadSpecRows(ctx context.Context, b *binding, withSecrets bool) ([]map[string]string, error) {
	keys, err := e.store.KeySetScan(ctx, b.keys.keySet, e.cfg.MaxScanRows)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExceeded) {
			metrics.DBCache.IncScanBudget(b.table)
		}
		return nil, err
	}
	sort.Strings(keys)
	rows := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		h, herr := e.store.HGetAll(ctx, b.keys.rowPrefix+key)
		if herr != nil {
			return nil, herr
		}
		if len(h) == 0 {
			continue
		}
		if withSecrets && len(b.secrets) > 0 {
			sec, serr := e.store.HGetAll(ctx, b.keys.secretPrefix+key)
			if serr != nil {
				return nil, serr
			}
			for col := range b.secrets {
				if v, ok := sec[col]; ok {
					h[col] = v
				}
			}
		}
		rows = append(rows, h)
	}
	return rows, nil
}

// joinSpecRows 把基表行展开成带限定列（alias.col）的组合行；基表列同时保留裸名。
// LEFT JOIN 无匹配时右表限定列缺席（空串=SQL NULL）；INNER 无匹配丢弃左行；
// 多匹配做笛卡尔展开。被 join 表各扫描一次（受 MaxScanRows 预算约束）。
func (e *Engine) joinSpecRows(ctx context.Context, b *binding, baseRef string, joins []specJoin, baseRows []map[string]string, fromSource bool) ([]map[string]string, error) {
	combined := make([]map[string]string, 0, len(baseRows))
	for _, row := range baseRows {
		m := make(map[string]string, len(row)*2)
		for col, v := range row {
			m[col] = v
			m[baseRef+"."+col] = v
		}
		combined = append(combined, m)
	}
	for _, j := range joins {
		rightRows, err := e.loadSpecBaseRows(ctx, j.bind, nil, false, fromSource, false)
		if err != nil {
			return nil, err
		}
		next := make([]map[string]string, 0, len(combined))
		for _, left := range combined {
			lv := left[j.leftAlias+"."+j.leftColumn]
			matched := false
			for _, right := range rightRows {
				if right[j.rightColumn] != lv || lv == "" {
					continue
				}
				matched = true
				m := make(map[string]string, len(left)+len(right))
				for k, v := range left {
					m[k] = v
				}
				for col, v := range right {
					m[j.alias+"."+col] = v
				}
				next = append(next, m)
			}
			if !matched && j.typ == "LEFT" {
				next = append(next, left)
			}
		}
		combined = next
	}
	return combined, nil
}

// specConds 把等值条件 + Ranges/Conditions + AnyOfGroups 翻成对组合行求值的 condDesc。
// 限定列（Range.Table 非空）按该别名绑定校验类型，Col 前缀 alias.；裸列按基表。
func specConds(b *binding, resolver map[string]*binding, vals []condVal, o *Options) ([]condDesc, [][]condDesc, error) {
	conds, err := condValsToDescs(b, vals)
	if err != nil {
		return nil, nil, err
	}
	conv := func(r Range) (condDesc, error) {
		target := b
		if r.Table != "" {
			ob, ok := resolver[r.Table]
			if !ok || ob == nil {
				return condDesc{}, ErrBadColumn
			}
			target = ob
		}
		c, cerr := condDescFromRange(target, r)
		if cerr != nil {
			return condDesc{}, cerr
		}
		if r.Table != "" {
			c.Col = r.Table + "." + r.Column
		}
		return c, nil
	}
	for _, r := range o.Ranges {
		c, cerr := conv(r)
		if cerr != nil {
			return nil, nil, cerr
		}
		conds = append(conds, c)
	}
	for _, r := range o.Conditions {
		c, cerr := conv(r)
		if cerr != nil {
			return nil, nil, cerr
		}
		conds = append(conds, c)
	}
	var anyOf [][]condDesc
	for _, grp := range o.AnyOfGroups {
		if len(grp) == 0 {
			return nil, nil, ErrBadColumn
		}
		g := make([]condDesc, 0, len(grp))
		for _, r := range grp {
			c, cerr := conv(r)
			if cerr != nil {
				return nil, nil, cerr
			}
			g = append(g, c)
		}
		anyOf = append(anyOf, g)
	}
	return conds, anyOf, nil
}

// specIsAggregate 报告是否聚合查询：显式 Agg / 分组 / Select 含聚合节点。
func specIsAggregate(o *Options) bool {
	if o.Agg != nil || len(o.Groups) > 0 || len(o.GroupAliases) > 0 {
		return true
	}
	for _, it := range o.Selects {
		if exprHasAggregate(it.Expr) {
			return true
		}
	}
	return false
}

func exprHasAggregate(e Expr) bool {
	switch x := e.(type) {
	case sumExpr, countExpr:
		return true
	case binExpr:
		return exprHasAggregate(x.l) || exprHasAggregate(x.r)
	case callExpr:
		for _, a := range x.args {
			if exprHasAggregate(a) {
				return true
			}
		}
	}
	return false
}

// specProject 逐行投影（非聚合）：有 Selects 按表达式求值取列，否则取基表整行。
func specProject(ev *specEval, b *binding, rows []map[string]string, o *Options) ([]map[string]string, error) {
	out := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		if len(o.Selects) == 0 {
			m := make(map[string]string, len(b.meta.Fields))
			for _, f := range b.meta.Fields {
				m[f.Column] = row[f.Column]
			}
			if o.WithSecrets {
				for col := range b.secrets {
					m[col] = row[col]
				}
			}
			out = append(out, m)
			continue
		}
		m := make(map[string]string, len(o.Selects))
		for _, it := range o.Selects {
			name, err := specSelectName(it)
			if err != nil {
				return nil, err
			}
			v, verr := ev.scalar(row, it.Expr)
			if verr != nil {
				return nil, verr
			}
			m[name] = encodeResult(v)
		}
		out = append(out, m)
	}
	return out, nil
}

// specSelectName 投影列名：别名优先，否则裸 Col 用列名（其余表达式必须给别名）。
func specSelectName(it SelectItem) (string, error) {
	if it.Alias != "" {
		if !columnNameRe.MatchString(it.Alias) {
			return "", ErrBadColumn
		}
		return it.Alias, nil
	}
	if c, ok := it.Expr.(colExpr); ok && c.table == "" {
		return c.name, nil
	}
	return "", fmt.Errorf("%w: select expression needs alias", ErrBadColumn)
}

// specAggregate 分组聚合：按 Groups 列 / GroupAliases 投影值分桶（无分组=单桶），
// 每桶算 Select 表达式 / Agg，输出列含分组键与投影别名。
func specAggregate(ev *specEval, rows []map[string]string, o *Options) ([]map[string]string, error) {
	type group struct {
		rows []map[string]string
		keys map[string]string
	}
	var order []string
	groups := map[string]*group{}
	for _, row := range rows {
		keys := map[string]string{}
		var parts []string
		for _, col := range o.Groups {
			if err := validateColumn(ev.base, col); err != nil {
				return nil, err
			}
			keys[col] = row[col]
			parts = append(parts, row[col])
		}
		for _, alias := range o.GroupAliases {
			it, ok := specSelectByAlias(o.Selects, alias)
			if !ok {
				return nil, fmt.Errorf("%w: group alias %q not selected", ErrBadColumn, alias)
			}
			v, err := ev.scalar(row, it.Expr)
			if err != nil {
				return nil, err
			}
			s := encodeResult(v)
			keys[alias] = s
			parts = append(parts, s)
		}
		k := strings.Join(parts, "\x00")
		g, ok := groups[k]
		if !ok {
			g = &group{keys: keys}
			groups[k] = g
			order = append(order, k)
		}
		g.rows = append(g.rows, row)
	}
	out := make([]map[string]string, 0, len(order))
	for _, k := range order {
		g := groups[k]
		m := make(map[string]string, len(g.keys)+len(o.Selects)+1)
		for col, v := range g.keys {
			m[col] = v
		}
		if len(o.Selects) > 0 {
			for _, it := range o.Selects {
				name, err := specSelectName(it)
				if err != nil {
					return nil, err
				}
				v, verr := ev.aggregate(g.rows, it.Expr)
				if verr != nil {
					return nil, verr
				}
				m[name] = encodeResult(v)
			}
		} else if o.Agg != nil {
			alias := o.Agg.Alias
			if alias == "" {
				alias = "total"
			}
			if !columnNameRe.MatchString(alias) {
				return nil, ErrBadColumn
			}
			v, err := ev.aggFn(g.rows, o.Agg)
			if err != nil {
				return nil, err
			}
			m[alias] = encodeResult(v)
		}
		out = append(out, m)
	}
	return out, nil
}

func specSelectByAlias(selects []SelectItem, alias string) (SelectItem, bool) {
	for _, it := range selects {
		if it.Alias == alias {
			return it, true
		}
	}
	return SelectItem{}, false
}

// sortSpecRows 按 Orders 稳定排序：两值皆可解析为数值则数值比较，否则字典序
// （日期串字典序=时间序）；限定列用 alias.col 键，别名/裸列用列名键。
func sortSpecRows(rows []map[string]string, orders []Order) {
	if len(orders) == 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, ord := range orders {
			key := ord.Column
			if ord.Table != "" {
				key = ord.Table + "." + ord.Column
			}
			a, b := rows[i][key], rows[j][key]
			cmp := specCompare(a, b)
			if cmp == 0 {
				continue
			}
			if ord.Desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
}

func specCompare(a, b string) int {
	af, aerr := strconv.ParseFloat(a, 64)
	bf, berr := strconv.ParseFloat(b, 64)
	if aerr == nil && berr == nil {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

func specPage(rows []map[string]string, o *Options) []map[string]string {
	if o.Offset > 0 {
		if o.Offset >= len(rows) {
			rows = nil
		} else {
			rows = rows[o.Offset:]
		}
	}
	if o.Limit > 0 && len(rows) > o.Limit {
		rows = rows[:o.Limit]
	}
	return rows
}

// scanSpecResult 把结果列映射扫描进 out（*T / *[]T / *标量 / *map），按目标结构
// 自身的列名规则（buildMeta）匹配，与 gorm Scan 同口径；未投影列保持零值。
func scanSpecResult(out any, rows []map[string]string, o *Options) error {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrBadOut
	}
	elem := rv.Elem()
	switch elem.Kind() {
	case reflect.Slice:
		et := elem.Type().Elem()
		res := reflect.MakeSlice(elem.Type(), 0, len(rows))
		for _, row := range rows {
			item := reflect.New(et).Elem()
			if err := scanSpecRow(item, row, o); err != nil {
				return err
			}
			res = reflect.Append(res, item)
		}
		elem.Set(res)
		return nil
	case reflect.Struct:
		if len(rows) == 0 {
			return nil
		}
		return scanSpecRow(elem, rows[0], o)
	case reflect.Map:
		if len(rows) == 0 {
			return nil
		}
		return scanSpecMap(elem, rows[0])
	default:
		if len(rows) == 0 {
			return nil
		}
		raw := specScalarColumn(rows[0], o)
		val, err := decodeValue(raw, elem.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDecode, err)
		}
		elem.Set(val)
		return nil
	}
}

func scanSpecRow(dest reflect.Value, row map[string]string, o *Options) error {
	switch dest.Kind() {
	case reflect.Struct:
		meta, err := buildMeta(dest.Type(), nil, backendMySQL)
		if err != nil {
			return err
		}
		return decodeRow(meta, dest, row)
	case reflect.Map:
		return scanSpecMap(dest, row)
	case reflect.Pointer:
		if dest.IsNil() {
			dest.Set(reflect.New(dest.Type().Elem()))
		}
		return scanSpecRow(dest.Elem(), row, o)
	default:
		raw := specScalarColumn(row, o)
		val, err := decodeValue(raw, dest.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDecode, err)
		}
		dest.Set(val)
		return nil
	}
}

func scanSpecMap(dest reflect.Value, row map[string]string) error {
	if dest.Type().Key().Kind() != reflect.String {
		return ErrBadOut
	}
	m := reflect.MakeMapWithSize(dest.Type(), len(row))
	for k, v := range row {
		val, err := decodeValue(v, dest.Type().Elem())
		if err != nil {
			return fmt.Errorf("%w: column %s: %v", ErrDecode, k, err)
		}
		m.SetMapIndex(reflect.ValueOf(k).Convert(dest.Type().Key()), val)
	}
	dest.Set(m)
	return nil
}

// specScalarColumn 取单列结果的列值：Agg 别名优先，否则结果首列（按名排序稳定）。
func specScalarColumn(row map[string]string, o *Options) string {
	if o.Agg != nil {
		alias := o.Agg.Alias
		if alias == "" {
			alias = "total"
		}
		if v, ok := row[alias]; ok {
			return v
		}
	}
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return row[keys[0]]
}

// encodeResult 把求值结果编码成结果列字符串（nil→空串=NULL 哨兵）。
// SUM/AVG 走 float64：整数值回退到 int64 定点串，避免 FormatFloat('g') 对
// ≥1e6 产出科学计数法（"9.99e+08"）导致下游 ParseInt 失败。
func encodeResult(v any) string {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok && f == math.Trunc(f) && f >= float64(math.MinInt64) && f < float64(math.MaxInt64) {
		return strconv.FormatInt(int64(f), 10)
	}
	s, err := encodeValue(reflect.ValueOf(v))
	if err != nil {
		return ""
	}
	return s
}

// specEval 表达式求值上下文：基表绑定 + 别名→绑定解析器。
type specEval struct {
	base     *binding
	resolver map[string]*binding
}

func (ev *specEval) colBinding(table string) *binding {
	if table == "" {
		return ev.base
	}
	return ev.resolver[table]
}

// scalar 逐行求值标量表达式（col/lit/sub/greatest/coalesce/dateBucket）。
func (ev *specEval) scalar(row map[string]string, e Expr) (any, error) {
	switch x := e.(type) {
	case colExpr:
		b := ev.colBinding(x.table)
		if b == nil {
			return nil, ErrBadColumn
		}
		if err := validateColumn(b, x.name); err != nil {
			return nil, err
		}
		key := x.name
		if x.table != "" {
			key = x.table + "." + x.name
		}
		raw := row[key]
		if raw == "" {
			return nil, nil
		}
		ft := b.meta.Fields[b.meta.ByColumn[x.name]].Type
		if colTypeFor(ft) == "num" {
			// 整数列保留整数类型：抹成 float64 会让 encodeResult 对 ≥1e6 的整值产出
			// 科学计数法（FormatFloat 'g'），结果列解码回 int64 时 ParseInt 失败
			// （生产 member id 为 11 位）。float/bool 列仍走 float64 数值域。
			switch ft.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				n, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return nil, nil
				}
				return n, nil
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				n, err := strconv.ParseUint(raw, 10, 64)
				if err != nil {
					return nil, nil
				}
				return n, nil
			default:
				f, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					return nil, nil
				}
				return f, nil
			}
		}
		return raw, nil
	case litExpr:
		return x.v, nil
	case binExpr:
		l, err := ev.scalar(row, x.l)
		if err != nil {
			return nil, err
		}
		r, err := ev.scalar(row, x.r)
		if err != nil {
			return nil, err
		}
		return specBin(x.op, l, r)
	case callExpr:
		args := make([]any, 0, len(x.args))
		for _, a := range x.args {
			v, err := ev.scalar(row, a)
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
		return specCall(x.fn, args)
	case dateBucketExpr:
		if err := validateColumn(ev.base, x.col); err != nil {
			return nil, err
		}
		raw := row[x.col]
		if raw == "" {
			return nil, nil
		}
		tv, err := decodeTime(raw, timeType)
		if err != nil {
			return nil, nil
		}
		t := tv.Interface().(time.Time).Add(time.Duration(x.offsetSec) * time.Second)
		return t.Format("2006-01-02"), nil
	case sumExpr, countExpr:
		return nil, fmt.Errorf("%w: aggregate in scalar context", ErrBadColumn)
	default:
		return nil, fmt.Errorf("%w: unknown expr %T", ErrBadColumn, e)
	}
}

// aggregate 对一组行求值聚合表达式（sum/count 跨行，标量节点取组首行）。
func (ev *specEval) aggregate(rows []map[string]string, e Expr) (any, error) {
	switch x := e.(type) {
	case sumExpr:
		var sum float64
		seen := false
		for _, row := range rows {
			v, err := ev.scalar(row, x.arg)
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			sum += specFloat(v)
			seen = true
		}
		if !seen {
			return nil, nil
		}
		return sum, nil
	case countExpr:
		if _, ok := x.arg.(starExpr); ok {
			return int64(len(rows)), nil
		}
		seen := map[string]bool{}
		var n int64
		for _, row := range rows {
			v, err := ev.scalar(row, x.arg)
			if err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			if x.distinct {
				k := encodeResult(v)
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			n++
		}
		return n, nil
	case binExpr:
		l, err := ev.aggregate(rows, x.l)
		if err != nil {
			return nil, err
		}
		r, err := ev.aggregate(rows, x.r)
		if err != nil {
			return nil, err
		}
		return specBin(x.op, l, r)
	case callExpr:
		args := make([]any, 0, len(x.args))
		for _, a := range x.args {
			v, err := ev.aggregate(rows, a)
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
		return specCall(x.fn, args)
	case colExpr, litExpr, dateBucketExpr:
		if len(rows) == 0 {
			if l, ok := e.(litExpr); ok {
				return l.v, nil
			}
			return nil, nil
		}
		return ev.scalar(rows[0], e)
	default:
		return nil, fmt.Errorf("%w: unknown aggregate expr %T", ErrBadColumn, e)
	}
}

// aggFn 求值 Aggregate（Fn/Column 或结构化 Expr）。
func (ev *specEval) aggFn(rows []map[string]string, a *Aggregate) (any, error) {
	if a.Expr != nil {
		return ev.aggregate(rows, a.Expr)
	}
	fn := strings.ToUpper(a.Fn)
	switch fn {
	case "COUNT", "SUM", "MIN", "MAX", "AVG":
	default:
		return nil, ErrBadColumn
	}
	if a.Column == "" {
		if fn == "COUNT" {
			return int64(len(rows)), nil
		}
		return nil, ErrBadColumn
	}
	if err := validateColumn(ev.base, a.Column); err != nil {
		return nil, err
	}
	if fn == "COUNT" && a.DistinctNotNull {
		return ev.aggregate(rows, countExpr{arg: colExpr{name: a.Column}, distinct: true})
	}
	var vals []float64
	for _, row := range rows {
		raw := row[a.Column]
		if raw == "" {
			continue
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		vals = append(vals, f)
	}
	switch fn {
	case "COUNT":
		return int64(len(vals)), nil
	case "SUM":
		var s float64
		for _, v := range vals {
			s += v
		}
		return s, nil
	case "AVG":
		if len(vals) == 0 {
			return nil, nil
		}
		var s float64
		for _, v := range vals {
			s += v
		}
		return s / float64(len(vals)), nil
	case "MIN":
		if len(vals) == 0 {
			return nil, nil
		}
		m := vals[0]
		for _, v := range vals[1:] {
			m = math.Min(m, v)
		}
		return m, nil
	case "MAX":
		if len(vals) == 0 {
			return nil, nil
		}
		m := vals[0]
		for _, v := range vals[1:] {
			m = math.Max(m, v)
		}
		return m, nil
	}
	return nil, ErrBadColumn
}

func specBin(op string, l, r any) (any, error) {
	switch op {
	case "-":
		return specFloat(l) - specFloat(r), nil
	default:
		return nil, fmt.Errorf("%w: bin op %q", ErrBadColumn, op)
	}
}

func specCall(fn string, args []any) (any, error) {
	switch fn {
	case "COALESCE":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "GREATEST":
		if len(args) == 0 {
			return nil, nil
		}
		m := specFloat(args[0])
		for _, a := range args[1:] {
			m = math.Max(m, specFloat(a))
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%w: call %q", ErrBadColumn, fn)
	}
}

func specFloat(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case bool:
		if x {
			return 1
		}
		return 0
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return 0
}
