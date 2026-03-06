package lua

import (
	"reflect"
	"sync"
	"touchgocore/util"
)

// MethodCache 缓存类的反射方法
type MethodCache struct {
	Methods      map[string]reflect.Method
	MethodNames  []string
	Once         sync.Once
	initialized  bool
}

// GetMethodCache 获取类的方法缓存
func GetMethodCache(class ILuaClassInterface) *MethodCache {
	className, _ := util.GetClassName(class)

	// 从全局注册表获取
	raw, ok := classRegistryMap.Load(className)
	if !ok {
		return nil
	}

	registry := raw.(*ClassRegistry)
	return registry.MethodCache
}

// CreateMethodCache 创建方法缓存
func CreateMethodCache(class ILuaClassInterface) *MethodCache {
	mc := &MethodCache{
		Methods: make(map[string]reflect.Method),
	}

	// 懒加载初始化
	mc.Once.Do(func() {
		// 获取所有导出方法
		classType := reflect.TypeOf(class).Elem()
		for i := 0; i < classType.NumMethod(); i++ {
			method := classType.Method(i)
			if method.PkgPath == "" { // 只导出方法
				mc.Methods[method.Name] = method
			}
		}

		// 预计算方法名列表
		mc.MethodNames = make([]string, 0, len(mc.Methods))
		for name := range mc.Methods {
			mc.MethodNames = append(mc.MethodNames, name)
		}

		mc.initialized = true
	})

	return mc
}

// GetMethod 从缓存获取方法
func (mc *MethodCache) GetMethod(name string) (reflect.Method, bool) {
	if mc == nil {
		return reflect.Method{}, false
	}

	// 确保缓存已初始化
	mc.Once.Do(func() {
		// 空实现，确保初始化
	})

	if !mc.initialized || mc.Methods == nil {
		return reflect.Method{}, false
	}

	method, ok := mc.Methods[name]
	return method, ok
}

// GetMethodNames 获取所有方法名
func (mc *MethodCache) GetMethodNames() []string {
	if mc == nil {
		return nil
	}

	// 确保缓存已初始化
	mc.Once.Do(func() {
		// 空实现，确保初始化
	})

	if !mc.initialized {
		return nil
	}

	return mc.MethodNames
}

// ParameterCache 缓存方法的参数类型
type ParameterCache struct {
	Types []reflect.Type
}

// GetParameterCache 获取方法的参数类型缓存
func GetParameterCache(method reflect.Method) *ParameterCache {
	paramTypes := make([]reflect.Type, method.Type.NumIn())
	for i := 0; i < method.Type.NumIn(); i++ {
		paramTypes[i] = method.Type.In(i)
	}

	return &ParameterCache{
		Types: paramTypes,
	}
}

// GetType 获取指定位置的参数类型
func (pc *ParameterCache) GetType(idx int) reflect.Type {
	if pc == nil || idx < 0 || idx >= len(pc.Types) {
		return nil
	}
	return pc.Types[idx]
}
