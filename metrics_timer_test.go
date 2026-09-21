package touchgocore

// 阶段12（S66）回归用例与基准：时间轮实时积压是否真的出现在 /metrics 上。
//
// 变更前 localtimer 只导出累计计数，监控看不到「此刻调度通道/入链通道堆了多少」，
// 而深度顶到上限的下一步就是静默丢弃；变更后由拉取式采集器在抓取瞬间读快照。

import (
	"context"
	"strconv"
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

	// 间隔取 60 秒而不是几十毫秒：毫秒级定时器会在断言期间被派发、续期、离链，
	// 「在链数」随之波动，断言只能写成 >=1 这种恒真的宽松形式。分钟轮上的它整个
	// 用例期间都不会迁移，数值可精确核对。
	tm, err := localtimer.NewTimer[*backlogNoopTimer](60*1000, localtimer.InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := localtimer.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	// 等入链通道被轮协程消化（否则 queue_depth 与 in_wheel 都还是 0）
	time.Sleep(150 * time.Millisecond)

	poolBefore, ok := seriesValue(mustGather(t)["touchgocore_timer_pool_in_use"], "", "")
	if !ok {
		t.Fatal("✘ 缺少池外借指标")
	}

	InitMetrics()
	fams := mustGather(t)

	depth := fams["touchgocore_timer_queue_depth"]
	if depth == nil {
		t.Fatalf("✘ 缺少调度通道积压指标，现有时间轮指标：%v", keysOf(fams))
	}
	if _, ok := seriesValue(depth, "queue", "schedule"); !ok {
		t.Fatal("✘ queue_depth 未含 schedule 序列")
	}
	for _, wheel := range wheelSeriesNames {
		if _, ok := seriesValue(depth, "queue", "wheel:"+wheel); !ok {
			t.Fatalf("✘ queue_depth 缺少入链通道序列 wheel:%s", wheel)
		}
	}

	// 容量不能只断「族存在」：它是告警表达式里的分母，读成 0 会让所有
	// 「深度/容量」比率告警恒假，而这正是最容易写错又最难发现的一种缺失。
	capacity := fams["touchgocore_timer_queue_capacity"]
	shardDepth := fams["touchgocore_timer_schedule_shard_depth"]
	shardCapacity := fams["touchgocore_timer_schedule_shard_capacity"]
	if shardDepth == nil || shardCapacity == nil {
		t.Fatalf("✘ 缺少调度分片积压指标，现有时间轮指标：%v", keysOf(fams))
	}
	wantSchedule := float64(localtimer.MaxTimerChannelNum)
	if v, ok := seriesValue(capacity, "queue", "schedule"); !ok || v != wantSchedule {
		t.Fatalf("✘ schedule 容量异常: got=%v want=%v", v, wantSchedule)
	}
	for _, wheel := range wheelSeriesNames {
		want := float64(localtimer.MaxAddTimerChannelNum)
		if v, ok := seriesValue(capacity, "queue", "wheel:"+wheel); !ok || v != want {
			t.Fatalf("✘ wheel:%s 容量异常: got=%v ok=%v want=%v", wheel, v, ok, want)
		}
	}

	// 逐片序列（S68）另用一组指标名，不塞进 queue 标签：那样不按计划过滤的
	// sum(queue_depth) 会把总量与各片量重复计入。默认单片也必须出这片序列，
	// 否则开关一开整组序列凭空出现，看板上历史全断。
	shards := localtimer.ScheduleShards()
	sumShardCap := float64(0)
	for i := 0; i < shards; i++ {
		name := strconv.Itoa(i)
		if _, ok := seriesValue(shardDepth, "shard", name); !ok {
			t.Fatalf("✘ schedule_shard_depth 缺少分片序列 %s（共 %d 片）", name, shards)
		}
		v, ok := seriesValue(shardCapacity, "shard", name)
		if !ok || v <= 0 {
			t.Fatalf("✘ schedule_shard_capacity 缺少分片序列 %s: got=%v ok=%v", name, v, ok)
		}
		sumShardCap += v
	}
	// 各片容量 = 总容量整除片数，不整除时余数弃掉，所以允许比总容量小 (片数-1)。
	if sumShardCap > wantSchedule || wantSchedule-sumShardCap >= float64(shards) {
		t.Fatalf("✘ 各片容量与总容量不是一套: sum=%v schedule=%v shards=%d",
			sumShardCap, wantSchedule, shards)
	}

	inWheel := fams["touchgocore_timer_in_wheel"]
	total, ok := seriesValue(inWheel, "wheel", "all")
	if !ok {
		t.Fatal("✘ 缺少在链定时器总数指标")
	}
	if total != 1 {
		t.Fatalf("✘ 在链总数与用例注册数不符: got=%v want=1", total)
	}
	// 按档序列：总数相同的「毫秒轮爆满」与「均匀分布」必须长得不一样
	perWheel := 0.0
	for _, wheel := range wheelSeriesNames {
		v, ok := seriesValue(inWheel, "wheel", wheel)
		if !ok {
			t.Fatalf("✘ in_wheel 缺少按档序列 wheel=%s", wheel)
		}
		perWheel += v
	}
	if v, _ := seriesValue(inWheel, "wheel", "minute"); v != 1 {
		t.Fatalf("✘ 60 秒间隔的定时器没落在分钟档: got=%v", v)
	}
	if perWheel != total {
		t.Fatalf("✘ 各档在链数与总数对不上: sum=%v all=%v", perWheel, total)
	}

	for _, name := range []string{"touchgocore_timer_dropped_total",
		"touchgocore_timer_reschedule_failed_total"} {
		if v, ok := seriesValue(fams[name], "", ""); !ok {
			t.Fatalf("✘ 缺少指标 %s", name)
		} else if v < 0 {
			t.Fatalf("✘ %s 出现负值: %v", name, v)
		}
	}

	// 外借量必须能「涨」：只断非负的话，把口径写反成 Puts-Gets（负数）或恒 0 都能通过
	tm2, err := localtimer.NewTimer[*backlogNoopTimer](60*1000, localtimer.InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tm2.Remove()
	if after, ok := seriesValue(mustGather(t)["touchgocore_timer_pool_in_use"], "", ""); !ok || after < poolBefore+1 {
		t.Fatalf("✘ 新认领一个定时器后外借量没有增加: before=%v after=%v", poolBefore, after)
	}

	tm.Remove()
	t.Log("✔ 时间轮积压、容量、按档在链数、丢弃/续期失败与池外借量均可从 /metrics 读到")
}

// mustGather 抓取一次并只保留时间轮指标族。
func mustGather(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	InitMetrics()
	return gatherTimerFamilies(t)
}

// wheelSeriesNames 五档轮的标签文本，与 TimerType.String() 逐字一致。
var wheelSeriesNames = []string{"millisecond", "second", "minute", "ten-minute", "hour"}

// TestTimerBacklogSeriesSurviveWithoutManager 回归（阶段12 复核 F2）：默认管理器
// 不存在（尚未 Run、或 TimeStop 之后）时，按档序列必须仍以 0 出现。
//
// Prometheus 里「序列缺失」和「取值为 0」不是一回事：整档序列凭空消失时，
// 按 wheel 配的告警规则查不到数据而不是查到 0，等于在最需要观察的启动/收尾窗口
// 静默失明。
func TestTimerBacklogSeriesSurviveWithoutManager(t *testing.T) {
	localtimer.TimeStop(context.Background()) // 幂等；此时 defaultTimerManager 为空
	if mgr := localtimer.GetDefaultManager(); mgr != nil {
		t.Skip("默认管理器仍在，前序用例未收尾")
	}

	fams := mustGather(t)
	for _, name := range []string{"touchgocore_timer_queue_depth", "touchgocore_timer_queue_capacity",
		"touchgocore_timer_in_wheel"} {
		labelKey, prefix := "queue", "wheel:"
		if name == "touchgocore_timer_in_wheel" {
			labelKey, prefix = "wheel", ""
		}
		for _, wheel := range wheelSeriesNames {
			key := prefix + wheel
			if v, ok := seriesValue(fams[name], labelKey, key); !ok {
				t.Fatalf("✘ 未 Run 时 %s 缺少 %s 序列（序列缺失 ≠ 0，告警规则会失明）", name, key)
			} else if v != 0 {
				t.Fatalf("✘ 未 Run 时 %s 的 %s 应为 0, got=%v", name, key, v)
			}
		}
	}

	// 分片序列同理（S68）：未 Run 时深度与容量都报 0（此刻没有通道，报「理论每片
	// 容量」会让 depth/capacity 算出 0% 而看着像健康运行），但序列本身必须在。
	for _, name := range []string{"touchgocore_timer_schedule_shard_depth",
		"touchgocore_timer_schedule_shard_capacity"} {
		for i := 0; i < localtimer.ScheduleShards(); i++ {
			key := strconv.Itoa(i)
			if v, ok := seriesValue(fams[name], "shard", key); !ok {
				t.Fatalf("✘ 未 Run 时 %s 缺少 shard=%s 序列", name, key)
			} else if v != 0 {
				t.Fatalf("✘ 未 Run 时 %s 的 shard=%s 应为 0, got=%v", name, key, v)
			}
		}
	}
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
	// 光比序列条数还证明不了「本采集器真的在 registry 里」——同名指标族也可能
	// 被别处注册出来。手动再注册一次必须撞 AlreadyRegistered，才算坐实。
	if err := metrics.Registry().Register(timerBacklogCollector{}); err == nil {
		t.Fatal("✘ 重复注册竟然成功，说明 InitMetrics 里根本没把采集器装进去")
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
