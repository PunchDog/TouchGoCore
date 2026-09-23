package ustd

import (
	"context"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
)

// TestStartNilCfgDoesNotPanic 没有任何 ustd 配置时启动必须安静通过，
// 且后续操作被拒而不是拿着空链路发请求。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	UstdStop(nil)
	UstdStart(corectx.WithCfg(context.Background(), nil))
	UstdStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	UstdStart(nil)
	UstdStart(corectx.WithCfg(context.Background(), &config.Cfg{Ustd: &config.UstdConfig{}}))
	if _, err := UstdQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	UstdStop(nil)
	UstdStop(nil) // 幂等
}

// TestTimeoutAndRetryDefaults 未配超时与重试时落到共用层默认值，而不是 0 超时。
func TestTimeoutAndRetryDefaults(t *testing.T) {
	cfg := payCfg("https://pay.invalid", map[string]string{pay.EndpointQuery: "/api/query"})
	cfg.PaySDks[testSection].TimeoutSec = 0
	cfg.PaySDks[testSection].MaxRetries = -1
	startWith(t, cfg)
	// 域名不可解析：一次请求 + 默认重试次数后返回错误即可，不 panic 也不无限等待。
	if _, err := UstdQuery(nil, "O1"); err == nil {
		t.Fatal("不可达供应商应当报错")
	}
}
