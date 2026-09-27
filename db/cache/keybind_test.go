package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ==================== M1：批量回源键不一致（late-binding KeyOf） ====================
//
// 事故形状：MysqlBatchSource 的回填 map 用 keyFn(id) 当键（裸键串），而查询侧
// MGetOrLoad 用 c.KeyOf(key) 去取（带 {prefix}:{group}:{name}: 前缀且 sanitize 过）。
// 两边各算一半 → 一条都对不上 → 批量回源全部落空，还顺手给每个 miss 写了空标记。
// 症状是「命中率异常低 + 每次 MGetOrLoad 都打一次 DB 但一个结果也用不上」，
// 全程无报错，属于最难查的静默退化。
//
// 修复（late-binding）：包内定义 KeyBinder 接口，Cache.New 末尾把 c.keyOfRaw 注入
// BatchLoader；MysqlBatchSource 回填统一走绑定后的 KeyOf；未绑定就调 LoadBatch
// 直接返回 ErrKeyOfUnbound 而不是静默给一堆错键结果。db/cache_api.go 的
// NewMysqlCacheBatchSource 签名不变。

// kbRow 批量回源键映射用的行实体（带主键列）
type kbRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// fakeBatchLoader 与修复后的 MysqlBatchSource 同形：结果 map 的键由 Cache.New
// 延迟绑定的 keyOf 生成，而不是它自己手里的裸键串。
// bare=true 时复现修复前的错键形状，用作回归见证。
type fakeBatchLoader struct {
	data map[string]*kbRow
	bare bool // true=回填裸键（旧 bug 形状）

	mu      sync.RWMutex
	keyOf   func(string) string
	batches int
}

func (l *fakeBatchLoader) BindKeyOf(f func(rawKey string) string) {
	l.mu.Lock()
	l.keyOf = f
	l.mu.Unlock()
}

func (l *fakeBatchLoader) boundKeyOf() func(string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.keyOf
}

func (l *fakeBatchLoader) batchCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.batches
}

func (l *fakeBatchLoader) Load(_ context.Context, key string) (*kbRow, error) {
	if v, ok := l.data[key]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}

func (l *fakeBatchLoader) LoadBatch(_ context.Context, keys []string) (map[string]*kbRow, error) {
	keyOf := l.boundKeyOf()
	if !l.bare && keyOf == nil {
		return nil, ErrKeyOfUnbound
	}
	l.mu.Lock()
	l.batches++
	l.mu.Unlock()
	out := make(map[string]*kbRow, len(keys))
	for _, k := range keys {
		v, ok := l.data[k]
		if !ok {
			continue // 源里没有的键不出现在结果里（契约见 BatchLoader 注释）
		}
		if l.bare {
			out[k] = v // 旧 bug：裸键，查询侧用 KeyOf 永远取不到
			continue
		}
		out[keyOf(k)] = v
	}
	return out, nil
}

var (
	_ BatchLoader[string, kbRow] = (*fakeBatchLoader)(nil)
	_ KeyBinder                  = (*fakeBatchLoader)(nil)
)

// namedCache 建一个带自定义命名空间的 Cache（键形 {prefix}:{group}:{name}:{key}）
func namedCache(t *testing.T, name, prefix, group string, ld Loader[string, kbRow]) (*Cache[string, kbRow], *fakeKV) {
	t.Helper()
	clk := newFakeClock()
	kv := newFakeKV(clk)
	c, err := New[string, kbRow](name, kv,
		WithConfig[string, kbRow](testConfig()),
		WithClock[string, kbRow](clk.Now),
		WithPrefix[string, kbRow](prefix),
		WithGroup[string, kbRow](group),
		WithLoader[string, kbRow](ld),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, kv
}

// TestMapBatchRows_KeysMatchKeyOf 回填键与查询侧 KeyOf 逐字一致，
// 且确实带上了命名空间前缀与 sanitize（":" 分段、超长键收敛）。
func TestMapBatchRows_KeysMatchKeyOf(t *testing.T) {
	long := strings.Repeat("k", 200) // >128 → sanitizeKey 会截断 + sha1 摘要
	rows := []*kbRow{
		{ID: "u1", Name: "a"},
		{ID: "tenant:42:u2", Name: "b"}, // 含 ":"，会破坏命名空间分段，必须被 sanitize
		{ID: long, Name: "c"},
	}
	keyFn := func(k string) string { return k }
	idOf := func(r *kbRow) string { return r.ID }

	for _, tc := range []struct{ name, prefix, group string }{
		{"user", "tg", ""},
		{"user", "game", "s1"},
	} {
		t.Run(tc.name+"_"+tc.prefix+"_"+tc.group, func(t *testing.T) {
			c, _ := namedCache(t, tc.name, tc.prefix, tc.group, nil)
			got := mapBatchRows(rows, keyFn, idOf, c.keyOfRaw)

			if len(got) != len(rows) {
				t.Fatalf("回填条数: got=%d want=%d", len(got), len(rows))
			}
			for _, r := range rows {
				want := c.KeyOf(r.ID) // 查询侧用的就是它
				if got[want] != r {
					t.Fatalf("回填键与 KeyOf 不一致：KeyOf=%q 取不到该行（got 键=%v）", want, mapKeys(got))
				}
				// 反证：裸键确实对不上（否则上面那条断言只是空转）
				if r.ID != want {
					if _, ok := got[r.ID]; ok {
						t.Fatalf("回填不得用裸键 %q（KeyOf 是 %q）", r.ID, want)
					}
				}
			}
			// 前缀与 sanitize 真的生效了
			if !strings.HasPrefix(c.KeyOf("u1"), tc.prefix+":") {
				t.Fatalf("键缺少命名空间前缀: %q", c.KeyOf("u1"))
			}
			// ":" 必须被 sanitize 成 "_"，否则会多出命名空间分段
			if seg := c.KeyOf("tenant:42:u2"); !strings.HasSuffix(seg, "tenant_42_u2") {
				t.Fatalf("\":\" 应被 sanitize 成 \"_\": %q", seg)
			}
			if len(c.KeyOf(long)) > 160 {
				t.Fatalf("超长键应被收敛: len=%d", len(c.KeyOf(long)))
			}
		})
	}
}

func mapKeys(m map[string]*kbRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMysqlBatchSource_BindsKeyOfAndFailsUnbound 真实构造函数的契约：
// 返回值实现 KeyBinder；绑定前调 LoadBatch 报错而不是静默给错键结果；
// Cache.New / Register 之后自动绑定，且绑定的就是该 Cache 的 KeyOf。
// 用 nil Repository 构造：keyToID/idOf 只在真正查库时才解引用，这里不查。
func TestMysqlBatchSource_BindsKeyOfAndFailsUnbound(t *testing.T) {
	ctx := context.Background()
	bl := MysqlBatchSource[testVal, string](nil, func(k string) any { return k }, "id", nil, nil)
	if bl == nil {
		t.Fatal("MysqlBatchSource 返回 nil")
	}
	kb, ok := bl.(KeyBinder)
	if !ok {
		t.Fatal("MysqlBatchSource 的返回值必须实现 KeyBinder，否则 Cache.New 无从注入")
	}
	if _, err := bl.LoadBatch(ctx, []string{"a"}); !errors.Is(err, ErrKeyOfUnbound) {
		t.Fatalf("未绑定时 LoadBatch 必须返回 ErrKeyOfUnbound，实际: %v", err)
	}

	inner, ok := bl.(*batchLoader[string, testVal])
	if !ok {
		t.Fatalf("内部类型变了？%T", bl)
	}
	if inner.boundKeyOf() != nil {
		t.Fatal("前置：尚未绑定")
	}

	clk := newFakeClock()
	kv := newFakeKV(clk)
	c, err := New[string, testVal]("mb", kv,
		WithConfig[string, testVal](testConfig()),
		WithClock[string, testVal](clk.Now),
		WithPrefix[string, testVal]("game"),
		WithGroup[string, testVal]("s7"),
		WithLoader[string, testVal](bl),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	keyOf := inner.boundKeyOf()
	if keyOf == nil {
		t.Fatal("Cache.New 应已把 keyOfRaw 注入 BatchLoader")
	}
	if got, want := keyOf("u1"), c.KeyOf("u1"); got != want {
		t.Fatalf("注入的映射必须等价于 KeyOf: got=%q want=%q", got, want)
	}
	if got := keyOf("u1"); !strings.HasPrefix(got, "game:s7:mb:") {
		t.Fatalf("绑定后的键应含本 Cache 的命名空间: %q", got)
	}
	_ = kb

	// Layer 路径同样绑定（Register → New）
	bl2 := MysqlBatchSource[testVal, string](nil, func(k string) any { return k }, "id", nil, nil)
	l := NewLayer(kv, WithLayerConfig(testConfig()))
	c2, err := Register[string, testVal](l, "reg", WithLoader[string, testVal](bl2))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	inner2 := bl2.(*batchLoader[string, testVal])
	if inner2.boundKeyOf() == nil {
		t.Fatal("Register 路径也应完成绑定")
	}
	if got, want := inner2.boundKeyOf()("k"), c2.KeyOf("k"); got != want {
		t.Fatalf("Register 注入的映射应等价于该 Cache 的 KeyOf: got=%q want=%q", got, want)
	}
}

// TestCache_NewBindsKeyOfPerNamespace 两个不同命名空间的 Cache 各自绑定各自的 KeyOf：
// 回填键必须跟着自己的 Cache 走，绝不能串到别人的键空间。
func TestCache_NewBindsKeyOfPerNamespace(t *testing.T) {
	blA := &fakeBatchLoader{data: map[string]*kbRow{}}
	blB := &fakeBatchLoader{data: map[string]*kbRow{}}
	cA, _ := namedCache(t, "user", "game", "s1", blA)
	cB, _ := namedCache(t, "user", "other", "s2", blB)

	if got, want := blA.boundKeyOf()("u1"), cA.KeyOf("u1"); got != want {
		t.Fatalf("A 绑定错: got=%q want=%q", got, want)
	}
	if got, want := blB.boundKeyOf()("u1"), cB.KeyOf("u1"); got != want {
		t.Fatalf("B 绑定错: got=%q want=%q", got, want)
	}
	if blA.boundKeyOf()("u1") == blB.boundKeyOf()("u1") {
		t.Fatal("两个命名空间的键必须不同，否则批量回源会互相覆盖")
	}
}

// TestMGetOrLoad_BatchKeysAligned 端到端：绑定后回填键与查询侧对齐，
// 结果可用、Redis 被正确回填，第二次调用直接命中缓存不再打 DB。
func TestMGetOrLoad_BatchKeysAligned(t *testing.T) {
	data := map[string]*kbRow{
		"u1":                     {ID: "u1", Name: "a"},
		"tenant:42:u2":           {ID: "tenant:42:u2", Name: "b"},
		strings.Repeat("k", 200): {ID: strings.Repeat("k", 200), Name: "c"},
	}
	bl := &fakeBatchLoader{data: data}
	c, _ := namedCache(t, "user", "game", "s1", bl)
	ctx := context.Background()

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	got, err := c.MGetOrLoad(ctx, keys)
	if err != nil {
		t.Fatalf("MGetOrLoad: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("批量回源结果条数: got=%d want=%d（键不对齐就会全部落空）", len(got), len(keys))
	}
	for _, k := range keys {
		v := got[c.KeyOf(k)]
		if v == nil || v.ID != k {
			t.Fatalf("KeyOf(%q)=%q 取不到正确结果: %+v", k, c.KeyOf(k), v)
		}
	}
	if n := bl.batchCount(); n != 1 {
		t.Fatalf("首次应只打一次 DB: %d", n)
	}
	// 第二次必须全部命中 Redis——回填真的写进去了，而不是白打一次 DB
	got2, err := c.MGetOrLoad(ctx, keys)
	if err != nil {
		t.Fatalf("二次 MGetOrLoad: %v", err)
	}
	if len(got2) != len(keys) {
		t.Fatalf("二次结果条数: %d", len(got2))
	}
	if n := bl.batchCount(); n != 1 {
		t.Fatalf("二次调用不该再回源（说明首次回填没落进 Redis）: batches=%d", n)
	}
	if s := c.Stats().Snapshot(); s.Hits == 0 {
		t.Fatalf("二次调用应有缓存命中: %+v", s)
	}
}

// TestMGetOrLoad_BareKeysAreUseless 回归见证：把回填键换回修复前的裸键串形状，
// 批量回源一条也用不上、每次调用都白打一次 DB，还给每个 miss 写了空标记。
// 这条用例存在的意义是让「键对齐」有反证——否则上面那条断言可能只是空转。
func TestMGetOrLoad_BareKeysAreUseless(t *testing.T) {
	data := map[string]*kbRow{"u1": {ID: "u1", Name: "a"}, "u2": {ID: "u2", Name: "b"}}
	bl := &fakeBatchLoader{data: data, bare: true}
	c, kv := namedCache(t, "user", "game", "s1", bl)
	ctx := context.Background()

	got, err := c.MGetOrLoad(ctx, []string{"u1", "u2"})
	if err != nil {
		t.Fatalf("MGetOrLoad: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("裸键回填应一条也用不上，却拿到了 %d 条——反证没立住", len(got))
	}
	// 反复调用：每次都白打一次 DB，永远填不上
	for range 3 {
		if _, err := c.MGetOrLoad(ctx, []string{"u1", "u2"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := bl.batchCount(); n != 4 {
		t.Fatalf("每次调用都应白打一次 DB: batches=%d", n)
	}
	// 更糟的后果：给每个 miss 写了空标记，把真实数据挡在缓存外面
	for _, k := range []string{"u1", "u2"} {
		raw, ok := kv.rawOf(c.KeyOf(k))
		if !ok {
			t.Fatalf("裸键退化路径下 %s 应被写了空标记", k)
		}
		e, err := decode[kbRow](c.codec, raw)
		if err != nil || !e.Null {
			t.Fatalf("应是空标记: raw=%q err=%v", raw, err)
		}
	}
}

// TestFakeBatchLoader_UnboundIsLoud 绑定前调 LoadBatch 必须报错（与 MysqlBatchSource 同契约）
func TestFakeBatchLoader_UnboundIsLoud(t *testing.T) {
	bl := &fakeBatchLoader{data: map[string]*kbRow{"u1": {ID: "u1"}}}
	if _, err := bl.LoadBatch(context.Background(), []string{"u1"}); !errors.Is(err, ErrKeyOfUnbound) {
		t.Fatalf("未绑定应返回 ErrKeyOfUnbound: %v", err)
	}
	bl.BindKeyOf(func(raw string) string { return "tg:t:" + raw })
	got, err := bl.LoadBatch(context.Background(), []string{"u1"})
	if err != nil {
		t.Fatal(err)
	}
	if got["tg:t:u1"] == nil {
		t.Fatalf("绑定后应按注入的 KeyOf 回填: %v", mapKeys(got))
	}
}

// 编译期确认：KeyBinder 是可选窄接口，未实现它的 BatchLoader 仍然可用
// （Cache.New 只在类型断言成功时注入，不改变既有行为）。
var _ BatchLoader[string, kbRow] = plainBatchLoader{}

type plainBatchLoader struct{}

func (plainBatchLoader) Load(_ context.Context, key string) (*kbRow, error) { return nil, ErrNotFound }
func (plainBatchLoader) LoadBatch(_ context.Context, _ []string) (map[string]*kbRow, error) {
	return nil, nil
}
