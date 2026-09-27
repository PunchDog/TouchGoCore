package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ==================== M6：LoadBatch 失败必须给本次全部 miss 键登记退避 ====================
//
// 事故形状：批量回源整体失败（源挂了/超时）时只给 misses[0] 记退避，其余键在
// FailBackoff 窗口内继续被放行去回源——退避表本来是「不重复打一个已经挂掉的 DB」
// 的闸门，只锁一个键等于没锁，DB 故障期间批量读会把压力原样放大回去。
// 修复：失败时对本次全部 miss 键登记退避；成功时把本次 miss 键的退避清掉
// （与单键 reload 的 fails.Delete 对称），不让旧失败把恢复后的请求继续挡在门外。

// failBatchLoader 批量回源与单键回源都失败（err 非 nil 时），用于验证退避登记。
// 不实现 KeyBinder：本组用例只关心失败路径，键对齐由 keybind_test.go 覆盖。
type failBatchLoader struct {
	mu         sync.Mutex
	batchCalls int
	loadCalls  int
	err        error
}

func (l *failBatchLoader) Load(_ context.Context, _ string) (*testVal, error) {
	l.mu.Lock()
	l.loadCalls++
	l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	return nil, ErrNotFound
}

func (l *failBatchLoader) LoadBatch(_ context.Context, _ []string) (map[string]*testVal, error) {
	l.mu.Lock()
	l.batchCalls++
	err := l.err
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return map[string]*testVal{}, nil
}

func (l *failBatchLoader) counts() (batch, load int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.batchCalls, l.loadCalls
}

func (l *failBatchLoader) restore() {
	l.mu.Lock()
	l.err = nil
	l.mu.Unlock()
}

var _ BatchLoader[string, testVal] = (*failBatchLoader)(nil)

// 核心回归：批量回源失败后，本次全部 miss 键都在退避表里，
// 且退避窗口内单键路径一次都不打 DB（修复前只有 misses[0] 被挡住）。
func TestMGetOrLoad_BatchFailureBacksOffAllMissKeys(t *testing.T) {
	ld := &failBatchLoader{err: errors.New("db down")}
	h := newHarness(t, WithLoader[string, testVal](ld))
	ctx := context.Background()
	keys := []string{"k1", "k2", "k3", "k4"}

	if _, err := h.c.MGetOrLoad(ctx, keys); err == nil {
		t.Fatal("批量回源失败必须把错误报给调用方")
	}
	if b, _ := ld.counts(); b != 1 {
		t.Fatalf("首次应打一次 DB: batchCalls=%d", b)
	}

	// 修复点：每个 miss 键都进了退避表（此前只有 misses[0]）
	for _, k := range keys {
		if _, ok := h.c.fails.Load(h.c.KeyOf(k)); !ok {
			t.Fatalf("键 %s 未登记退避：LoadBatch 失败应对本次全部 miss 键登记", k)
		}
	}

	// 退避窗口内：所有键都不得再打 DB
	for _, k := range keys {
		if _, err := h.c.GetOrLoad(ctx, k); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("退避窗口内 %s 期望 ErrSourceUnavailable，得 %v", k, err)
		}
	}
	if _, l := ld.counts(); l != 0 {
		t.Fatalf("退避窗口内不得回源: loadCalls=%d", l)
	}
	if b, _ := ld.counts(); b != 1 {
		t.Fatalf("退避窗口内不该再打批量: batchCalls=%d", b)
	}
	if s := h.c.Stats().Snapshot(); s.LoadErr == 0 {
		t.Fatalf("批量回源失败应计入 LoadErr: %+v", s)
	}

	// 越过退避窗口后恢复回源（证明挡住的是窗口而不是永久）
	h.clk.Add(h.c.cfg.FailBackoff + time.Millisecond)
	if _, err := h.c.GetOrLoad(ctx, keys[1]); err == nil || errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("退避过期后应重新打源并透传源错误，得 %v", err)
	}
	if _, l := ld.counts(); l != 1 {
		t.Fatalf("退避过期后应恰好回源一次: loadCalls=%d", l)
	}
}

// 只锁一个键的反证：把 keys[0] 之外的键换成一次新的批量请求，
// 修复前它们会立刻重打 DB。这里用「批量失败 → 单键全部被挡」的调用次数钉住。
func TestMGetOrLoad_BatchBackoffIsNotJustFirstKey(t *testing.T) {
	ld := &failBatchLoader{err: errors.New("db down")}
	h := newHarness(t, WithLoader[string, testVal](ld))
	ctx := context.Background()

	if _, err := h.c.MGetOrLoad(ctx, []string{"a1", "a2", "a3"}); err == nil {
		t.Fatal("应报错")
	}
	// 逐个键单独请求：修复前 a2/a3 会各自再打一次 DB（loadCalls 变 2）
	blocked := 0
	for _, k := range []string{"a1", "a2", "a3"} {
		if _, err := h.c.GetOrLoad(ctx, k); errors.Is(err, ErrSourceUnavailable) {
			blocked++
		}
	}
	if blocked != 3 {
		t.Fatalf("三个键都应在退避窗口内被挡，实际挡住 %d 个", blocked)
	}
	if _, l := ld.counts(); l != 0 {
		t.Fatalf("退避期内一次回源都不该发生: loadCalls=%d", l)
	}
}

// 批量回源成功后必须清掉本次 miss 键的退避：源已恢复，
// 旧失败不得继续把后续单键请求挡在门外（与 reload 成功清 fails 对称）。
func TestMGetOrLoad_BatchSuccessClearsBackoff(t *testing.T) {
	ld := &failBatchLoader{err: errors.New("db down")}
	cfg := testConfig()
	cfg.NegativeTTL = 0 // 不写空标记，保证后续单键请求真的会走到回源判定
	h := newHarness(t, WithConfig[string, testVal](cfg), WithLoader[string, testVal](ld))
	ctx := context.Background()
	keys := []string{"k1", "k2"}

	if _, err := h.c.MGetOrLoad(ctx, keys); err == nil {
		t.Fatal("首次批量失败应报错")
	}
	for _, k := range keys {
		if _, ok := h.c.fails.Load(h.c.KeyOf(k)); !ok {
			t.Fatalf("前置：%s 应已登记退避", k)
		}
	}

	ld.restore() // 源恢复
	h.clk.Add(cfg.FailBackoff + time.Millisecond)
	if _, err := h.c.MGetOrLoad(ctx, keys); err != nil {
		t.Fatalf("源恢复后批量回源应成功: %v", err)
	}
	for _, k := range keys {
		if _, ok := h.c.fails.Load(h.c.KeyOf(k)); ok {
			t.Fatalf("批量成功后 %s 的退避应被清掉（否则恢复后仍被误挡）", k)
		}
	}
	if _, err := h.c.GetOrLoad(ctx, keys[0]); errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("退避已清，单键路径不该再报源不可用")
	}
}
