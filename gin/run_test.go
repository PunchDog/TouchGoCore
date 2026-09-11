package gin

import "testing"

func TestPackageCompiles(t *testing.T) {
	// gin 包中无独立状态可供断言；只验证测试可被编译
	// 实际行为在集成测试中验证（通过 app.go 启动完整服务）。
}
