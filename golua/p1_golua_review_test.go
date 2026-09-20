package lua

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/util"

	rt "github.com/arnodel/golua/runtime"
)

// Delete 记录 Go 侧回收是否真的发生：Close 与热重载对它的期望相反
// （前者要 Delete，后者不能 Delete）。
func (p *probeObject) Delete() { p.deleted = true }

// probeObjectsIn 取脚本实例当前登记的全部对象，按对象号索引。
func probeObjectsIn(ls *LuaScript) map[int64]*probeObject {
	out := map[int64]*probeObject{}
	ls.RangeRegisteredObjects(func(uid int64, obj any) bool {
		if p, ok := obj.(*probeObject); ok {
			out[uid] = p
		}
		return true
	})
	return out
}

// TestLuaScriptCloseReleasesObjects 脚本实例是 Lua 侧对象的持有者：Close 必须逐个
// Delete 并清空登记表。修复前 __gc 挂在 rt.NewUserData 上永远不触发（golua 只有
// Runtime.NewUserDataValue 才注册终结器），对象只增不减。
func TestLuaScriptCloseReleasesObjects(t *testing.T) {
	registerProbeClass(t)
	ls := runProbeScript(t, "local a = probeObject()\nlocal b = probeObject()\n")

	before := probeObjectsIn(ls)
	if len(before) != 2 {
		t.Fatalf("✘ 前置条件：登记表应有 2 个对象，实际 %d", len(before))
	}
	ls.Close()

	if after := probeObjectsIn(ls); len(after) != 0 {
		t.Fatalf("✘ Close 后登记表仍留着 %d 个对象", len(after))
	}
	for uid, p := range before {
		if !p.deleted {
			t.Errorf("✘ 对象号 %d 没有被 Delete", uid)
		}
	}
}

// TestReloadKeepsObjectsAliveAndCountersMonotonic 热重载会重建运行时：旧对象既不能
// 被 Delete，也不能被重载后新建的对象顶掉登记项；主块重跑时方法还得照常调得到。
func TestReloadKeepsObjectsAliveAndCountersMonotonic(t *testing.T) {
	registerProbeClass(t)
	// 主块带一次 setter 调用：重载后类没重新绑进 env 的话这里就直接报错
	ls := runProbeScript(t, "probe = probeObject()\nprobe:SetName('甲')\n")

	before := probeObjectsIn(ls)
	if len(before) != 1 {
		t.Fatalf("✘ 前置条件：应有 1 个对象，实际 %d", len(before))
	}
	var oldUID int64
	var old *probeObject
	for uid, p := range before {
		oldUID, old = uid, p
	}

	if err := ls.ReloadScriptWithContext(context.Background()); err != nil {
		t.Fatalf("✘ 重载失败: %v", err)
	}
	t.Cleanup(ls.Close)

	after := probeObjectsIn(ls)
	if len(after) != 2 {
		t.Fatalf("✘ 重载后应有旧+新 2 个对象，实际 %d", len(after))
	}
	if after[oldUID] != old {
		t.Fatal("✘ 旧对象的登记项被顶掉了")
	}
	if old.deleted {
		t.Fatal("✘ 热重载把还要用的旧对象 Delete 了")
	}
	for uid, p := range after {
		if uid == oldUID {
			continue
		}
		if p.GetName() != "甲" {
			t.Fatalf("✘ 重载后主块里的 SetName 没落到新对象上: %q", p.GetName())
		}
	}
}

// TestRestoreRegisteredObjectsKeepsNewObjects 重建实例的重载路径先跑完新脚本再回挂
// 旧对象：同序号上直接 Store 等于让旧对象盖住新对象，新 userdata 的调用会落到旧对象上。
func TestRestoreRegisteredObjectsKeepsNewObjects(t *testing.T) {
	dst := &LuaScript{registeredObjects: syncmap.NewAny()}
	newer := &probeObject{uid: 1}
	dst.registeredObjects.Store(int64(1), newer)

	src := syncmap.NewAny()
	src.Store(int64(1), &probeObject{uid: 1})
	src.Store(int64(7), &probeObject{uid: 7})

	restoreRegisteredObjects(dst, src)

	if got, _ := dst.registeredObjects.Load(int64(1)); got != newer {
		t.Fatal("✘ 旧对象把新脚本刚建的对象顶掉了")
	}
	if _, ok := dst.registeredObjects.Load(int64(7)); !ok {
		t.Fatal("✘ 无人占用的旧对象应照常回挂")
	}
	// 水位必须盖过回挂进来的最大号，否则后续对象会撞号
	if got := dst.nextObjectID.Add(1); got <= 7 {
		t.Fatalf("✘ 对象号水位未推进: %d", got)
	}
}

// TestRegisterLuaFuncExtraArgsReachable 宿主函数按「1 个具名槽 + etc」绑定：
// 绑定成 hasEtc=false 时 golua 会把第 2 个之后的实参直接丢掉。
func TestRegisterLuaFuncExtraArgsReachable(t *testing.T) {
	var first string
	var etc []string
	const fname = "probe_record_args"
	if err := RegisterLuaFunc(fname, func(_ *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		s, err := c.StringArg(0)
		if err != nil {
			return nil, err
		}
		first = s
		for _, v := range c.Etc() {
			str, ok := v.TryString()
			if !ok {
				return nil, fmt.Errorf("参数应为字符串")
			}
			etc = append(etc, str)
		}
		return c.Next(), nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registeredFuncsMu.Lock()
		delete(registeredFuncs, fname)
		registeredFuncsMu.Unlock()
	})

	runProbeScript(t, fname+`("甲", "乙", "丙")`)

	if first != "甲" {
		t.Errorf("✘ 首个实参没取到: %q", first)
	}
	if strings.Join(etc, ",") != "乙,丙" {
		t.Fatalf("✘ 第 2 个之后的实参丢失: %v", etc)
	}
}

// TestRegisterLuaClassSameTypeIsIdempotent 同一类型重复注册走幂等：注册表既不追加
// 记录，也不换掉先登记的那份引用。
func TestRegisterLuaClassSameTypeIsIdempotent(t *testing.T) {
	baseline := classCount(t)

	first := &probeObject{}
	name, _ := util.GetClassName(first)
	t.Cleanup(func() { removeClassByName(t, name) })

	if err := RegisterLuaClass(first); err != nil {
		t.Fatalf("✘ 首次注册应成功: %v", err)
	}
	if err := RegisterLuaClass(&probeObject{}); err != nil {
		t.Fatalf("✘ 同一类型重复注册应幂等: %v", err)
	}

	registeredClassesMu.RLock()
	held, total := registeredClasses[name], len(registeredClasses)
	registeredClassesMu.RUnlock()
	if held != first {
		t.Fatal("✘ 重复注册换掉了注册表里的实例，先前那份引用被丢弃")
	}
	if total != baseline+1 {
		t.Fatalf("✘ 注册表多出记录: 基线 %d，现在 %d", baseline, total)
	}
}

// TestRegisterLuaClassShortNameConflict 类名只取短名，同名不同类型的两次注册必须
// 报错并点名双方，否则后注册的类静默用不上。
func TestRegisterLuaClassShortNameConflict(t *testing.T) {
	registeredClassesMu.Lock()
	if registeredClasses == nil {
		registeredClasses = make(map[string]ILuaClassInterface)
	}
	registeredClasses["probeObject"] = &otherProbe{}
	registeredClassesMu.Unlock()
	t.Cleanup(func() { removeClassByName(t, "probeObject") })

	err := RegisterLuaClass(&probeObject{})
	if err == nil {
		t.Fatal("✘ 短名冲突应报错")
	}
	if !strings.Contains(err.Error(), "otherProbe") || !strings.Contains(err.Error(), "probeObject") {
		t.Fatalf("✘ 冲突信息未点名双方: %v", err)
	}
}

// TestRegisterLuaClassRejectsBadValue 带类型的空指针是合法的非空接口，会在
// util.GetClassName 的 reflect.Indirect 上 panic；值类型的方法集不含指针接收者
// 方法，注册进去就是个空方法表。两种都要在入口拒绝。
func TestRegisterLuaClassRejectsBadValue(t *testing.T) {
	var nilProbe *probeObject
	if err := RegisterLuaClass(nilProbe); err == nil {
		t.Fatal("✘ 带类型的空指针应被拒绝")
	}
	if err := RegisterLuaClass(probeValue{}); err == nil {
		t.Fatal("✘ 值类型（非指针）应被拒绝")
	}
}

// TestLuaPathHelpersReturnUniformResults dofile / getpathluafile 是 mapmanager/init.lua
// 约定写法的前提：成功与失败都返回 (值, 说明)，脚本根目录外的路径必须被拒绝。
func TestLuaPathHelpersReturnUniformResults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.lua"), []byte("loaded_flag = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })

	prevParent := luaParentCtx
	luaParentCtx = corectx.WithCfg(context.Background(), &config.Cfg{
		LuaConfig: &config.LuaConfig{ScriptPath: filepath.Join(dir, "main.lua")},
	})
	t.Cleanup(func() { luaParentCtx = prevParent })

	root := filepath.ToSlash(dir)
	src := `
local ok, msg = dofile("` + root + `/good.lua")
if ok ~= true or msg ~= "ok" then
    error("dofile 成功分支应返回 (true, 'ok')，实际 " .. tostring(ok) .. "/" .. tostring(msg))
end

local ok2, msg2 = dofile("` + root + `/missing.lua")
if ok2 ~= false or msg2 == nil or msg2 == "" then
    error("dofile 失败分支应返回 (false, 原因)，实际 " .. tostring(ok2) .. "/" .. tostring(msg2))
end

local ok3, msg3 = dofile("` + root + `/../outside.lua")
if ok3 ~= false or msg3 == nil or msg3 == "" then
    error("越界路径应被拒绝，实际 " .. tostring(ok3) .. "/" .. tostring(msg3))
end

local files, fmsg = getpathluafile("` + root + `")
if type(files) ~= "table" then error("getpathluafile 成功应返回表，实际 " .. type(files)) end
if fmsg ~= "ok" then error("getpathluafile 成功应带第二返回值，实际 " .. tostring(fmsg)) end

local found = false
for _, f in pairs(files) do
    if type(f) ~= "string" then error("文件列表元素应为路径字符串") end
    if string.find(f, "good%.lua") then found = true end
end
if not found then error("文件列表里没有 good.lua") end

local bad, bmsg = getpathluafile("` + root + `/../outside")
if bad ~= false or bmsg == nil or bmsg == "" then
    error("getpathluafile 越界应返回 (false, 原因)，实际 " .. tostring(bad) .. "/" .. tostring(bmsg))
end
`
	path := filepath.Join(dir, "driver.lua")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("✘ 脚本执行失败: %v", err)
	}
	t.Cleanup(StopLua)
	_ = ls
}

// TestLuaErrorIsRealBuiltin error 必须是标准 Lua 的抛错入口：早前它被同名 Go 日志
// 函数覆盖，脚本里的 error("配置无效") 只落一行日志然后继续往下跑，npc.lua 和
// init.lua 的失败分支全部失效。
func TestLuaErrorIsRealBuiltin(t *testing.T) {
	// 先确认 error 是函数：不是函数就调用 nil 强制抛错，免得断言本身被吞掉
	runProbeScript(t, `if type(error) ~= "function" then local missing = nil missing() end`)
	// 再确认 logerror 接管了原来的日志输出
	runProbeScript(t, `if type(logerror) ~= "function" then local missing = nil missing() end`)

	if _, err := newProbeScript(t, "error('故意抛错')\nlocal x = 1\n"); err == nil {
		t.Fatal("✘ error() 没有中断脚本")
	}
}

// TestLuaDebugLibNotShadowed debug 是标准库表，被日志函数占用后 debug.getinfo、
// debug.traceback 这类惯用法全部报 "attempt to index a function value"。
func TestLuaDebugLibNotShadowed(t *testing.T) {
	runProbeScript(t, "local f = debug.getinfo\n")
	runProbeScript(t, `if type(logdebug) ~= "function" then local missing = nil missing() end`)
}

func classCount(t *testing.T) int {
	t.Helper()
	registeredClassesMu.RLock()
	defer registeredClassesMu.RUnlock()
	return len(registeredClasses)
}

func removeClassByName(t *testing.T, name string) {
	t.Helper()
	registeredClassesMu.Lock()
	delete(registeredClasses, name)
	registeredClassesMu.Unlock()
}

// otherProbe 只为造出「短名相同、类型不同」的冲突场景而存在。
type otherProbe struct {
	ILuaClassObject
}

func (o *otherProbe) Init(int64, *LuaScript) {}

// probeValue 值类型：注册入口必须拒绝，所以三个接口方法都得是值接收者。
type probeValue struct {
	ILuaClassObject
}

func (probeValue) Init(int64, *LuaScript) {}
func (probeValue) Delete()                {}
func (probeValue) Update()                {}
