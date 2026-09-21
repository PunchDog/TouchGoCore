package localtimer

// ============================================================================
// 跨档下沉精度回归（S67 复核整改）：桶化只收窄扫描范围，绝不能把粗档变成晚点源。
//
// 一档轮的 ticker 周期恰好等于该档精度，于是一个桶一拍只被看一次，看点在
// 「格起点 + 该协程的固定节拍相位 δ」。节点到期时刻落在格内的偏移 r 与 δ 无关：
// δ >= r 时这一拍看到它已到期，直接从粗档派发，晚点 δ-r 最大逼近一整格
// （秒档 1s、分档 60s、10 分档 600s）。变更前每拍全扫，remaining 一跌破本档下界
// 就下沉到更精确的一档，最终由毫秒轮按 1ms 兑现。
//
// 实测（本机 i9-12900H）：补 lookahead 前，40 个 1500ms 周期定时器 160 次派发
// 全部稳定晚 500ms、wheelMigrations 恒为 0；补上后同样负载晚点 0~1ms、迁移计数 > 0。
// 这两条一起构成回归判据，缺一条就退化成「看起来一切正常」。
// ============================================================================

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"touchgocore/util"
)

type tierProbeTimer struct {
	Timer
	mu   sync.Mutex
	late []int64
}

// Tick 在续期之前执行（executeTimer 先回调后 HasNext/addTimerWithRetry），
// 所以此刻 nextTime 仍是本拍的标称到期时刻，差值即真实晚点。
func (p *tierProbeTimer) Tick() {
	d := util.CurrentMS() - p.nextTime.Load()
	p.mu.Lock()
	p.late = append(p.late, d)
	p.mu.Unlock()
}

// TestSecondTierDispatchStaysPrecise 秒档周期定时器必须按毫秒精度兑现：
// 1500ms 的定时器本该在到期那一瞬被下沉到毫秒轮，而不是由秒轮攒到下一拍整批发出去。
func TestSecondTierDispatchStaysPrecise(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 8 秒观察窗口累计 5 轮续期样本，-short 跳过")
	}
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	const num = 40
	probes := make([]*tierProbeTimer, 0, num)
	for i := 0; i < num; i++ {
		tm, err := NewTimer[*tierProbeTimer](1500, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, tm)
	}

	migBase := m.GetStats().WheelMigrations
	time.Sleep(8 * time.Second) // 约 5 轮续期，样本量足够让「相位巧合全中」的概率归零
	for _, p := range probes {
		p.Pause()
	}

	var all []int64
	for _, p := range probes {
		p.mu.Lock()
		all = append(all, p.late...)
		p.mu.Unlock()
	}
	if len(all) < num*2 {
		t.Fatalf("✘ 样本太少不足以判定: %d（%d 个定时器 × 至少 2 轮）", len(all), num)
	}

	// 判据一：确有 tick 驱动的跨档下沉。只看晚点会漏掉一种退化形态——节点一直待在
	// 秒档、靠 lookahead 那格刚好在到期前几毫秒被派发，晚点看着正常而迁移计数为 0。
	if mig := m.GetStats().WheelMigrations - migBase; mig <= 0 {
		t.Fatalf("✘ 全程未发生任何 tick 驱动的跨档迁移（计数增量=%d），粗档仍在自己派发到期项", mig)
	}

	// 判据二：晚点受控。阈值取 400ms，卡在「一整格 1000ms」与「毫秒级」之间：
	// 回归形态是恒定 500ms（一半格），正常形态是 0~1ms，全量测试并行抢 CPU 时
	// 消费端排队会抬高尾值，400ms 留得住噪声又仍能把 500ms 那一档判红。
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	const maxLate = int64(400)
	if p50, mx := all[len(all)/2], all[len(all)-1]; p50 > maxLate || mx > maxLate*2 {
		t.Fatalf("✘ 秒档派发晚点过大: p50=%dms max=%dms（阈值 %dms，回归形态恒为 500ms）",
			p50, mx, maxLate)
	}
	t.Logf("✔ 秒档 %d 次派发：迁移 %d 次，晚点 p50=%dms max=%dms",
		len(all), m.GetStats().WheelMigrations-migBase, all[len(all)/2], all[len(all)-1])
}
