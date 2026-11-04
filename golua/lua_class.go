package lua

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"touchgocore/syncmap"
	"touchgocore/vars"

	rt "github.com/arnodel/golua/runtime"
)

// ILuaClassInterface 注册类接口（保持向后兼容）
type ILuaClassInterface interface {
	Init(id int64, luascript *LuaScript)
	Delete()
	Update()
}

// ILuaClassObject 注册类接口基类（保持向后兼容）
type ILuaClassObject struct{}

func (l *ILuaClassObject) Delete() {
}

func (l *ILuaClassObject) Update() {
}

// userDataMeta 元数据，用于关联 Go 对象和 Lua userdata
type userDataMeta struct {
	uid    int64
	script *LuaScript
}

// ClassRegistry 类注册信息
type ClassRegistry struct {
	Name string
	// reflectType 是类的指针类型（如 *Npc）：只有它的方法集才包含指针接收者
	// 方法，用结构体类型枚举会得到空方法表，Lua 侧一个方法都调不到。
	reflectType reflect.Type
	methodCache map[string]reflect.Method
	methodMutex sync.RWMutex
}

// 全局类注册表，避免重复创建
var (
	classRegistryMap = syncmap.NewAny() // key: className, value: *ClassRegistry
)

// lifecycleMethods 是 ILuaClassInterface 的框架方法，由 Go 侧驱动，
// 暴露给 Lua 等于让脚本自己改写对象的登记键与生命周期。
var lifecycleMethods = map[string]bool{
	"Init":   true,
	"Delete": true,
	"Update": true,
}

// getMethod 获取缓存的反射方法
func (cr *ClassRegistry) getMethod(methodName string) (reflect.Method, bool) {
	cr.methodMutex.RLock()
	method, ok := cr.methodCache[methodName]
	cr.methodMutex.RUnlock()
	return method, ok
}

// buildMethodCache 构建方法缓存
func (cr *ClassRegistry) buildMethodCache() {
	cr.methodMutex.Lock()
	defer cr.methodMutex.Unlock()

	if cr.methodCache != nil {
		return // 已经构建过
	}

	cr.methodCache = make(map[string]reflect.Method)
	for i := 0; i < cr.reflectType.NumMethod(); i++ {
		method := cr.reflectType.Method(i)
		if method.PkgPath != "" { // 只导出方法
			continue
		}
		if lifecycleMethods[method.Name] {
			continue
		}
		cr.methodCache[method.Name] = method
	}
}

// methodNames 返回暴露给 Lua 的方法名快照。
func (cr *ClassRegistry) methodNames() []string {
	cr.methodMutex.RLock()
	defer cr.methodMutex.RUnlock()
	names := make([]string, 0, len(cr.methodCache))
	for name := range cr.methodCache {
		names = append(names, name)
	}
	return names
}

// methodCallback 是 Lua→Go 方法调用的派发入口，每个「类×方法」一个实例。
//
// 类名与方法在注册期一次取好存进来。此前每次调用都要 util.GetClassName(data)
// 反查类名、再查两张表，而 GetClassName 会 reflect.TypeOf+Indirect+Name 并遍历
// NumMethod() 构造一整条方法名切片（第二个返回值在桥接层四个调用点全被丢弃），
// 只为拿一个类名字符串。
type methodCallback struct {
	methodName string
	className  string
	method     reflect.Method
	fixed      int // 定长段形参个数（含接收者），注册期算好
	ctx        context.Context
}

// newMethodCallback 在注册期固化方法句柄与元数。
func newMethodCallback(className string, method reflect.Method, ctx context.Context) *methodCallback {
	numIn := method.Type.NumIn()
	fixed := numIn
	if method.Type.IsVariadic() {
		fixed = numIn - 1 // 末尾的切片在可变段里凑
	}
	return &methodCallback{
		methodName: method.Name,
		className:  className,
		method:     method,
		fixed:      fixed,
		ctx:        ctx,
	}
}

// argAt 取第 i 个实参（i=0 是接收者）。
// 本方法按「fixed 个具名槽 + etc」绑定，越出具名槽的下标要从 etc 里取。
// 调用前必须已过 CheckNArgs(fixed)：Arg 不做任何边界检查。
func (mc *methodCallback) argAt(c *rt.GoCont, i int) rt.Value {
	if i < mc.fixed {
		return c.Arg(i)
	}
	return c.Etc()[i-mc.fixed]
}

// callBack 处理 Lua 到 Go 的方法调用。
//
// 注册时按「fixed 个具名槽 + hasEtc=true」绑定，fixed 由反射从真实签名算出。
// hasEtc 必须为真：否则超出的实参在 golua 侧被直接丢弃，这里再也报不出「参数过多」。
func (mc *methodCallback) callBack(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	if err := mc.checkLive(); err != nil {
		return nil, err
	}

	// 必须先查再取：槽位不足时 c.Arg(i) 直接下标越界 panic
	if err := c.CheckNArgs(mc.fixed); err != nil {
		return nil, errMethodArity(mc.methodName, mc.fixed, c.NArgs())
	}
	total := c.NArgs() + len(c.Etc())
	if !mc.method.Type.IsVariadic() && total > mc.fixed {
		return nil, errMethodArity(mc.methodName, mc.fixed, total)
	}

	obj, err := mc.resolveReceiver(c)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		// 对象已不存在：按既有语义推空返回，Lua 侧拿到 nil 而不是报错
		return c.Next(), nil
	}

	args, err := mc.buildArgs(c, obj, total)
	if err != nil {
		return nil, err
	}

	var results []reflect.Value
	if mc.method.Type.IsVariadic() {
		results = mc.method.Func.CallSlice(args)
	} else {
		results = mc.method.Func.Call(args)
	}

	next := c.Next()
	if err := mc.pushResults(t, next, results); err != nil {
		return nil, err
	}
	return next, nil
}

// checkLive 报告宿主脚本是否已关闭。
func (mc *methodCallback) checkLive() error {
	select {
	case <-mc.ctx.Done():
		return errScriptClosed
	default:
	}
	return nil
}

// resolveReceiver 从 0 号具名槽里的 userdata 取回 Go 对象；取不到时返回 (nil, nil)。
func (mc *methodCallback) resolveReceiver(c *rt.GoCont) (ILuaClassInterface, error) {
	userData, ok := c.Arg(0).TryUserData()
	if !ok {
		return nil, fmt.Errorf("invalid userdata: not a userdata type")
	}
	if userData == nil {
		return nil, nil
	}

	meta, ok := userData.Value().(*userDataMeta)
	if !ok {
		return nil, fmt.Errorf("invalid userdata metadata type")
	}

	dataRaw, ok := meta.script.registeredObjects.Load(meta.uid)
	if !ok {
		vars.Error("object not found: uid=%d class=%s method=%s", meta.uid, mc.className, mc.methodName)
		return nil, nil
	}

	data, ok := dataRaw.(ILuaClassInterface)
	if !ok {
		vars.Error("object does not implement ILuaClassInterface: uid=%d", meta.uid)
		return nil, nil
	}
	return data, nil
}

// buildArgs 组装反射调用入参：第 0 个是接收者，其余按形参类型逐个转换。
//
// 指针类型的方法以「静态函数」形式反射调用，第一个入参是接收者，
// 必须显式补上；否则 Call 会拿 Lua 的实参去填接收者位置。
func (mc *methodCallback) buildArgs(c *rt.GoCont, obj ILuaClassInterface, total int) ([]reflect.Value, error) {
	sig := mc.method.Type

	recv := reflect.ValueOf(obj)
	if !recv.Type().AssignableTo(sig.In(0)) {
		return nil, fmt.Errorf("receiver type mismatch for method '%s' of class '%s': got %s, want %s",
			mc.methodName, mc.className, recv.Type(), sig.In(0))
	}

	args := make([]reflect.Value, 0, mc.fixed+1)
	args = append(args, recv)
	for i := 1; i < mc.fixed; i++ {
		argValue, err := luaArgToReflect(mc.ctx, mc.argAt(c, i), sig.In(i))
		if err != nil {
			return nil, fmt.Errorf("parameter %d conversion failed: %w", i, err)
		}
		args = append(args, argValue)
	}

	if sig.IsVariadic() {
		sliceType := sig.In(sig.NumIn() - 1)
		variadicTail := reflect.MakeSlice(sliceType, 0, total-mc.fixed)
		for i := mc.fixed; i < total; i++ {
			argValue, err := luaArgToReflect(mc.ctx, mc.argAt(c, i), sliceType.Elem())
			if err != nil {
				return nil, fmt.Errorf("variadic parameter %d conversion failed: %w", i-mc.fixed+1, err)
			}
			variadicTail = reflect.Append(variadicTail, argValue)
		}
		args = append(args, variadicTail)
	}
	return args, nil
}

// pushResults 把反射返回值逐个转成 Lua 值推回调用方。
//
// 转换失败不向 Lua 抛错：脚本契约里这些方法一直是有值就有值、没有就是 nil，
// 中途改成运行时错误会让既有脚本的分支全变。原因记进日志。
func (mc *methodCallback) pushResults(t *rt.Thread, next rt.Cont, results []reflect.Value) error {
	for i, result := range results {
		if result.Kind() == reflect.Invalid {
			return fmt.Errorf("return value %d is invalid", i+1)
		}
		luaValue, err := GoToLuaValueErr(mc.ctx, result.Interface())
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			vars.Error("方法 %s.%s 的第 %d 个返回值转换失败: %v", mc.className, mc.methodName, i+1, err)
		}
		t.Push1(next, luaValue)
	}
	return nil
}

// luaArgToReflect 转换单个 Lua 实参。interface 形参拿到 nil 时 reflect.ValueOf
// 会返回无效值，直接交给 Call 就是 panic，这里回退成零值。
//
// 转换结果必须自己可赋值给目标形参：非空 interface 形参拿到不相干的类型时，
// reflect.Call 会 panic 把整个进程带下去，这里提前报错。
func luaArgToReflect(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	argValue, err := LuaToReflectValueWithContext(ctx, v, targetType)
	if err != nil {
		return reflect.Value{}, err
	}
	if !argValue.IsValid() {
		return reflect.Zero(targetType), nil
	}
	if !argValue.Type().AssignableTo(targetType) {
		return reflect.Value{}, fmt.Errorf("实参类型 %s 无法赋给形参类型 %s", argValue.Type(), targetType)
	}
	return argValue, nil
}
