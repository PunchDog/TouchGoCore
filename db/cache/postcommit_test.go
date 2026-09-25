package cache

import (
	"context"
	"errors"
	"testing"
)

// ==================== post-commit 队列 ====================

func TestPostCommit_AppendAndRunInOrder(t *testing.T) {
	ctx, err := WithPostCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	mk := func(s string) PostCommitAction {
		return func(context.Context) error { seq = append(seq, s); return nil }
	}
	if err := AppendPostCommit(ctx, mk("a"), mk("b")); err != nil {
		t.Fatal(err)
	}
	if err := AppendPostCommit(ctx, mk("c")); err != nil {
		t.Fatal(err)
	}
	if len(seq) != 0 {
		t.Fatalf("未 Run 前不得执行任何动作: %v", seq)
	}
	if err := RunPostCommit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seq) != 3 || seq[0] != "a" || seq[1] != "b" || seq[2] != "c" {
		t.Fatalf("必须按登记顺序执行: %v", seq)
	}
}

// 回滚路径：只 Append 不 Run —— Redis 一个字节都不该被动过（资金事务的硬要求）。
func TestPostCommit_RollbackNeverRuns(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, err := WithPostCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := InvalidateOnCommit(ctx, h.c, "k1"); err != nil {
		t.Fatal(err)
	}
	// 模拟回滚：不 RunPostCommit
	if _, err := h.c.Get(context.Background(), "k1"); err != nil {
		t.Fatalf("回滚路径不得触碰 Redis，值应仍在: %v", err)
	}
}

func TestPostCommit_NoSinkIsLoud(t *testing.T) {
	// 忘记挂队列 = 忘记失效，必须报错而不是静默丢弃
	if err := AppendPostCommit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrNoPostCommitSink) {
		t.Fatalf("未挂队列应返回 ErrNoPostCommitSink，得 %v", err)
	}
	if err := RunPostCommit(context.Background()); err != nil {
		t.Fatalf("未挂队列的 Run 应为 nil（本次事务无失效需求）: %v", err)
	}
}

func TestPostCommit_RejectNestedMount(t *testing.T) {
	ctx, err := WithPostCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithPostCommit(ctx); !errors.Is(err, ErrPostCommitNested) {
		t.Fatalf("重复挂载应返回 ErrPostCommitNested，得 %v", err)
	}
}

func TestPostCommit_AppendAfterRunIsLoud(t *testing.T) {
	ctx, _ := WithPostCommit(context.Background())
	if err := AppendPostCommit(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := RunPostCommit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := AppendPostCommit(ctx, func(context.Context) error { return nil }); !errors.Is(err, ErrPostCommitRan) {
		t.Fatalf("队列已执行后再排动作必须报错（否则永不被运行），得 %v", err)
	}
}

func TestPostCommit_AllActionsRunDespiteFirstFailure(t *testing.T) {
	ctx, _ := WithPostCommit(context.Background())
	ran := 0
	ok := func(context.Context) error { ran++; return nil }
	bad := errors.New("redis 抖动")
	if err := AppendPostCommit(ctx, func(context.Context) error { return bad }, ok, ok); err != nil {
		t.Fatal(err)
	}
	err := RunPostCommit(ctx)
	if ran != 2 {
		t.Fatalf("首个失败不得中断后续动作: ran=%d", ran)
	}
	if err == nil || !errors.Is(err, bad) {
		t.Fatalf("应返回聚合错误并含原错误: %v", err)
	}
}

func TestPostCommit_RejectNilAction(t *testing.T) {
	ctx, _ := WithPostCommit(context.Background())
	if err := AppendPostCommit(ctx, nil); err == nil {
		t.Fatal("nil 动作必须报错")
	}
}

// ==================== Invalidate / InvalidateOnCommit ====================

func TestInvalidate_NilCacheIsNoop(t *testing.T) {
	var c *Cache[string, testVal]
	act := Invalidate(context.Background(), c, "k1")
	if act == nil {
		t.Fatal("必须返回非 nil 空动作，免得调用方写 nil 分支")
	}
	if err := act(context.Background()); err != nil {
		t.Fatalf("无缓存时失效应是空动作: %v", err)
	}
}

func TestInvalidate_OnCommitDeletesRedisOnly(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 7}); err != nil {
		t.Fatal(err)
	}
	ctx, _ := WithPostCommit(context.Background())
	if err := InvalidateOnCommit(ctx, h.c, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := RunPostCommit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Get(context.Background(), "k1"); !errors.Is(err, ErrMiss) {
		t.Fatalf("失效后 Redis 应无值: %v", err)
	}
	if s, _ := sn.counts(); s != 0 {
		t.Fatalf("失效动作绝不能落库（库里由业务事务写真源）: saves=%d", s)
	}
}

// 非事务 ctx 上调用：立即失效 + 告警，但仍不得抛错（此时没有未提交需要保护）
func TestInvalidateOnCommit_PlainCtxRunsImmediately(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := InvalidateOnCommit(context.Background(), h.c, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Get(context.Background(), "k1"); !errors.Is(err, ErrMiss) {
		t.Fatalf("应立即失效: %v", err)
	}
}
