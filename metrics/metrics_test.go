package metrics

import "testing"

func TestInit_RegistryNonNil(t *testing.T) {
	Init()
	if Registry() == nil {
		t.Fatal("Registry 不应为 nil")
	}
}

func TestWSMetrics_BasicOps(t *testing.T) {
	WS.SetConnections(5)
	WS.IncConnection()
	WS.IncMessages("in")
	WS.IncErrors("decode")
	// 不 panic 即视为通过
}

func TestRPCMetrics_BasicOps(t *testing.T) {
	RPC.IncRequests("svc", "Method")
	RPC.ObserveLatency("svc", "Method", 100_000_000) // 100ms
	RPC.IncErrors("svc", "Method", "timeout")
}

func TestHTTPMetrics_BasicOps(t *testing.T) {
	HTTP.IncRequests("GET", "/api/v1/health", "200")
	HTTP.ObserveLatency("GET", "/api/v1/health", 5_000_000)
}

func TestTimerMetrics_BasicOps(t *testing.T) {
	Timer.SetActive(10)
	Timer.IncExecutions("millisecond")
}

func TestLuaMetrics_BasicOps(t *testing.T) {
	Lua.SetInstances(1)
	Lua.IncCalls("update")
	Lua.ObserveCallLatency("update", 1_000_000)
}

func TestDBMetrics_BasicOps(t *testing.T) {
	DB.SetConnections("mysql", "open", 3)
}

func TestLogMetrics_BasicOps(t *testing.T) {
	Log.IncMessages("info")
}
