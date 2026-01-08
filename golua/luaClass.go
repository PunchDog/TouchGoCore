//go:build lua54

package lua

import (
	"fmt"
	"reflect"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
	"unsafe"

	"github.com/aarzilli/golua/lua"
)

// 注册类接口
type ILuaClassInterface interface {
	Init(id int64, luascript *LuaScript)
	Delete()
	Update()
}

// 注册类接口基类
type ILuaClassObject struct {
}

func (this *ILuaClassObject) Delete() {
}

func (this *ILuaClassObject) Update() {
}

// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////
// userdata 元数据，用于关联 Go 对象和 Lua userdata
type userdataMeta struct {
	uid    int64
	script *LuaScript
	// 缓存反射信息，避免重复反射
	reflectType reflect.Type
	reflectValue reflect.Value
}

// 类注册信息，用于存储类的元数据
type classRegistry struct {
	className string
	methods   map[string]reflect.Method
}

// 全局类注册表，避免重复创建
var (
	_classRegistry = syncmap.Map{} // key: className, value: *classRegistry
)

// 创建回调函数
type metaOperate struct {
	methodName string
}

// 函数默认interface{}类型的number识别为int64,返回值是table的话，目前只支持*syncmap.Map,并且key目前只能是string
func (this *metaOperate) callBack(L *lua.State) int {
	// 获取 userdata 元数据
	userdataPtr := L.ToUserdata(1)
	if userdataPtr == nil {
		vars.Error("LUA回调%s: userdata为空", this.methodName)
		return 0
	}

	meta := *(*userdataMeta)(userdataPtr)

	// 从 syncmap 中获取实际对象
	dataRaw, ok := meta.script.defaultLuaData.Load(meta.uid)
	if !ok {
		vars.Error("LUA回调%s: 找不到uid=%d的对象", this.methodName, meta.uid)
		return 0
	}

	data, ok := dataRaw.(ILuaClassInterface)
	if !ok {
		vars.Error("LUA回调%s: uid=%d的对象未实现ILuaClassInterface", this.methodName, meta.uid)
		return 0
	}

	// 获取类反射信息
	rcvr := reflect.ValueOf(data)

	// 获取方法反射
	method := rcvr.MethodByName(this.methodName)
	if !method.IsValid() {
		vars.Error("LUA回调%s: 方法不存在", this.methodName)
		return 0
	}

	// 处理输入参数
	args := make([]reflect.Value, 0, method.Type().NumIn())
	luaArgCount := L.GetTop()

	// Lua 参数从索引 2 开始（索引 1 是对象自身）
	for i := 0; i < method.Type().NumIn() && (i+2) <= luaArgCount; i++ {
		luaIdx := i + 2
		paramType := method.Type().In(i)
		argValue, err := convertLuaValue(L, luaIdx, paramType)
		if err != nil {
			vars.Error("LUA回调%s: 参数%d转换失败: %v", this.methodName, i+1, err)
			return 0
		}
		args = append(args, argValue)
	}

	// 调用原始函数
	resultValues := method.Call(args)

	// 处理返回值
	for i, result := range resultValues {
		if result.Kind() == reflect.Invalid {
			vars.Error("LUA回调函数%s返回值%d无效", this.methodName, i+1)
			return 0
		}

		if !pushValue(L, result.Interface()) {
			vars.Error("LUA回调函数%s返回值%d处理失败,类型:%v 值:%v",
				this.methodName, i+1, result.Type(), result.Interface())
			return 0
		}
	}

	return len(resultValues)
}

// convertLuaValue 将 Lua 值转换为 Go 类型的反射值
func convertLuaValue(L *lua.State, idx int, targetType reflect.Type) (reflect.Value, error) {
	luaType := L.Type(idx)

	switch luaType {
	case lua.LUA_TBOOLEAN:
		if targetType.Kind() == reflect.Bool {
			return reflect.ValueOf(L.ToBoolean(idx)), nil
		}
		return reflect.Value{}, fmt.Errorf("无法将boolean转换为%s", targetType.Kind())

	case lua.LUA_TSTRING:
		if targetType.Kind() == reflect.String {
			return reflect.ValueOf(L.ToString(idx)), nil
		}
		return reflect.Value{}, fmt.Errorf("无法将string转换为%s", targetType.Kind())

	case lua.LUA_TNUMBER:
		num := L.ToNumber(idx)
		switch targetType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			converted := util.ConvertToKind(num, targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			converted := util.ConvertToKind(num, targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		case reflect.Float32, reflect.Float64:
			return reflect.ValueOf(num).Convert(targetType), nil
		case reflect.Interface:
			return reflect.ValueOf(num), nil
		default:
			return reflect.Value{}, fmt.Errorf("无法将number转换为%s", targetType.Kind())
		}

	case lua.LUA_TTABLE:
		table := getTable(L, idx)
		if table == nil {
			return reflect.Value{}, fmt.Errorf("无法读取table")
		}

		// 检查目标类型是否是 *LuaTable
		if targetType.Kind() == reflect.Ptr && targetType.Elem() == reflect.TypeOf(LuaTable{}) {
			return reflect.ValueOf(table), nil
		}

		// 检查目标类型是否是 *syncmap.Map
		if targetType.Kind() == reflect.Ptr && targetType.Elem() == reflect.TypeOf(syncmap.Map{}) {
			return reflect.ValueOf(table.tbl), nil
		}

		return reflect.Value{}, fmt.Errorf("不支持的table转换类型: %v", targetType)

	case lua.LUA_TNIL:
		return reflect.Zero(targetType), nil

	default:
		return reflect.Value{}, fmt.Errorf("不支持的Lua类型: %v", luaType)
	}
}

// pushValue 将 Go 值压入 Lua 栈
func pushValue(L *lua.State, val interface{}) bool {
	if val == nil {
		L.PushNil()
		return true
	}

	switch v := val.(type) {
	case string:
		L.PushString(v)
		return true

	case int, int8, int16, int32, int64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		L.PushInteger(d)
		return true

	case uint, uint8, uint16, uint32, uint64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		L.PushInteger(d)
		return true

	case bool:
		L.PushBoolean(v)
		return true

	case float32:
		L.PushNumber(float64(v))
		return true

	case float64:
		L.PushNumber(v)
		return true

	case *LuaTable:
		if v != nil {
			return v.PushTable(L)
		}
		L.PushNil()
		return true

	case *syncmap.Map:
		if v != nil {
			tbl := &LuaTable{tbl: v}
			return tbl.PushTable(L)
		}
		L.PushNil()
		return true

	default:
		// 尝试将其他类型转换为 table
		tbl := newTable(val)
		if tbl.HaveData() {
			return tbl.PushTable(L)
		}

		vars.Error("不支持的Go类型: %T, 值: %v", val, val)
		return false
	}
}

// 创建一个类注册
func newLuaClass(class ILuaClassInterface, script *LuaScript) error {
	script.defaultLuaDataUid++

	// 获取类名和方法列表
	className, methodNames := util.GetClassName(class)

	// 检查是否已存在同名类（避免重复注册）
	if _, exists := script.l.GetGlobal(className); exists != 0 {
		vars.Warn("类 %s 已注册，跳过重复注册", className)
		return nil
	}

	// 创建类构造函数
	constructor := func(L *lua.State) int {
		// 创建新实例
		classType := reflect.TypeOf(class).Elem()
		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 初始化对象
		cls.Init(script.defaultLuaDataUid, script)

		// 存储到 syncmap 中
		script.defaultLuaData.Store(script.defaultLuaDataUid, cls)

		// 创建 userdata 并设置元表
		meta := &userdataMeta{
			uid:         script.defaultLuaDataUid,
			script:      script,
			reflectType: reflect.TypeOf(cls),
		}

		// 分配 userdata 内存
		userdataPtr := script.l.NewUserdata(uintptr(unsafe.Sizeof(meta)))
		*(*userdataMeta)(userdataPtr) = *meta

		// 设置元表
		script.l.LGetMetaTable(className)
		if script.l.IsNil(-1) {
			vars.Error("类 %s 的元表不存在", className)
			return 0
		}
		script.l.SetMetaTable(-2)

		return 1
	}

	// 创建析构函数（__gc 元方法）
	destructor := func(L *lua.State) int {
		userdataPtr := L.ToUserdata(1)
		if userdataPtr == nil {
			return 0
		}

		meta := *(*userdataMeta)(userdataPtr)

		// 从 syncmap 中获取并删除对象
		if dataRaw, ok := meta.script.defaultLuaData.Load(meta.uid); ok {
			if data, ok := dataRaw.(ILuaClassInterface); ok {
				// 调用对象的 Delete 方法
				data.Delete()
				// 从 map 中删除
				meta.script.defaultLuaData.Delete(meta.uid)
			}
		}

		return 0
	}

	// 创建 __index 元方法
	indexMethod := func(L *lua.State) int {
		// 检查栈顶是否是字符串（方法名）
		if L.Type(2) != lua.LUA_TSTRING {
			return 0
		}

		methodName := L.ToString(2)

		// 尝试从元表获取方法
		L.GetMetaTable(1)
		L.PushString(methodName)
		L.GetTable(-2)

		// 如果找到方法，返回
		if L.IsFunction(-1) {
			L.Remove(-2) // 移除元表
			return 1
		}

		// 没找到方法，返回 nil
		L.Pop(2) // 移除结果和元表
		return 0
	}

	// 注册构造函数到全局
	script.l.PushGoFunction(constructor)
	script.l.SetGlobal(className)

	// 创建或获取类的元表
	script.l.NewMetaTable(className)

	// 注册 __gc 元方法
	script.l.PushString("__gc")
	script.l.PushGoFunction(destructor)
	script.l.SetTable(-3)

	// 注册 __index 元方法
	script.l.PushString("__index")
	script.l.PushGoFunction(indexMethod)
	script.l.SetTable(-3)

	// 注册 __tostring 元方法（用于调试）
	script.l.PushString("__tostring")
	script.l.PushGoFunction(func(L *lua.State) int {
		script.l.PushString(fmt.Sprintf("%s userdata", className))
		return 1
	})
	script.l.SetTable(-3)

	// 注册所有方法到元表
	for _, methodName := range methodNames {
		// 跳过 Delete、Update 等基础方法（如果不需要）
		// 如果需要注册所有方法，可以不加过滤

		script.l.PushString(methodName)
		methodMeta := &metaOperate{methodName: methodName}
		script.l.PushGoFunction(methodMeta.callBack)
		script.l.SetTable(-3)
	}

	// 弹出元表
	script.l.Pop(1)

	vars.Info("成功注册 Lua 类: %s, 方法数: %d", className, len(methodNames))

	return nil
}
