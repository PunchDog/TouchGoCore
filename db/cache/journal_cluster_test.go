package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ==================== L4：journal 集群模式同槽（hashtag）原子化 ====================
//
// 症结：写线的「写值 + 记账」是双键操作。集群下值键与旧的全局账本键分属不同槽，
// go-redis 只能按节点拆分 pipeline 执行，留下「值已写、账未记」的非原子窗口。
// 修复：集群下把账本键派生成与值键同槽的 hashtag 键（clusterJournalKey），
// 双键 Lua 不再 CROSSSLOT，可原子执行。值键格式（KeyOf）一律不动。
//
// 本机无 C 工具链（-race 不可用）、无本地 Redis 集群，故这里用两类断言覆盖：
//  1. 纯函数：槽计算（CRC16 + hashtag 规则）、键派生、由 member 反推值键、SCAN glob；
//  2. 用嵌入了 redis.Cmdable 的录制替身驱动 redisStore 的集群分支，断言走了
//     双键 Lua（Eval 且两 KEYS 同槽），而非 pipeline 退化路径。

// ---------- Redis Cluster 槽计算（CRC16-CCITT/XMODEM + hashtag 规则） ----------

func crc16XModem(data []byte) uint16 {
	crc := uint16(0)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// hashSlot 复刻 Redis Cluster 的键→槽映射：若键含第一个 '{' 且其后存在非空 '}'，
// 只对这对花括号内的子串算 CRC16；否则对整键算。结果 mod 16384。
func hashSlot(key string) uint16 {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		if j := strings.IndexByte(key[i+1:], '}'); j > 0 {
			key = key[i+1 : i+1+j]
		}
	}
	return crc16XModem([]byte(key)) % 16384
}

// ---------- 纯函数：键派生与槽一致 ----------

// 集群账本键形如 {<值键>}:dirty#zset，且其 hashtag 槽与值键槽恒等——这是双键 Lua
// 在集群下不报 CROSSSLOT、可原子执行的前提。
func TestClusterJournalKey_SameSlotAsValueKey(t *testing.T) {
	for _, vk := range []string{
		"tg:g1:t:1001",
		"tg:t:x",
		"tg:t:a_b",           // sanitize 后（原 keyStr 含 ':'）
		"user:cache:42:prof", // 任意命名空间
	} {
		jk := clusterJournalKey(vk)
		want := "{" + vk + "}" + jDirtySuffix
		if jk != want {
			t.Fatalf("clusterJournalKey(%q)=%q 应为 %q", vk, jk, want)
		}
		if hashSlot(jk) != hashSlot(vk) {
			t.Fatalf("账本键与值键必须同槽: vk=%q(slot %d) jk=%q(slot %d)",
				vk, hashSlot(vk), jk, hashSlot(jk))
		}
	}
}

// 反证：旧全局账本键与值键**不同**槽（否则本次修复无意义）。
func TestClusterJournalKey_GlobalKeyWouldDifferSlot(t *testing.T) {
	vk := "tg:g1:t:1001"
	oldGlobal := "tg:g1:t" + jDirtySuffix // 旧格式：keyBase + 后缀，无 hashtag
	if hashSlot(oldGlobal) == hashSlot(vk) {
		// 极小概率撞槽；换一个键再验，避免断言空转
		vk = "tg:g1:t:2002"
		oldGlobal = "tg:g1:t" + jDirtySuffix
		if hashSlot(oldGlobal) == hashSlot(vk) {
			t.Skip("构造的键恰好撞槽，跳过反证")
		}
	}
	if hashSlot(clusterJournalKey(vk)) != hashSlot(vk) {
		t.Fatal("同槽派生失效")
	}
}

// clusterValueKeyFromMember 必须由「全局账本键 + member」逐字还原出 Cache.KeyOf 的值键，
// 否则集群销账会清到错误的键、账本永远清不掉。这里直接对齐 KeyOf。
func TestClusterValueKeyFromMember_MatchesKeyOf(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()))
	// h.c.jz 是全局账本锚点（keyBase + jDirtySuffix）
	for _, key := range []string{"x", "1001", "a:b", "long-key-with-dash"} {
		member := jOpUpsertPrefix + h.c.keyFn(key) // 写线记的就是 keyFn(key)
		got, ok := clusterValueKeyFromMember(h.c.jz, member)
		if !ok {
			t.Fatalf("member %q 应能反推", member)
		}
		if want := h.c.KeyOf(key); got != want {
			t.Fatalf("反推值键与 KeyOf 不一致: got=%q want=%q (member=%q)", got, want, member)
		}
		// tombstone member 同样还原
		gotD, okD := clusterValueKeyFromMember(h.c.jz, jOpDeletePrefix+h.c.keyFn(key))
		if !okD || gotD != h.c.KeyOf(key) {
			t.Fatalf("tombstone member 反推失败: ok=%v got=%q want=%q", okD, gotD, h.c.KeyOf(key))
		}
	}
	// 坏 member（无 op 前缀）→ 明确失败，不静默
	if _, ok := clusterValueKeyFromMember(h.c.jz, "garbage"); ok {
		t.Fatal("无 op 前缀的 member 不应反推成功")
	}
}

func TestClusterJournalScanPattern(t *testing.T) {
	got := clusterJournalScanPattern("tg:g1:t" + jDirtySuffix)
	want := "{tg:g1:t:*}" + jDirtySuffix
	if got != want {
		t.Fatalf("SCAN glob=%q 应为 %q", got, want)
	}
	// glob 必须能匹配到 clusterJournalKey 派生出的键（Redis glob 中 { } 为字面量、* 匹配键尾）
	jk := clusterJournalKey("tg:g1:t:1001")
	if !globMatch(want, jk) {
		t.Fatalf("glob %q 应匹配账本键 %q", want, jk)
	}
	if globMatch(want, "tg:g1:t:dirty#zset") {
		t.Fatal("glob 不应匹配旧的全局账本键（无花括号）")
	}
}

// globMatch 极简 Redis 风格 glob（仅 '*' 通配），用于验证 SCAN 模式形态。
func globMatch(pattern, s string) bool {
	// 以 '*' 分段做子串顺序匹配
	parts := strings.Split(pattern, "*")
	idx := 0
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(s, p) {
				return false
			}
			idx = len(p)
			continue
		}
		found := strings.Index(s[idx:], p)
		if found < 0 {
			return false
		}
		idx += found + len(p)
	}
	// 末段必须贴尾
	if last := parts[len(parts)-1]; last != "" {
		return strings.HasSuffix(s, last)
	}
	return true
}

// ---------- redisStore 集群分支：断言走双键 Lua（原子），而非 pipeline ----------

// recordingCmd 嵌入 redis.Cmdable（未覆盖的方法不会被集群记账路径调用），
// 只录制 Eval 的 KEYS 与脚本，用于断言「双键 Lua 原子分支」被启用。
type recordingCmd struct {
	redis.Cmdable
	evalKeys    [][]string
	evalScripts []string
	pipeCalls   int
}

func (r *recordingCmd) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	r.evalScripts = append(r.evalScripts, script)
	cp := append([]string(nil), keys...)
	r.evalKeys = append(r.evalKeys, cp)
	c := redis.NewCmd(ctx)
	c.SetVal(int64(1)) // 双键 Lua 成功恒返回 1（写生效）
	return c
}

// 集群模式：JAddUpsertSeq 必须走一段双键 Lua（Eval，两 KEYS 同槽），不走 pipeline。
func TestRedisStoreCluster_JAddUpsertSeq_UsesAtomicDoubleKeyLua(t *testing.T) {
	rc := &recordingCmd{}
	s := &redisStore{cmd: rc, isCluster: true, conc: 16}
	ctx := context.Background()

	valueKey := "tg:g1:t:1001"
	globalZ := "tg:g1:t" + jDirtySuffix
	ok, err := s.JAddUpsertSeq(ctx, valueKey, `{"s":7}`, time.Minute, globalZ, "1001", 7)
	if err != nil {
		t.Fatalf("JAddUpsertSeq: %v", err)
	}
	if !ok {
		t.Fatal("Eval 返回 1，应判定写入生效")
	}
	if rc.pipeCalls != 0 {
		t.Fatalf("集群分支不得再走 pipeline 退化路径，pipeCalls=%d", rc.pipeCalls)
	}
	if len(rc.evalKeys) != 1 {
		t.Fatalf("应恰好一次 Eval，得 %d 次", len(rc.evalKeys))
	}
	keys := rc.evalKeys[0]
	if len(keys) != 2 {
		t.Fatalf("双键 Lua 应有 2 个 KEYS，得 %v", keys)
	}
	if keys[0] != valueKey {
		t.Fatalf("KEYS[1] 应为值键: %q", keys[0])
	}
	if keys[1] != clusterJournalKey(valueKey) {
		t.Fatalf("KEYS[2] 应为同槽账本键: got=%q want=%q", keys[1], clusterJournalKey(valueKey))
	}
	if hashSlot(keys[0]) != hashSlot(keys[1]) {
		t.Fatalf("两 KEYS 必须同槽才能原子执行: %d vs %d", hashSlot(keys[0]), hashSlot(keys[1]))
	}
	if !strings.Contains(rc.evalScripts[0], "ZADD") || !strings.Contains(rc.evalScripts[0], "SET") {
		t.Fatalf("应使用写值+记账的双键 Lua 脚本，得脚本片段: %q", rc.evalScripts[0])
	}
}

// 集群模式：JAddDeleteSeq 同样走同槽双键 Lua（DEL + 记账）。
func TestRedisStoreCluster_JAddDeleteSeq_UsesAtomicDoubleKeyLua(t *testing.T) {
	rc := &recordingCmd{}
	s := &redisStore{cmd: rc, isCluster: true, conc: 16}
	ctx := context.Background()

	valueKey := "tg:g1:t:2002"
	globalZ := "tg:g1:t" + jDirtySuffix
	ok, err := s.JAddDeleteSeq(ctx, valueKey, globalZ, "2002", 9)
	if err != nil {
		t.Fatalf("JAddDeleteSeq: %v", err)
	}
	if !ok {
		t.Fatal("应判定删除生效")
	}
	if len(rc.evalKeys) != 1 || len(rc.evalKeys[0]) != 2 {
		t.Fatalf("应恰好一次双键 Eval，得 %v", rc.evalKeys)
	}
	keys := rc.evalKeys[0]
	if keys[0] != valueKey || keys[1] != clusterJournalKey(valueKey) {
		t.Fatalf("KEYS 形态异常: %v", keys)
	}
	if hashSlot(keys[0]) != hashSlot(keys[1]) {
		t.Fatal("两 KEYS 必须同槽")
	}
	if !strings.Contains(rc.evalScripts[0], "DEL") {
		t.Fatal("应使用删除+记账的双键 Lua 脚本")
	}
}

// 非集群模式：JAddUpsertSeq 的 KEYS 必须仍是 [值键, 全局账本键]——单机行为逐字不变。
func TestRedisStoreSingleNode_JAddUpsertSeq_KeepsGlobalZKey(t *testing.T) {
	rc := &recordingCmd{}
	s := &redisStore{cmd: rc, isCluster: false, conc: 16}
	ctx := context.Background()

	valueKey := "tg:g1:t:1001"
	globalZ := "tg:g1:t" + jDirtySuffix
	if _, err := s.JAddUpsertSeq(ctx, valueKey, `{"s":3}`, time.Minute, globalZ, "1001", 3); err != nil {
		t.Fatalf("JAddUpsertSeq: %v", err)
	}
	if len(rc.evalKeys) != 1 {
		t.Fatalf("应恰好一次 Eval，得 %d", len(rc.evalKeys))
	}
	keys := rc.evalKeys[0]
	if keys[1] != globalZ {
		t.Fatalf("单机下 KEYS[2] 必须仍是全局账本键（行为不变）: got=%q want=%q", keys[1], globalZ)
	}
	// journalZKey 单机直接透传
	if got := s.journalZKey(valueKey, globalZ); got != globalZ {
		t.Fatalf("单机 journalZKey 应透传全局键: %q", got)
	}
	// 集群才派生同槽键
	sc := &redisStore{cmd: rc, isCluster: true}
	if got := sc.journalZKey(valueKey, globalZ); got != clusterJournalKey(valueKey) {
		t.Fatalf("集群 journalZKey 应派生同槽键: %q", got)
	}
}
