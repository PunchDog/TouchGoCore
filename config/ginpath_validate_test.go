package config

import "testing"

// TestValidateGinPath 覆盖反向代理 ginpath 项的格式轻校验：合法放行、非法逐类报错。
func TestValidateGinPath(t *testing.T) {
	valid := []string{
		"/proxy/hello|GET",
		"/proxy/user/:id|GET",
		"/onlypath",  // 省略方法 = 通配
		"/trailing|", // 显式空方法 = 通配
		"/a/b/c|POST",
		"/x/:y/:z|DELETE", // 多参数段
		"/a/:b|GET",       // 参数段冒号后有字符
	}
	for _, s := range valid {
		if err := validateGinPath(s); err != nil {
			t.Fatalf("✘ 合法项 %q 被拒: %v", s, err)
		}
	}

	invalid := []string{
		"",                // 空
		"proxy/hello|GET", // 缺前导斜杠
		"/hello|get",      // 方法小写
		"/hello|Patch",    // 方法非全大写
		"/hello|FOO",      // 非法方法名
		"/a/:|GET",        // ":name" 段冒号后为空
	}
	for _, s := range invalid {
		if err := validateGinPath(s); err == nil {
			t.Fatalf("✘ 非法项 %q 未报错", s)
		}
	}
}

// TestValidateGinPathThroughCfg 走完整 Validate：client.ginpath 非法应阻断启动，合法放行。
func TestValidateGinPathThroughCfg(t *testing.T) {
	base := func(ginpath []string) *Cfg {
		return &Cfg{
			Web: &WebConfig{HTTPPort: 1000},
			Rpc: &RpcConfig{
				Client: []*RpcAddr{{Name: "c", Addr: "127.0.0.1", Port: 7100, GinPath: ginpath}},
			},
		}
	}
	if err := base([]string{"/ok|GET", "/u/:id"}).Validate(); err != nil {
		t.Fatalf("✘ 合法 ginpath 被拒: %v", err)
	}
	if err := base([]string{"bad|GET"}).Validate(); err == nil {
		t.Fatal("✘ 非法 ginpath（缺前导斜杠）应报错")
	}
}
