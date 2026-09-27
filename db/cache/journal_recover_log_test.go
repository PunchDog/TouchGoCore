package cache

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ==================== L1：recoverJournal 中止日志的「剩余条数」语义 ====================
//
// 事故形状：超时中止那条告警写的是「剩余 %s 条」，传进去的却是 je.KeyStr（键名），
// 打出来是「剩余 u|k1 条」——运维看到的是一个键而不是条数，既读不懂也数不出来，
// 恢复进度彻底失真。修复：改成剩余条数（len(entries)-i）。
//
// vars 包没有日志捕获设施（Warning/Error 直接走 console+文件通道），无法在单测里
// 抓日志原文，因此把文案抽成 recoverAbortMsg 纯函数：语义可断言、可反证，
// 调用点仍是一条 vars.Warning。

// 文案必须报「剩余 N 条」，且 N 随剩余条数变化；不得出现格式化残留或键名。
func TestRecoverAbortMsgReportsRemainingCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining int
		want      string
	}{
		{"user", 1, "剩余 1 条"},
		{"user", 7, "剩余 7 条"},
		{"order", 0, "剩余 0 条"},
		{"order", 1024, "剩余 1024 条"},
	} {
		got := recoverAbortMsg(tc.name, tc.remaining)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("recoverAbortMsg(%q,%d)=%q 应含 %q", tc.name, tc.remaining, got, tc.want)
		}
		if !strings.Contains(got, "cache["+tc.name+"]") {
			t.Fatalf("告警应带 cache 名: %q", got)
		}
		// 格式化残留 = 占位符与实参类型不匹配（旧 bug 正是 %s 配键名）
		if strings.Contains(got, "%!") {
			t.Fatalf("存在格式化不匹配残留: %q", got)
		}
		if strings.Contains(got, jOpUpsertPrefix) || strings.Contains(got, jOpDeletePrefix) {
			t.Fatalf("剩余条数位置不得是账本键名: %q", got)
		}
	}
	// 反证：条数不同文案必须不同（否则断言只是空转）
	if recoverAbortMsg("user", 2) == recoverAbortMsg("user", 3) {
		t.Fatal("剩余条数没有进入文案")
	}
}

// slowJScan 把扫账本身变慢，用于确定性地耗光恢复预算。
// 中止分支只在「JScan 成功之后、循环第一次查预算」时才可达；单靠 ReadTimeout=1ns
// 的 30ns 定时器与循环赛跑不可确定（整包运行时定时器 goroutine 未必抢在前面，
// 会变成偶发落库 3 条），于是把真实耗时放在 JScan 里，用 20ms 换确定性。
type slowJScan struct {
	*fakeKV
	delay time.Duration
}

func (s *slowJScan) JScan(ctx context.Context, zkey string) ([]JournalEntry, error) {
	time.Sleep(s.delay)
	return s.fakeKV.JScan(ctx, zkey)
}

var _ Journaler = (*slowJScan)(nil)

// 行为面：恢复超时中止时，剩余账目必须原样留待下次启动，且中止点的剩余条数
// 就是账本里还没销的条数——文案报的数与真实待恢复量一致。
func TestRecoverJournal_TimeoutAbortKeepsRemainingEntries(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	// 先攒 3 条账（模拟上次进程被杀时缓冲丢失、账本还在）
	c1 := journalHarness(t, kv, clk, newRecSaver())
	for i := 0; i < 3; i++ {
		if err := c1.Write(ctx, string(rune('a'+i)), &testVal{N: 100 + i}); err != nil {
			t.Fatal(err)
		}
	}
	if jm := kv.journalOf(c1.jz); len(jm) != 3 {
		t.Fatalf("前置：账本应有 3 条，得 %v", jm)
	}

	// 「重启」后的 Cache：ReadTimeout=1ns → 恢复预算 30ns；JScan 被拖慢 20ms，
	// 扫完账时预算必然已过期，循环第一轮就走中止分支（remaining = 全部 3 条）
	cfg := testConfig()
	cfg.FlushInterval = time.Hour
	cfg.ReadTimeout = time.Nanosecond
	sn2 := newRecSaver()
	ld2 := &mapLoader{data: map[string]*testVal{}, notFound: map[string]bool{}}
	c2, err := New[string, testVal]("t", kv,
		WithConfig[string, testVal](cfg), WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld2), WithSaver[string, testVal](sn2),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c2.jr = &slowJScan{fakeKV: kv, delay: 20 * time.Millisecond}
	c2.recoverJournal(ctx)

	if s, _ := sn2.counts(); s != 0 {
		t.Fatalf("超时中止后不该落库，saves=%d", s)
	}
	jm := kv.journalOf(c2.jz)
	if len(jm) != 3 {
		t.Fatalf("中止必须把剩余账目原样留待下次，得 %v", jm)
	}
	// 分支判别：JournalErr=0 证明扫账成功（没走「扫账失败跳过恢复」分支），
	// 叠加 0 落库 + 账本原封 + Recovered/RecoverMiss 均为 0，只能是「超时中止」分支
	if snap := c2.Stats().Snapshot(); snap.Recovered != 0 || snap.RecoverMiss != 0 || snap.JournalErr != 0 {
		t.Fatalf("中止路径不该动任何计数（尤其 JournalErr!=0 说明根本没扫到账）: %+v", snap)
	}
	// 文案报的剩余条数与账本真实待恢复量一致
	msg := recoverAbortMsg(c2.name, len(jm))
	if !strings.Contains(msg, "剩余 3 条") {
		t.Fatalf("剩余条数应与账本条数一致: %q", msg)
	}

	// 反证（防用例空转）：同一份账本换成正常预算就能全部重放，
	// 说明上面确实是「超时中止」而不是账本/装配本身坏了。
	sn3 := newRecSaver()
	c3 := journalHarness(t, kv, clk, sn3)
	c3.recoverJournal(ctx)
	if s, _ := sn3.counts(); s != 3 {
		t.Fatalf("正常预算下应重放 3 条，得 %d", s)
	}
	if jm3 := kv.journalOf(c3.jz); len(jm3) != 0 {
		t.Fatalf("重放成功应销账: %v", jm3)
	}
	if snap := c3.Stats().Snapshot(); snap.Recovered != 3 {
		t.Fatalf("Recovered 计数: %+v", snap)
	}
}
