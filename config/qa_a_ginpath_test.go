package config

import (
	"strings"
	"testing"
)

// ============================================================================
// A-F7 回归：ginpath 校验补口——
//   - 同一客户端内 path|method 重复报错；跨客户端重复同样报错（全局汇总）；
//   - "/x"（省略方法）与 "/x|"（显式空方法）等价，也算重复；
//   - /static、/ws 保留前缀（含段边界）报错：根劫持在查表前处理这两类前缀，
//     此类配置是死路由；/staticfoo、/wsx 不受影响。
// ============================================================================

// qaACfg 组装只含 rpc.client 段的最小配置。
func qaACfg(clients ...[]string) *Cfg {
	rpc := &RpcConfig{}
	for i, gps := range clients {
		rpc.Client = append(rpc.Client, &RpcAddr{
			Name:    "qa-client-" + string(rune('a'+i)),
			Addr:    "127.0.0.1",
			Port:    7100 + i,
			GinPath: gps,
		})
	}
	return &Cfg{Web: &WebConfig{HTTPPort: 1000}, Rpc: rpc}
}

func TestQaAGinPathDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		clients [][]string
		wantErr string // 期望报错中包含的片段；空串表示应通过
	}{
		{"同一客户端完全重复", [][]string{{"/a|GET", "/a|GET"}}, "重复"},
		{"通配写法等价重复", [][]string{{"/a", "/a|"}}, "重复"},
		{"省略方法 vs 带方法不重复", [][]string{{"/a", "/a|GET"}}, ""},
		{"同路径不同方法不重复", [][]string{{"/a|GET", "/a|POST"}}, ""},
		{"跨客户端重复", [][]string{{"/b|POST"}, {"/b|POST"}}, "重复"},
		{"跨客户端通配重复", [][]string{{"/c"}, {"/c|"}}, "重复"},
		{"无重复放行", [][]string{{"/d|GET", "/e"}, {"/f|POST"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := qaACfg(tc.clients...).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("✘ 合法配置被拒: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("✘ 重复 ginpath 未报错")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("✘ 报错文案应含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}
}

func TestQaAGinPathReservedPrefix(t *testing.T) {
	// 单项校验层面：保留前缀按段边界拦截
	reserved := []string{"/static", "/static/", "/static/a/b", "/ws", "/ws/", "/ws/chat", "/static|GET", "/ws/x|POST"}
	for _, s := range reserved {
		if err := validateGinPath(s); err == nil {
			t.Fatalf("✘ 保留前缀项 %q 未报错", s)
		}
	}
	allowed := []string{"/staticfoo", "/staticfoo|GET", "/wsx", "/wsx|POST", "/w", "/s|GET", "/wst/api/:id|GET"}
	for _, s := range allowed {
		if err := validateGinPath(s); err != nil {
			t.Fatalf("✘ 合法项 %q 被误拦: %v", s, err)
		}
	}

	// 整链 Validate 同样拦截
	if err := qaACfg([]string{"/proxy/ok|GET", "/static/img|GET"}).Validate(); err == nil {
		t.Fatal("✘ Validate 未拦截 /static 前缀 ginpath")
	}
	if err := qaACfg([]string{"/ws/live"}).Validate(); err == nil {
		t.Fatal("✘ Validate 未拦截 /ws 前缀 ginpath")
	}
	if err := qaACfg([]string{"/proxy/ok|GET", "/staticfoo|GET", "/wsx|POST"}).Validate(); err != nil {
		t.Fatalf("✘ 段边界外的相似前缀被误拦: %v", err)
	}
}

// TestQaAGinPathKeyNormalize 去重键归一化口径。
func TestQaAGinPathKeyNormalize(t *testing.T) {
	for _, tc := range []struct{ item, want string }{
		{"/a", "/a|"},
		{"/a|", "/a|"},
		{"/a|GET", "/a|GET"},
		{"/u/:id|POST", "/u/:id|POST"},
	} {
		if got := ginPathKey(tc.item); got != tc.want {
			t.Fatalf("✘ ginPathKey(%q)=%q，期望 %q", tc.item, got, tc.want)
		}
	}
}
