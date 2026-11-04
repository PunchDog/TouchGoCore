package lua

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	rt "github.com/arnodel/golua/runtime"
)

// ==================== 注册表回放（缺陷 a）====================
//
// Init() 是 poison 恢复与热重载失败回退的唯一重建入口。它原先只装 5 个内置函数，
// 不回放 registeredFuncs / registeredClasses，于是这两条路径重建出的 runtime
// 只有内置函数——Lua 侧 Npc() 与宿主函数全部消失，且没有任何报错。

// qaEnvHas 报告实例的全局环境里是否存在某个非 nil 名字。
func qaEnvHas(ls *LuaScript, name string) bool {
	ls.rtMu.RLock()
	defer ls.rtMu.RUnlock()
	if ls.env == nil {
		return false
	}
	return ls.env.Get(rt.StringValue(name)) != rt.NilValue
}

// TestQaGL_InitReplaysRegisteredClass 验证 Init() 之后类构造器仍在 env 里。
func TestQaGL_InitReplaysRegisteredClass(t *testing.T) {
	registerProbeClass(t)
	ls := runProbeScript(t, "local a = probeObject()\n")
	t.Cleanup(ls.Close)

	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("前置条件不成立：创建实例后 env 里应有 probeObject")
	}

	if err := ls.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("✘ Init() 没有回放类注册表，poison 恢复/重载回退后 Lua 侧构造器会消失")
	}
}

// TestQaGL_InitReplaysRegisteredFunc 验证 Init() 之后宿主函数仍在 env 里，
// 且仍按「1 个具名槽 + etc」绑定（第 2 个之后的实参不能丢）。
func TestQaGL_InitReplaysRegisteredFunc(t *testing.T) {
	setupLuaTestInstances(t)

	const fname = "qa_probe_replay_fn"
	var got []string
	if err := RegisterLuaFunc(fname, func(_ *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		got = got[:0]
		for _, v := range append([]rt.Value{c.Arg(0)}, c.Etc()...) {
			s, ok := v.TryString()
			if !ok {
				return nil, fmt.Errorf("参数应为字符串")
			}
			got = append(got, s)
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

	path := filepath.Join(t.TempDir(), "replay_func.lua")
	if err := os.WriteFile(path, []byte(fname+`("甲", "乙")`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}
	t.Cleanup(ls.Close)

	if err := ls.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !qaEnvHas(ls, fname) {
		t.Fatalf("✘ Init() 没有回放宿主函数 %s", fname)
	}

	// 回放后的绑定形状必须仍能把第 2 个之后的实参送到 etc
	if _, err := ls.CallWithContext(context.Background(), fname, "甲", "乙", "丙"); err != nil {
		t.Fatalf("✘ Init() 后调用宿主函数失败: %v", err)
	}
	if len(got) != 3 || got[0] != "甲" || got[2] != "丙" {
		t.Fatalf("✘ Init() 后宿主函数的 extra 实参丢失: %v", got)
	}
}

// TestQaGL_ReloadRollbackKeepsConstructor 走真实生产形状：脚本改坏 → 自动重载
// 失败 → 回退路径调 oldScript.Init()。回退后旧实例必须还能用 Lua 侧构造器，
// 否则等于「重载失败把业务类也弄丢了」。
func TestQaGL_ReloadRollbackKeepsConstructor(t *testing.T) {
	registerProbeClass(t)
	setupLuaTestInstances(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.lua")
	good := "probe = probeObject()\nprobe:SetName('甲')\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}

	sw := NewScriptWatcherWithContext(context.Background(), ls)

	// 写坏脚本，让 NewLuaScriptWithContext 编译失败，从而走回退分支
	if err := os.WriteFile(path, []byte("this is not lua ~~\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sw.reloadScript(context.Background())

	if got := sw.GetScript(); got != ls {
		t.Fatalf("✘ 回退后监视器应仍指向旧实例，实际指向另一实例")
	}
	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("✘ 重载失败回退后 Lua 侧构造器消失（Init 未回放注册表）")
	}
}
