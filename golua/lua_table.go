package lua

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"touchgocore/syncmap"
	"touchgocore/vars"

	rt "github.com/arnodel/golua/runtime"
)

// maxLuaConvDepth 是 Go 值折成 Lua 表的下潜层数上限。
// 现网最深的真实形状是 DialogItem{Param1 *LuaTable{…[{…}]}} 约 3~4 层，8 有余量。
const maxLuaConvDepth = 8

// populateTable 填充 LuaTable 数据（内部辅助函数）。
//
// 折不动的类型（chan/func）与超深的子树会被丢弃并记一条错误：本函数没有错误
// 返回通道，调用方拿到的就是一张缺了那一项的表。
func populateTable(tbl *LuaTable, data interface{}) {
	if data == nil {
		return
	}
	if err := fillTable(context.Background(), tbl, reflect.ValueOf(data), 0, map[uintptr]*LuaTable{}); err != nil {
		vars.Error("Lua 表填充失败: %v", err)
	}
}

// fillTable 把复合值（map / slice / array / struct 及指向它们的指针）展开进 tbl。
func fillTable(ctx context.Context, tbl *LuaTable, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		switch rv.Kind() {
		case reflect.Interface:
			if rv.IsNil() {
				return nil
			}
			rv = rv.Elem()
			continue
		case reflect.Pointer:
			if rv.IsNil() {
				return nil
			}
			// 环与菱形共享：同一个指针只展开一次，重复出现直接复用那张表
			addr := rv.Pointer()
			if seen != nil {
				if prev, ok := seen[addr]; ok {
					*tbl = *prev
					return nil
				}
			}
			markSeen(seen, addr, tbl)
			rv = rv.Elem()
			continue
		}
		break
	}

	switch rv.Kind() {
	case reflect.Struct:
		return fillStruct(ctx, tbl, rv, depth, seen)
	case reflect.Map:
		return fillMap(ctx, tbl, rv, depth, seen)
	case reflect.Slice, reflect.Array:
		return fillSlice(ctx, tbl, rv, depth, seen)
	default:
		return fmt.Errorf("%s 不是表结构，无法展开成 Lua 表", rv.Kind())
	}
}

// fillMap 按键值展开 map。键原样交给 Set，由 Set 统一归一化键类型。
// 折不动的值（chan/func 或空切片）仍占一个键、值为 nil，与既有 Lua 侧
// 「键在、读出来是 nil」的形状一致。
func fillMap(ctx context.Context, tbl *LuaTable, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) error {
	iter := rv.MapRange()
	for iter.Next() {
		key, keyOK, err := goTableKey(ctx, iter.Key(), depth+1, seen)
		if err != nil {
			return err
		}
		if !keyOK {
			continue // 复合键 Lua 侧无法表达
		}
		value, _, err := goTableValue(ctx, iter.Value(), depth+1, seen)
		if err != nil {
			return fmt.Errorf("键 %v: %w", key, err)
		}
		tbl.Set(key, value)
	}
	return nil
}

// fillStruct 按字段展开结构体。键名口径见 luaFieldName。
func fillStruct(ctx context.Context, tbl *LuaTable, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) error {
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, exposed := luaFieldName(field)
		if !exposed {
			continue
		}
		value, _, err := goTableValue(ctx, rv.Field(i), depth+1, seen)
		if err != nil {
			return fmt.Errorf("字段 %s: %w", field.Name, err)
		}
		tbl.Set(name, value)
	}
	return nil
}

// fillSlice 展开 slice/array。下标统一用 int64 顺序键，Lua 侧 ipairs / # 才能用。
func fillSlice(ctx context.Context, tbl *LuaTable, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) error {
	for i := 0; i < rv.Len(); i++ {
		value, ok, err := goTableValue(ctx, rv.Index(i), depth+1, seen)
		if err != nil {
			return fmt.Errorf("元素 %d: %w", i+1, err)
		}
		if !ok {
			value = nil
		}
		tbl.Set(int64(i+1), value)
	}
	return nil
}

// goTableValue 把单个 Go 值折成「可以直接放进 LuaTable」的形式：
// 标量原样给出，map/slice/array/struct（及其指针）折成 *LuaTable。
// ok=false 表示这个值不进表；*LuaTable 与 *syncmap.MapAny 一律保留引用不复制，
// 脚本侧与 Go 侧看到的必须同一份数据。
func goTableValue(ctx context.Context, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) (interface{}, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !rv.IsValid() {
		return nil, false, nil
	}

	// 具体类型优先：引用必须原样传递
	switch v := rv.Interface().(type) {
	case *LuaTable:
		if v == nil {
			return nil, false, nil
		}
		return v, true, nil
	case *syncmap.MapAny:
		if v == nil {
			return nil, false, nil
		}
		return &LuaTable{tbl: v}, true, nil
	}

	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		if rv.Kind() == reflect.Pointer && !isScalarKind(rv.Elem().Kind()) {
			if depth >= maxLuaConvDepth {
				return nil, false, fmt.Errorf("嵌套超过 %d 层，已截断", maxLuaConvDepth)
			}
			addr := rv.Pointer()
			if prev, ok := seenTable(seen, addr); ok {
				return prev, true, nil
			}
			nested := newTable(nil)
			markSeen(seen, addr, nested)
			if err := fillTable(ctx, nested, rv.Elem(), depth, seen); err != nil {
				return nil, false, err
			}
			if !nested.HasData() {
				return nil, false, nil
			}
			return nested, true, nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), true, nil
	case reflect.String:
		return rv.String(), true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return int64(rv.Uint()), true, nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true, nil
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		if depth >= maxLuaConvDepth {
			return nil, false, fmt.Errorf("嵌套超过 %d 层，已截断", maxLuaConvDepth)
		}
		nested := newTable(nil)
		if err := fillTable(ctx, nested, rv, depth, seen); err != nil {
			return nil, false, err
		}
		if !nested.HasData() {
			return nil, false, nil
		}
		return nested, true, nil
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil, false, fmt.Errorf("不支持的类型 %s", rv.Type())
	default:
		return nil, false, fmt.Errorf("不支持的类型 %s", rv.Kind())
	}
}

// goTableKey 折 map 的键：标量直接给，指针/接口先剥开；复合键 Lua 侧无法表达，丢弃。
func goTableKey(ctx context.Context, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) (interface{}, bool, error) {
	key, ok, err := goTableValue(ctx, rv, depth, seen)
	if err != nil || !ok {
		return nil, false, err
	}
	switch key.(type) {
	case string, int64, float64, bool:
		return key, true, nil
	}
	return nil, false, nil
}

// luaFieldName 返回结构体字段暴露给 Lua 的键名。
//
// 口径是 json tag 优先（非空且非 "-"，去掉 ,omitempty 这类后缀），否则用 Go 导出字段名。
// 选这个口径是因为 mapmanager 里所有 Lua/Go 共享结构（ShopItem / DialogItem /
// DialogOption / Npc）的字段本来就逐个带 snake_case 的 json tag：同一份数据在
// JSON 里是 item_id，在 Lua 里也必须是 item_id，不能让脚本看到第三套名字。
// json:"-" 的字段（如 Npc.Shop / Npc.Dialog 那两把 syncmap 指针）与非导出字段跳过。
func luaFieldName(field reflect.StructField) (string, bool) {
	if field.PkgPath != "" { // 非导出字段
		return "", false
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		tag = tag[:comma]
	}
	if tag != "" {
		return tag, true
	}
	return field.Name, true
}

// isScalarKind 报告该 Kind 是否可以直接作为 Lua 标量给出（指针解引用到这一层就停）。
func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

func markSeen(seen map[uintptr]*LuaTable, addr uintptr, tbl *LuaTable) {
	if seen == nil {
		return
	}
	seen[addr] = tbl
}

func seenTable(seen map[uintptr]*LuaTable, addr uintptr) (*LuaTable, bool) {
	if seen == nil {
		return nil, false
	}
	prev, ok := seen[addr]
	return prev, ok
}

// newTable 从 Go 数据创建 LuaTable 对象
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
	populateTable(tbl, data)
	return tbl
}

// tableFromRuntime 从 arnodel/golua 的 rt.Table 转换为 *LuaTable
func tableFromRuntime(ctx context.Context, tbl *rt.Table) *LuaTable {
	if tbl == nil {
		return nil
	}

	result := newTable(nil)

	// 使用 Next 遍历 table
	var k rt.Value = rt.NilValue
	for {
		key, val, ok := tbl.Next(k)
		if !ok || key == rt.NilValue {
			break
		}

		goKey := LuaToGoValueWithContext(ctx, key)
		if goKey == nil {
			// 函数/协程这类键没有 Go 表示，跳过而不是当成非法键报错
			k = key
			continue
		}

		// 检查值是否是嵌套 table
		if t, ok := val.TryTable(); ok {
			nestedTbl := tableFromRuntime(ctx, t)
			result.Set(goKey, nestedTbl)
		} else {
			goValue := LuaToGoValueWithContext(ctx, val)
			result.Set(goKey, goValue)
		}

		k = key
	}

	return result
}

type LuaTable struct {
	tbl *syncmap.MapAny
}

// HasData 检查 table 是否有数据
func (lt *LuaTable) HasData() bool {
	return lt.tbl != nil && lt.tbl.Length() > 0
}

// Append 添加列表元素。
//
// 键类型统一为 int64：Lua 数字经 LuaToGoValue 转回 Go 侧就是 int64，
// GetArray 也按 int64 识别数组下标。旧实现用 Go int 作键，Go 侧 Append
// 建的表在 GetArray 里永远读回空数组。
func (lt *LuaTable) Append(val interface{}) {
	if lt.tbl == nil {
		lt.tbl = syncmap.NewAny()
	}
	lt.Set(int64(lt.tbl.Length()+1), val)
}

// Set 设置键值对。
//
// 键统一经 normalizeTableKey 归一化：Go 侧 int、int64 与 Lua 回传的数字必须落在
// 同一个条目上，否则 Set(1, v) 之后 Get(int64(1)) 读不到。非法键丢弃并记日志
// （签名里没有 error 通道，不能 panic）。
func (lt *LuaTable) Set(key, val interface{}) {
	normKey, ok := normalizeTableKey(key)
	if !ok {
		vars.Error("LuaTable 收到非法键 %T(%v)，已丢弃", key, key)
		return
	}
	if lt.tbl == nil {
		lt.tbl = syncmap.NewAny()
	}
	lt.tbl.Store(normKey, val)
}

// ToTable 转换为 arnodel/golua 的 rt.Table
func (lt *LuaTable) ToTable() *rt.Table {
	return lt.ToTableWithContext(context.Background())
}

// ToTableWithContext 使用上下文转换为 arnodel/golua 的 rt.Table
func (lt *LuaTable) ToTableWithContext(ctx context.Context) *rt.Table {
	return lt.toTableSeen(ctx, map[*syncmap.MapAny]*rt.Table{})
}

// toTableSeen 把中间表折成 rt.Table。
//
// seen 按底层 *syncmap.MapAny 去重，原因有两条：
//   - Go 侧的环（struct 自指折出来的表会把自己作为某个键的值）必须复用同一张
//     rt.Table，否则这里无限递归直接把栈打爆；
//   - 两个 LuaTable 可以包着同一个 *syncmap.MapAny（菱形共享），按包装器指针
//     去重认不出来。
//
// golua 的表允许成环，Lua 侧读这样的表没有问题（只是不能整体序列化/打印）。
func (lt *LuaTable) toTableSeen(ctx context.Context, seen map[*syncmap.MapAny]*rt.Table) *rt.Table {
	if lt.tbl == nil {
		return rt.NewTable()
	}
	if prev, ok := seen[lt.tbl]; ok {
		return prev
	}

	tbl := rt.NewTable()
	seen[lt.tbl] = tbl

	lt.tbl.Range(func(key, value interface{}) bool {
		luaKey := GoToLuaValueWithContext(ctx, key)

		switch v := value.(type) {
		case *LuaTable:
			if v == nil {
				tbl.Set(luaKey, rt.NilValue)
				return true
			}
			tbl.Set(luaKey, rt.TableValue(v.toTableSeen(ctx, seen)))
		case *syncmap.MapAny:
			if v == nil {
				tbl.Set(luaKey, rt.NilValue)
				return true
			}
			tbl.Set(luaKey, rt.TableValue((&LuaTable{tbl: v}).toTableSeen(ctx, seen)))
		default:
			tbl.Set(luaKey, GoToLuaValueWithContext(ctx, value))
		}
		return true
	})

	return tbl
}

// PushTable 保持向后兼容的函数（已废弃）
// 在新版本中，使用 ToTable() 替代
func (lt *LuaTable) PushTable(L interface{}) bool {
	// 为了向后兼容，接受第一个参数但不使用
	_ = L
	return lt.HasData()
}

// SubTable 获取或创建嵌套 table
func (lt *LuaTable) SubTable(key interface{}) *LuaTable {
	normKey, ok := normalizeTableKey(key)
	if !ok {
		// 非法键给一张游离表：调用方后续写入不会污染本表，也不会 panic
		return newTable(nil)
	}
	if lt.tbl == nil {
		lt.tbl = syncmap.NewAny()
	}
	data, _ := lt.tbl.LoadOrStore(normKey, newTable(nil))
	return data.(*LuaTable)
}

// Length 返回 table 长度
func (lt *LuaTable) Length() int {
	if lt.tbl == nil {
		return 0
	}
	return lt.tbl.Length()
}

// Get 获取 table 中的值（键口径与 Set 一致）
func (lt *LuaTable) Get(key interface{}) (interface{}, bool) {
	if lt.tbl == nil {
		return nil, false
	}
	normKey, ok := normalizeTableKey(key)
	if !ok {
		return nil, false
	}
	return lt.tbl.Load(normKey)
}

// GetInt 获取 int64 类型的值
func (lt *LuaTable) GetInt(key interface{}) (int64, bool) {
	val, ok := lt.Get(key)
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// GetString 获取 string 类型的值
func (lt *LuaTable) GetString(key interface{}) (string, bool) {
	val, ok := lt.Get(key)
	if !ok {
		return "", false
	}
	if str, ok := val.(string); ok {
		return str, true
	}
	return "", false
}

// GetTable 获取嵌套的 LuaTable
func (lt *LuaTable) GetTable(key interface{}) (*LuaTable, bool) {
	val, ok := lt.Get(key)
	if !ok {
		return nil, false
	}
	if tbl, ok := val.(*LuaTable); ok {
		return tbl, true
	}
	return nil, false
}

// GetArray 获取数组形式的值。
//
// 两遍法保证顺序与 syncmap.Range 的遍历顺序无关：第一遍把 int/int64 下标收进
// map（Append 统一用 int64，Set 是公开入口、历史调用可能传 int，一并识别），
// 第二遍从 1 开始顺次取出，遇空洞即止（Lua 数组语义）。
func (lt *LuaTable) GetArray() []interface{} {
	if lt.tbl == nil {
		return nil
	}

	entries := make(map[int64]interface{})
	var maxIdx int64
	lt.tbl.Range(func(key, value interface{}) bool {
		var idx int64
		switch k := key.(type) {
		case int64:
			idx = k
		case int:
			idx = int64(k)
		default:
			return true
		}
		if idx > 0 {
			entries[idx] = value
			if idx > maxIdx {
				maxIdx = idx
			}
		}
		return true
	})

	arr := make([]interface{}, 0, maxIdx)
	for i := int64(1); i <= maxIdx; i++ {
		v, ok := entries[i]
		if !ok {
			break
		}
		arr = append(arr, v)
	}
	return arr
}

// Keys 获取所有 key
func (lt *LuaTable) Keys() []interface{} {
	if lt.tbl == nil {
		return nil
	}

	keys := make([]interface{}, 0)
	lt.tbl.Range(func(key, _ interface{}) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values 获取所有 value
func (lt *LuaTable) Values() []interface{} {
	if lt.tbl == nil {
		return nil
	}

	values := make([]interface{}, 0)
	lt.tbl.Range(func(_, value interface{}) bool {
		values = append(values, value)
		return true
	})
	return values
}

// ToMap 转换为 map[string]interface{}（仅限 string key）
func (lt *LuaTable) ToMap() map[string]interface{} {
	if lt.tbl == nil {
		return nil
	}

	result := make(map[string]interface{})
	lt.tbl.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if nestedTbl, ok := value.(*LuaTable); ok {
				result[k] = nestedTbl.ToMap()
			} else {
				result[k] = value
			}
		}
		return true
	})
	return result
}

// Merge 合并另一个 LuaTable
func (lt *LuaTable) Merge(other *LuaTable) {
	if lt.tbl == nil {
		lt.tbl = syncmap.NewAny()
	}
	if other.tbl != nil {
		other.tbl.Range(func(key, value interface{}) bool {
			normKey, ok := normalizeTableKey(key)
			if !ok {
				return true
			}
			lt.tbl.Store(normKey, value)
			return true
		})
	}
}

// normalizeTableKey 是表键的唯一口径。
//
// 整数族与整值浮点统一成 int64（Lua 的数字经 LuaToGoValue 回转也是 int64），
// string/bool 原样，其余（nil、NaN）判为非法。写入点与读取点必须同口径，
// 否则 Set(1, v) 之后 Get(int64(1)) 读不到——此前 Append 用 int64 键、
// Set 收任意键、CompareKeys 又不认 int，同一个下标能存在三种表征。
func normalizeTableKey(key interface{}) (interface{}, bool) {
	switch k := key.(type) {
	case string:
		return k, true
	case bool:
		return k, true
	case int:
		return int64(k), true
	case int8:
		return int64(k), true
	case int16:
		return int64(k), true
	case int32:
		return int64(k), true
	case int64:
		return k, true
	case uint:
		return int64(k), true
	case uint8:
		return int64(k), true
	case uint16:
		return int64(k), true
	case uint32:
		return int64(k), true
	case uint64:
		return int64(k), true
	case float32:
		return normalizeFloatKey(float64(k))
	case float64:
		return normalizeFloatKey(k)
	default:
		return nil, false
	}
}

// normalizeFloatKey 把整值浮点折成 int64（Lua 里 1 与 1.0 是同一个键），
// 非整值与 ±Inf 保留为 float64，NaN 不是合法键。
func normalizeFloatKey(f float64) (interface{}, bool) {
	if math.IsNaN(f) {
		return nil, false
	}
	if !math.IsInf(f, 0) && f == math.Trunc(f) {
		return int64(f), true
	}
	return f, true
}

// Clear 清空 table
func (lt *LuaTable) Clear() {
	if lt.tbl != nil {
		lt.tbl.ClearAll(nil)
	}
}
