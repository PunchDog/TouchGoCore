package touchgocore

// 阶段12（S66）回归用例与基准：时间轮实时积压是否真的出现在 /metrics 上。
//
// 变更前 localtimer 只导出累计计数，监控看不到「此刻调度通道/入链通道堆了多少」，
// 而深度顶到上限的下一步就是静默丢弃；变更后由拉取式采集器在抓取瞬间读快照。

import (
	"context"
	"strings"
	"testing"
	"time"

	"touchgocore/localtimer"
	"touchgocore/metrics"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type backlogNoopTimer struct {
	localtimer.Timer
}

func (b *backlogNoopTimer) Tick() {}

// gatherTimerFamilies 抓取一次并挑出时间轮相关的指标族。
func gatherTimerFamilies(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]*dto.MetricFamily)
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "touchgocore_timer_") {
			out[mf.GetName()] = mf
		}
	}
	return out
}

// seriesValue 取某个序列的值。labelKey 为空表示匹配「没有标签的那一条」。
func seriesValue(mf *dto.MetricFamily, labelKey, labelVal string) (float64, bool) {
	if mf == nil {
		return 0, false
	}
	for _, m := range mf.GetMetric() {
		if labelKey == "" {
			if len(m.GetLabel()) != 0 {
				continue
			}
		} else {
			matched := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelVal {
					matched = true
				}
			}
			if !matched {
				continue
			}
		}
		switch {
		case m.GetGauge() != nil:
			return m.GetGauge().GetValue(), true
		case m.GetCounter() != nil:
			return m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

func TestTimerBacklogMetricsExposed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localtimer.Run(ctx)
	defer localtimer.TimeStop(context.Background())

	tm, err := localtimer.NewTimer[*backlogNoopTimer](20, localtimer.InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := localtimer.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	InitMetrics()
	fams := gatherTimerFamilies(t)

	depth := fams["touchgocore_timer_queue_depth"]
	if depth == nil {
		t.Fatalf("✘ 缺少调度通道积压指标，现有时间轮指标：%v", keysOf(fams))
	}
	if _, ok := seriesValue(depth, "queue", "schedule"); !ok {
		t.Fatal("✘ queue_depth 未含 schedule 序列")
	}
	for _, wheel := range []string{"millisecond", "second", "minute", "ten-minute", "hour"} {
		if _, ok := seriesValue(depth, "queue", "wheel:"+wheel); !ok {
			t.Fatalf("✘ queue_depth 缺少入链通道序列 wheel:%s", wheel)
		}
	}
	if fams["touchgocore_timer_queue_capacity"] == nil {
		t.Fatal("✘ 缺少通道容量指标")
	}

	inWheel, ok := seriesValue(fams["touchgocore_timer_in_wheel"], "wheel", "all")
	if !ok {
		t.Fatal("✘ 缺少在链定时器总数指标")
	}
	if inWheel < 1 {
		t.Fatalf("✘ 已注册的定时器未体现在 in_wheel 指标上: %v", inWheel)
	}

	for _, name := range []string{"touchgocore_timer_dropped_total",
		"touchgocore_timer_reschedule_failed_total", "touchgocore_timer_pool_in_use"} {
		if v, ok := seriesValue(fams[name], "", ""); ok && v < 0 {
			t.Fatalf("✘ %s 出现负值: %v", name, v)
		} else if !ok {
			t.Fatalf("✘ 缺少指标 %s", name)
		}
	}

	tm.Remove()
	t.Log("✔ 时间轮积压、容量、丢弃/续期失败与池空闲量均可从 /metrics 读到")
}

// TestTimerBacklogCollectorRegistersOnce 回归（S66）：InitMetrics 会被宿主启动流程
// 与 StartMetricsServer 重复调用，采集器注册必须幂等，否则重复抓取会看到翻倍序列。
func TestTimerBacklogCollectorRegistersOnce(t *testing.T) {
	InitMetrics()
	mf := gatherTimerFamilies(t)["touchgocore_timer_queue_depth"]
	if mf == nil {
		t.Fatal("✘ 采集器未注册")
	}
	before := len(mf.GetMetric())

	InitMetrics()
	InitMetrics()
	after := len(gatherTimerFamilies(t)["touchgocore_timer_queue_depth"].GetMetric())
	if before != after {
		t.Fatalf("✘ 重复 InitMetrics 导致序列翻倍: %d -> %d", before, after)
	}
}

func keysOf(m map[string]*dto.MetricFamily) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ==================== 基准：每次抓取新增的代价 ====================

func BenchmarkTimerBacklogCollect(b *testing.B) {
	InitMetrics()
	c := timerBacklogCollector{}
	ch := make(chan prometheus.Metric, 64)
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Collect(ch)
	}
	b.StopTimer()
	close(ch)
	<-drained
}

func BenchmarkMetricsGatherWithTimerCollector(b *testing.B) {
	InitMetrics()
	benchmarkGather(b)
}

// BenchmarkMetricsGatherWithoutTimerCollector 同一 registry 摘掉时间轮采集器后的
// 抓取开销，两者之差即本项给每次抓取加的钱。
func BenchmarkMetricsGatherWithoutTimerCollector(b *testing.B) {
	InitMetrics()
	if !metrics.Registry().Unregister(timerBacklogCollector{}) {
		b.Fatal("时间轮采集器未注册，无法做对比")
	}
	defer func() {
		if err := metrics.Registry().Register(timerBacklogCollector{}); err != nil {
			b.Error(err)
		}
	}()
	benchmarkGather(b)
}

func benchmarkGather(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := metrics.Registry().Gather(); err != nil {
			b.Fatal(err)
		}
	}
}
