package lua

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/arnodel/golua/lib"
	rt "github.com/arnodel/golua/runtime"
)

// 全局 Lua 实例管理
var (
	defaultLua     *LuaScript = nil
	luaInstances   map[int64]*LuaScript
	luaInstancesMu sync.RWMutex // 保护 luaInstances 的并发访问
	nextInstanceID atomic.Int64
)

// 注册的函数和类
var (
	registeredFuncs     map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)
	registeredFuncsMu   sync.RWMutex // 保护 registeredFuncs 的并发访问
	registeredClasses   map[ILuaClassInterface]bool
	registeredClassesMu sync.RWMutex // 保护 registeredClasses 的并发访问
)

// luaTimer 定时器结构
type luaTimer struct {
	localtimer.TimerInterface
	tick      atomic.Int64
	luaScript *LuaScript
	ctx       context.Context
}

// Tick 执行定时更新
func (lt *luaTimer) Tick() {
	tick := lt.tick.Add(1)

	// 定期触发垃圾回收
	if tick%GCTickCount == 0 {
		runtime.GC()
	}

	// 使用工作池并发更新对象
	lt.updateObjectsConcurrently(lt.ctx)
}

// updateObjectsConcurrently 并发更新对象
func (lt *luaTimer) updateObjectsConcurrently(ctx context.Context) {
	var workerCount int
	maxWorkers := runtime.NumCPU()
	sem := make(chan struct{}, maxWorkers)

	lt.luaScript.registeredObjects.Range(func(key, value interface{}) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		obj, ok := value.(ILuaClassInterface)
		if !ok {
			return true
		}

		workerCount++
		sem <- struct{}{}

		go func(k interface{}, o ILuaClassInterface) {
			defer func() { <-sem }()

			// 调用旧版Update方法以保持兼容
			o.Update()
		}(key, obj)

		return true
	})

	// 等待所有worker完成
	for i := 0; i < workerCount; i++ {
		<-sem
	}
}

// LuaScript Lua脚本实例
type LuaScript struct {
	runtime           *rt.Runtime
	thread            *rt.Thread
	returnValues      []interface{}
	initScriptPath    string
	registeredObjects *syncmap.MapAny
	timer             *luaTimer
	UID               int64
	env               *rt.Table
	ctx               context.Context
	cancel            context.CancelFunc
}

// Init 初始化Lua运行时
func (ls *LuaScript) Init() error {
	ls.Close()

	// 创建上下文
	ls.ctx, ls.cancel = context.WithCancel(context.Background())

	// 创建新的运行时
	ls.runtime = rt.New(os.Stdout)
	ls.thread = ls.runtime.MainThread()
	ls.env = ls.runtime.GlobalEnv()

	// 注册标准库
	lib.LoadAll(ls.runtime)()

	// 注册内置函数
	if err := registerDefaultFunctions(ls); err != nil {
		return fmt.Errorf("register default functions failed: %w", err)
	}

	return nil
}

// Close 关闭Lua运行时
func (ls *LuaScript) Close() {
	if ls.cancel != nil {
		ls.cancel()
	}

	if ls.runtime != nil {
		if ls.timer != nil {
			ls.timer.Remove()
		}
		ls.runtime = nil
		ls.thread = nil
		ls.env = nil
	}
}

// Call 调用 Lua 函数
func (ls *LuaScript) Call(funcname string, list ...interface{}) ([]interface{}, error) {
	return ls.CallWithContext(context.Background(), funcname, list...)
}

// CallWithContext 使用上下文调用 Lua 函数
func (ls *LuaScript) CallWithContext(ctx context.Context, funcname string, list ...interface{}) ([]interface{}, error) {
	select {
	case <-ls.ctx.Done():
		return nil, fmt.Errorf("Lua script is closed")
	default:
	}

	// 从全局环境获取函数
	funcVal := ls.env.Get(rt.StringValue(funcname))
	if funcVal == rt.NilValue {
		return nil, fmt.Errorf("function not found: '%s'", funcname)
	}

	// 检查是否是函数类型
	if _, ok := funcVal.TryCallable(); !ok {
		return nil, fmt.Errorf("'%s' is not a callable function", funcname)
	}

	// 转换参数为 rt.Value
	args := make([]rt.Value, 0, len(list))
	for _, val := range list {
		args = append(args, GoToLuaValueWithContext(ctx, val))
	}

	// 调用 Lua 函数（带超时保护）
	resultChan := make(chan rt.Value, 1)
	errChan := make(chan error, 1)

	go func() {
		result, err := rt.Call1(ls.thread, funcVal, args...)
		if err != nil {
			errChan <- err
		} else {
			resultChan <- result
		}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("call timeout: %w", ctx.Err())
	case result := <-resultChan:
		ls.returnValues = []interface{}{LuaToGoValueWithContext(ctx, result)}
		return ls.returnValues, nil
	case err := <-errChan:
		return nil, fmt.Errorf("Lua call failed: %w", err)
	}
}

// NewLuaScript 创建一个 Lua 脚本实例
func NewLuaScript(initluapath string) (*LuaScript, error) {
	return NewLuaScriptWithContext(context.Background(), initluapath)
}

// NewLuaScriptWithContext 使用上下文创建 Lua 脚本实例
func NewLuaScriptWithContext(ctx context.Context, initluapath string) (*LuaScript, error) {
	p := &LuaScript{
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initluapath,
		registeredObjects: syncmap.NewAny(),
	}

	// 初始化Lua运行时
	if err := p.Init(); err != nil {
		return nil, err
	}

	// 初始化注册的函数
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		p.runtime.SetEnvGoFunc(p.env, funcName, function, 1, false)
	}
	registeredFuncsMu.RUnlock()

	// 注册类
	registeredClassesMu.RLock()
	for class := range registeredClasses {
		if err := registerClassWithContext(ctx, class, p); err != nil {
			vars.Error("register Lua class failed: %v", err)
		}
	}
	registeredClassesMu.RUnlock()

	// 读取并编译脚本文件
	source, err := os.ReadFile(p.initScriptPath)
	if err != nil {
		return nil, fmt.Errorf("read Lua script file failed: %w", err)
	}

	chunk, err := p.runtime.CompileAndLoadLuaChunk("main", source, rt.TableValue(p.env))
	if err != nil {
		return nil, fmt.Errorf("compile Lua script failed: %w", err)
	}

	// 执行主脚本（带超时保护）
	if _, err = rt.Call1(p.thread, rt.FunctionValue(chunk)); err != nil {
		return nil, fmt.Errorf("execute Lua script failed: %w", err)
	}

	// 创建定时器
	tmr, err := localtimer.NewTimer(UpdateIntervalMs, -1, &luaTimer{})
	if err != nil {
		return nil, fmt.Errorf("create timer failed: %w", err)
	}
	p.timer = tmr.(*luaTimer)
	p.timer.luaScript = p
	p.timer.ctx = p.ctx
	localtimer.AddTimer(p.timer)

	// 加入管理列表
	instanceID := nextInstanceID.Add(1)
	luaInstancesMu.Lock()
	luaInstances[instanceID] = p
	luaInstancesMu.Unlock()
	p.UID = instanceID
	return p, nil
}

// Call 调用默认 Lua 实例的函数
func Call(funcName string, args ...interface{}) ([]interface{}, error) {
	return CallWithContext(context.Background(), funcName, args...)
}

// CallWithContext 使用上下文调用默认 Lua 实例的函数
func CallWithContext(ctx context.Context, funcName string, args ...interface{}) ([]interface{}, error) {
	luaInstancesMu.RLock()
	dl := defaultLua
	luaInstancesMu.RUnlock()
	if dl == nil {
		return nil, fmt.Errorf("Lua service not started")
	}
	return dl.CallWithContext(ctx, funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例
func RegisterLuaFunc(funcName string, function func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)) error {
	registeredFuncsMu.Lock()
	defer registeredFuncsMu.Unlock()

	if registeredFuncs == nil {
		registeredFuncs = make(map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error))
	}
	if _, ok := registeredFuncs[funcName]; ok {
		return fmt.Errorf("function '%s' already registered", funcName)
	}
	registeredFuncs[funcName] = function
	return nil
}

// RegisterLuaClass 注册一个类到所有 Lua 实例
func RegisterLuaClass(class ILuaClassInterface) error {
	if class == nil {
		return fmt.Errorf("cannot register nil class")
	}

	className, err := util.GetClassName(class)
	if err != nil {
		return fmt.Errorf("get class name failed: %w", err)
	}

	registeredClassesMu.Lock()
	defer registeredClassesMu.Unlock()

	if registeredClasses == nil {
		registeredClasses = make(map[ILuaClassInterface]bool)
	}
	if _, ok := registeredClasses[class]; ok {
		return fmt.Errorf("class '%s' already registered", className)
	}
	registeredClasses[class] = true
	return nil
}

// RunLua 启动 Lua 服务
func RunLua() error {
	if config.Cfg_.Lua == "off" {
		vars.Info("Lua service disabled")
		return nil
	}

	luaInstancesMu.Lock()
	luaInstances = make(map[int64]*LuaScript)
	luaInstancesMu.Unlock()

	var err error
	dl, err := NewLuaScript(config.Cfg_.Lua)
	if err != nil {
		return fmt.Errorf("create Lua script failed: %w", err)
	}

	luaInstancesMu.Lock()
	defaultLua = dl
	luaInstancesMu.Unlock()

	vars.Info("Lua service started successfully")
	return nil
}

// StopLua 关闭所有的定时器
func StopLua() {
	luaInstancesMu.Lock()
	defer luaInstancesMu.Unlock()

	for _, ls := range luaInstances {
		ls.Close()
	}
	luaInstances = nil
	defaultLua = nil
}

// registerDefaultFunctions 注册默认的内置函数
func registerDefaultFunctions(ls *LuaScript) error {
	ls.runtime.SetEnvGoFunc(ls.env, "info", info, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "debug", debug, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "error", error1, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "dofile", dofile, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "getpathluafile", getpathluafile, 1, false)
	return nil
}

// registerClassWithContext 使用上下文注册一个 Go 类到 Lua 脚本
func registerClassWithContext(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	// 获取类名
	className, err := util.GetClassName(class)
	if err != nil {
		return fmt.Errorf("get class name failed: %w", err)
	}

	// 检查是否已存在同名类（避免重复注册）
	existing := script.env.Get(rt.StringValue(className))
	if existing != rt.NilValue {
		vars.Info("class '%s' already registered, skipping", className)
		return nil
	}

	// 创建或获取类注册信息
	var registry *ClassRegistry
	rawRegistry, loaded := classRegistryMap.Load(className)
	if loaded {
		registry = rawRegistry.(*ClassRegistry)
	} else {
		// 创建新的类注册信息
		classType := reflect.TypeOf(class).Elem()
		registry = &ClassRegistry{
			Name:        className,
			reflectType: classType,
			methodCache: nil, // 延迟构建
		}
		classRegistryMap.Store(className, registry)
		registry.buildMethodCache() // 立即构建方法缓存
		vars.Info("registered Lua class: %s with %d methods", className, len(registry.methodCache))
	}

	// 创建类构造函数
	constructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		classType := reflect.TypeOf(class).Elem()
		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 初始化对象（调用旧版Init方法以保持兼容）
		cls.Init(script.UID, script)

		// 存储到 syncmap 中
		script.registeredObjects.Store(script.UID, cls)

		// 创建元数据
		meta := &userDataMeta{
			uid:    script.UID,
			script: script,
		}

		// 创建 UserData
		userData := rt.NewUserData(meta, nil)

		next := c.Next()
		t.Push1(next, rt.UserDataValue(userData))
		return next, nil
	}

	// 创建析构函数（__gc 元方法）
	destructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		arg0 := c.Arg(0)

		userData, ok := arg0.TryUserData()
		if !ok || userData == nil {
			return c.Next(), nil
		}

		meta, ok := userData.Value().(*userDataMeta)
		if !ok {
			return c.Next(), nil
		}

		// 从 syncmap 中获取并删除对象
		if dataRaw, ok := meta.script.registeredObjects.Load(meta.uid); ok {
			if data, ok := dataRaw.(ILuaClassInterface); ok {
				// 调用旧版Delete方法以保持兼容
				data.Delete()
				meta.script.registeredObjects.Delete(meta.uid)
			}
		}

		return c.Next(), nil
	}

	// 创建 __index 元方法
	indexMethod := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		arg1 := c.Arg(1)
		methodName, ok := arg1.TryString()
		if !ok {
			return c.Next(), nil
		}

		// 从缓存检查方法是否存在
		if _, ok := registry.getMethod(methodName); !ok {
			return c.Next(), nil
		}

		// 创建方法闭包
		methodMeta := &methodCallback{methodName: methodName, ctx: ctx}
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, false)

		next := c.Next()
		t.Push1(next, rt.FunctionValue(methodFunc))
		return next, nil
	}

	// 注册构造函数到全局
	constructorFunc := rt.NewGoFunction(constructor, className, 0, false)
	script.runtime.SetEnv(script.env, className, rt.FunctionValue(constructorFunc))

	// 创建类的元表
	metaTable := rt.NewTable()

	// 注册 __gc 元方法
	script.runtime.SetEnv(metaTable, "__gc", rt.FunctionValue(rt.NewGoFunction(destructor, "__gc", 1, false)))

	// 注册 __index 元方法
	script.runtime.SetEnv(metaTable, "__index", rt.FunctionValue(rt.NewGoFunction(indexMethod, "__index", 2, false)))

	// 注册 __tostring 元方法
	tostringFunc := rt.NewGoFunction(func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		next := c.Next()
		t.Push1(next, rt.StringValue(fmt.Sprintf("%s userdata", className)))
		return next, nil
	}, "__tostring", 1, false)
	script.runtime.SetEnv(metaTable, "__tostring", rt.FunctionValue(tostringFunc))

	// 注册所有方法到元表（从缓存）
	for methodName := range registry.methodCache {
		methodMeta := &methodCallback{methodName: methodName, ctx: ctx}
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, false)
		script.runtime.SetEnv(metaTable, methodName, rt.FunctionValue(methodFunc))
	}

	return nil
}
