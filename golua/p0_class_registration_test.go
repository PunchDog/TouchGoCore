package lua

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"touchgocore/localtimer"
)

// probeObject 用于验证「Go 类注册到 Lua」整条链路是否真的可用：
// 方法全部以下列指针接收者声明，正是注册表最容易漏掉的一类方法。
type probeObject struct {
	ILuaClassObject

	uid     int64
	name    string
	items   []int64
	deleted bool
}

func (p *probeObject) Init(id int64, ls *LuaScript) { p.uid = id }
func (p *probeObject) SetName(name string)          { p.name = name }
func (p *probeObject) GetName() string              { return p.name }
func (p *probeObject) Uid() int64                   { return p.uid }
func (p *probeObject) AddItem(v int64)              { p.items = append(p.items, v) }
func (p *probeObject) ItemCount() int               { return len(p.items) }

func (p *probeObject) Tag(id int, text string, extra ...any) string {
	return fmt.Sprintf("%d|%s|%d", id, text, len(extra))
}

// registerProbeClass 注册探针类并在用例结束后摘掉，避免污染全局注册表。
func registerProbeClass(t *testing.T) {
	t.Helper()
	probed := &probeObject{}
	registeredClassesMu.Lock()
	if registeredClasses == nil {
		registeredClasses = make(map[string]ILuaClassInterface)
	}
	registeredClasses["probeObject"] = probed
	registeredClassesMu.Unlock()

	t.Cleanup(func() {
		registeredClassesMu.Lock()
		delete(registeredClasses, "probeObject")
		registeredClassesMu.Unlock()
	})
}

// newProbeScript 用给定 Lua 源码建一个脚本实例，把主块报错原样交回调用方。
func newProbeScript(t *testing.T, src string) (*LuaScript, error) {
	t.Helper()
	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })

	// 不预建 luaInstances：NewLuaScriptWithContext 自己负责登记，
	// 这里先建好等于把「nil map 赋值」的回归点遮掉
	t.Cleanup(StopLua)

	path := filepath.Join(t.TempDir(), "probe.lua")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewLuaScriptWithContext(context.Background(), path)
}

// runProbeScript 主块必须执行成功，否则直接判失败。
func runProbeScript(t *testing.T, src string) *LuaScript {
	t.Helper()
	ls, err := newProbeScript(t, src)
	if err != nil {
		t.Fatalf("✘ 脚本执行失败: %v", err)
	}
	return ls
}

func TestRegisterLuaClass_PointerReceiver(t *testing.T) {
	if err := RegisterLuaClass(&probeObject{}); err != nil {
		t.Fatalf("✘ 注册指针类型类应成功: %v", err)
	}
	t.Cleanup(func() {
		registeredClassesMu.Lock()
		for name, class := range registeredClasses {
			if _, ok := class.(*probeObject); ok {
				delete(registeredClasses, name)
			}
		}
		registeredClassesMu.Unlock()
	})
}

// TestLuaClassMethodRoundTrip 验证 Lua 侧能真正调到指针接收者方法，
// 且同一脚本内的两个对象互不串状态。
func TestLuaClassMethodRoundTrip(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
local b = probeObject()
a:SetName("alpha")
b:SetName("beta")
a:AddItem(10)
a:AddItem(20)
if a:GetName() ~= "alpha" then error("A 的名称被 B 覆盖: " .. tostring(a:GetName())) end
if b:GetName() ~= "beta" then error("B 的名称丢失: " .. tostring(b:GetName())) end
if a:ItemCount() ~= 2 then error("A 的条目数不对: " .. tostring(a:ItemCount())) end
if b:ItemCount() ~= 0 then error("B 串到了 A 的数据: " .. tostring(b:ItemCount())) end
`)
}

// TestLuaClassObjectIdentity 验证每个 userdata 拿到独立对象号：
// 修复前构造器用 script.UID 做键，同脚本内所有对象共用一格。
func TestLuaClassObjectIdentity(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
local b = probeObject()
if a:Uid() == b:Uid() then error("两个对象拿到同一个对象号: " .. tostring(a:Uid())) end
if a:Uid() == 0 then error("对象号未分配，脚本 UID 在主块执行完才赋值") end
`)
}

// TestLuaClassVariadic 验证可变参数方法按 Go 签名收全。
func TestLuaClassVariadic(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
if a:Tag(7, "txt", "x", "y") ~= "7|txt|2" then error("变参收集错: " .. a:Tag(7, "txt", "x", "y")) end
if a:Tag(1, "z") ~= "1|z|0" then error("零变参收集错: " .. a:Tag(1, "z")) end
`)
}

// TestLuaClassLifecycleMethodsHidden Init/Delete/Update 是框架生命周期方法，
// 交给 Lua 调用会让脚本自行改写对象登记键。
func TestLuaClassLifecycleMethodsHidden(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
if a.Init ~= nil or a.Delete ~= nil or a.Update ~= nil then error("生命周期方法泄漏给了 Lua") end
if a.SetName == nil then error("普通方法应可见") end
`)
}

// TestLuaClassMissingArgsError 参数不足应回 Lua 错误，而不是反射层 panic。
func TestLuaClassMissingArgsError(t *testing.T) {
	registerProbeClass(t)
	if _, err := newProbeScript(t, "local a = probeObject()\nreturn a:SetName()\n"); err == nil {
		t.Fatal("✘ 缺少必填参数应报错，不能让 reflect.Call 直接崩")
	}
}
