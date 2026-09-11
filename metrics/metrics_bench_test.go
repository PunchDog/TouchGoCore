package metrics

import "testing"

func BenchmarkWSMetrics_IncConnection(b *testing.B) {
	for i := 0; i < b.N; i++ {
		WS.IncConnection()
	}
}

func BenchmarkRPCMetrics_IncRequests(b *testing.B) {
	for i := 0; i < b.N; i++ {
		RPC.IncRequests("svc", "Method")
	}
}

func BenchmarkTimerMetrics_IncExecutions(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Timer.IncExecutions("millisecond")
	}
}

func BenchmarkLuaMetrics_IncCalls(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Lua.IncCalls("update")
	}
}
