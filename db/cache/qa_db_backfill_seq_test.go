package cache

import (
	"context"
	"testing"
	"time"
)

// ==================== P2：读线回填必须走 seq-guarded 写 ====================
//
// 事故形状：reload 单键回填与 MGetOrLoad 批量回填用裸 Set 写 Redis。
// 「buf.has 脏检查」与「Set」之间若并发 Write 落了更高 seq 的新值，
// 回填会用源库旧值把它盖掉 → 违 Redis 优先。修复：回填统一走
// setEnvelopeSeq（seqSetScript CAS：仅当待写 seq 不小于现存 seq 才落地）。

// 单键回填：Redis 已有高 seq 新值时，低 seq 的源库旧值不得覆盖它。
func TestReloadBackfill_LowSeqDoesNotOverwriteHighSeq(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ks := h.c.KeyOf("k1")

	// 预置：Redis 里是 seq=100 的新值（模拟并发 Write 刚落地的新行）
	raw, err := h.c.buildEnvelope(&testVal{N: 999}, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.kv.Set(ctx, ks, raw, time.Second); err != nil {
		t.Fatal(err)
	}

	// 回源拿到的是库里的旧值；c.seq 从 0 起 → 本次回填 seq=1（远小于 100）
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.reload(ks, "k1", h.ld); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got, ok := h.kv.rawOf(ks)
	if !ok {
		t.Fatal("键被删了")
	}
	if seq := envelopeSeqOf(got); seq != 100 {
		t.Fatalf("低 seq 旧回填覆盖了高 seq 新值: 现存 seq=%d, 期望 100", seq)
	}
	e, derr := decode[testVal](h.c.codec, got)
	if derr != nil {
		t.Fatal(derr)
	}
	if e.Value.N != 999 {
		t.Fatalf("值被降级: N=%d, 期望 999", e.Value.N)
	}
}

// staleBatchLoader 返回「库里的旧值」，键经 KeyBinder 绑定 KeyOf。
type staleBatchLoader struct {
	keyOf func(string) string
	vals  map[string]*testVal
}

func (l *staleBatchLoader) BindKeyOf(fn func(string) string) { l.keyOf = fn }

func (l *staleBatchLoader) Load(_ context.Context, key string) (*testVal, error) {
	if v, ok := l.vals[key]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}

func (l *staleBatchLoader) LoadBatch(_ context.Context, keys []string) (map[string]*testVal, error) {
	out := map[string]*testVal{}
	for _, k := range keys {
		if v, ok := l.vals[k]; ok {
			out[l.keyOf(k)] = v
		}
	}
	return out, nil
}

var _ BatchLoader[string, testVal] = (*staleBatchLoader)(nil)

// 批量回填：空标记（高 seq）在 Redis 里时，低 seq 批量回填不得覆盖。
func TestMGetOrLoadBackfill_LowSeqDoesNotOverwriteHighSeq(t *testing.T) {
	bl := &staleBatchLoader{vals: map[string]*testVal{"k1": {N: 1}}}
	h := newHarness(t, WithLoader[string, testVal](bl))
	ctx := context.Background()
	ks := h.c.KeyOf("k1")

	// 预置：seq=100 的空标记（MGet 视为 miss → 触发批量回源回填）
	raw, err := h.c.buildEnvelope(nil, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.kv.Set(ctx, ks, raw, time.Second); err != nil {
		t.Fatal(err)
	}

	if _, err := h.c.MGetOrLoad(ctx, []string{"k1"}); err != nil {
		t.Fatalf("MGetOrLoad: %v", err)
	}

	got, ok := h.kv.rawOf(ks)
	if !ok {
		t.Fatal("键被删了")
	}
	if seq := envelopeSeqOf(got); seq != 100 {
		t.Fatalf("低 seq 批量回填覆盖了高 seq 现存值: 现存 seq=%d, 期望 100", seq)
	}
	e, derr := decode[testVal](h.c.codec, got)
	if derr != nil {
		t.Fatal(derr)
	}
	if !e.Null {
		t.Fatalf("空标记被旧值顶掉: %+v", e)
	}
}
