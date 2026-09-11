package localtimer

import "testing"

func BenchmarkTimerType_String(b *testing.B) {
	types := []TimerType{
		TimerTypeMillisecond, TimerTypeSecond, TimerTypeMinute,
		TimerTypeTenMinute, TimerTypeHour, TimerType(99),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range types {
			_ = t.String()
		}
	}
}

func BenchmarkCalculateType(b *testing.B) {
	tr := &Timer{}
	tr.Init(100, 1, nil)
	intervals := []int64{50, 2000, 120000, 1200000, 7200000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, ms := range intervals {
			_ = tr.calculateType(ms)
		}
	}
}

func BenchmarkTimer_HasNext(b *testing.B) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.HasNext()
	}
}
