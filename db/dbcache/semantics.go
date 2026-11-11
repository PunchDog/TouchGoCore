package dbcache

// ============================================================================
// 纯 Go 参考实现：与 lua_write.go 的 writeScript 逐条同构。
//
// fakeStore 的新写路径委托这里；真 Redis 差分测试（TestRedisParity*）用同一
// 断言表对拍 Lua 与本实现的全状态 dumps，任何一侧漂移立刻红。改 Lua 必须同步
// 改这里，反之亦然——这是两套实现唯一的契约锚。
//
// 哨兵约定（与 SQL 的差异在此钉死）：缺失列与 "" 一律视为 NULL；NULL 不参与
// 唯一守卫、不建索引 entry、条件比较（除 IS NULL/IS NOT NULL）一律不命中。
// ============================================================================

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// memState 是内存版 Redis 状态：hash / zset / set / string 四类键 + 惰性过期。
type memState struct {
	hashes map[string]map[string]string
	zsets  map[string]map[string]float64
	sets   map[string]map[string]struct{}
	strs   map[string]string
	expiry map[string]int64 // 键 → 到期 UnixMilli；0 表示不过期
}

func newMemState() *memState {
	return &memState{
		hashes: make(map[string]map[string]string),
		zsets:  make(map[string]map[string]float64),
		sets:   make(map[string]map[string]struct{}),
		strs:   make(map[string]string),
		expiry: make(map[string]int64),
	}
}

// alive 判断 string 键是否存活（惰性清理过期，调用方持锁）。
func (m *memState) alive(key string, now int64) bool {
	v, ok := m.strs[key]
	if !ok || v == "" {
		return false
	}
	if exp := m.expiry[key]; exp > 0 && exp <= now {
		delete(m.strs, key)
		delete(m.expiry, key)
		return false
	}
	return true
}

func (m *memState) hash(key string) map[string]string {
	h := m.hashes[key]
	if h == nil {
		h = make(map[string]string)
		m.hashes[key] = h
	}
	return h
}

func (m *memState) zset(key string) map[string]float64 {
	z := m.zsets[key]
	if z == nil {
		z = make(map[string]float64)
		m.zsets[key] = z
	}
	return z
}

func (m *memState) set(key string) map[string]struct{} {
	s := m.sets[key]
	if s == nil {
		s = make(map[string]struct{})
		m.sets[key] = s
	}
	return s
}

// applyWriteOp 执行一次原子写（writeScript 的 Go 镜像）。txid 非空时校验锁归属。
func applyWriteOp(m *memState, k writeKeys, op *writeOp, txid string, now int64) *writeResult {
	if txid != "" {
		if !m.alive(k.lock, now) || m.strs[k.lock] != txid {
			return &writeResult{Err: "lock_lost"}
		}
	}
	old := m.hashes[k.row]
	res := applyOne(m, k, op, old, now)
	if res.Err == "" && txid != "" {
		delete(m.strs, k.lock)
		delete(m.expiry, k.lock)
	}
	return res
}

// applyTxOps 执行一批原子写（txScript 的 Go 镜像）：先校验全部行锁归属，再逐 op
// 复用 applyOne（每 op 重新读 m.hashes[row]，同行多 op 链式读己写）；任一 op err
// 立即中止返回。txid 非空且全部成功后释放全部行锁。返回逐 op 结果与首个 err。
func applyTxOps(m *memState, blocks []writeKeys, ops []*writeOp, txid string, now int64) ([]*writeResult, string) {
	if txid != "" {
		for _, k := range blocks {
			if !m.alive(k.lock, now) || m.strs[k.lock] != txid {
				return nil, "lock_lost"
			}
		}
	}
	results := make([]*writeResult, len(ops))
	for i, op := range ops {
		old := m.hashes[blocks[i].row]
		res := applyOne(m, blocks[i], op, old, now)
		if res.Err != "" {
			return nil, res.Err
		}
		results[i] = res
	}
	if txid != "" {
		for _, k := range blocks {
			delete(m.strs, k.lock)
			delete(m.expiry, k.lock)
		}
	}
	return results, ""
}

// applyOne 按 op 分派；old 为 live 旧行（可能 nil）。
func applyOne(m *memState, k writeKeys, op *writeOp, old map[string]string, now int64) *writeResult {
	switch op.Op {
	case "save":
		if op.NX && (len(old) > 0 || m.alive(k.tomb, now)) {
			return &writeResult{Affected: 0, Ver: rowVer(old), Row: copyMap(old)}
		}
		newM := copyMap(op.Fields)
		if r := checkGuards(m, k, op, old, newM, false, now); r != nil {
			return r
		}
		return applyRow(m, k, op, old, newM, now)
	case "insert":
		if len(old) > 0 {
			return &writeResult{Affected: 0, Ver: rowVer(old), Row: copyMap(old)}
		}
		newM := copyMap(op.Fields)
		if r := checkGuards(m, k, op, old, newM, true, now); r != nil {
			return r
		}
		return applyRow(m, k, op, old, newM, now)
	case "increment":
		if len(old) == 0 || !evalConds(old, op.Conds, op.AnyOf) {
			return &writeResult{Affected: 0}
		}
		cur, _ := strconv.ParseInt(old[op.Column], 10, 64)
		newRaw := strconv.FormatInt(cur+op.Delta, 10)
		newM := copyMap(old)
		newM[op.Column] = newRaw
		newM[versionField] = strconv.FormatInt(rowVer(old)+1, 10)
		m.hashes[k.row] = newM
		maintainIndexes(m, k, op, old, newM)
		m.zset(k.dirty)[op.Key] = float64(op.Due)
		return &writeResult{Affected: 1, Ver: rowVer(newM), Row: copyMap(newM)}
	case "max":
		if len(old) == 0 || !evalConds(old, op.Conds, op.AnyOf) {
			return &writeResult{Affected: 0}
		}
		cur, cand := old[op.Column], op.Max
		pick := cur
		cf, ce := strconv.ParseFloat(cur, 64)
		df, de := strconv.ParseFloat(cand, 64)
		if ce != nil {
			pick = cand
		} else if de == nil && df > cf {
			pick = cand
		}
		newM := copyMap(old)
		newM[op.Column] = pick
		newM[versionField] = strconv.FormatInt(rowVer(old)+1, 10)
		m.hashes[k.row] = newM
		maintainIndexes(m, k, op, old, newM)
		m.zset(k.dirty)[op.Key] = float64(op.Due)
		return &writeResult{Affected: 1, Ver: rowVer(newM), Row: copyMap(newM)}
	case "cas":
		if len(old) == 0 || !evalExpectAll(old, op.Expect) || !evalConds(old, op.Conds, op.AnyOf) {
			return &writeResult{Affected: 0}
		}
		newM := copyMap(old)
		for c, v := range op.Set {
			newM[c] = v
		}
		if r := checkGuards(m, k, op, old, newM, false, now); r != nil {
			return r
		}
		return applyRow(m, k, op, old, newM, now)
	case "delete":
		existed := len(old) > 0
		if !op.NoTomb {
			m.strs[k.tomb] = "1"
			if op.TombTTL > 0 {
				m.expiry[k.tomb] = now + op.TombTTL
			} else {
				delete(m.expiry, k.tomb)
			}
		}
		removeIndexes(m, k, op, old)
		uq := m.hash(k.uq)
		for _, group := range op.UQ {
			if f := guardField(group, old); f != "" {
				delete(uq, f)
			}
		}
		delete(m.set(k.keySet), op.Key)
		delete(m.hashes, k.row)
		delete(m.hashes, k.secret)
		delete(m.zset(k.dirty), op.Key)
		delete(m.hash(k.retry), op.Key)
		delete(m.zset(k.inflight), op.Key)
		if op.Sink && existed {
			m.zset(k.dirtyDel)[op.Key] = float64(op.Due)
		}
		n := int64(0)
		if existed {
			n = 1
		}
		return &writeResult{Affected: n}
	}
	return &writeResult{Err: "bad_op"}
}

// checkGuards 唯一守卫冲突检查：insert 模式任何**有效**占用都冲突（affected=0）；
// save/cas 模式他行占用才算冲突（duplicate，零副作用）。
//
// 守卫可能比行活得更久：夹具/运维用裸 SQL 删行时 Redis 守卫不会同步清。cur 指向
// 的行 hash 已不存在且墓碑不在存活期时，视为陈旧守卫——放行并由 applyRow 覆盖，
// 否则「同键重建」会被一条早已不存在的行永久占住。
func checkGuards(m *memState, k writeKeys, op *writeOp, old, newM map[string]string, insert bool, now int64) *writeResult {
	uq := m.hashes[k.uq]
	for _, group := range op.UQ {
		f := guardField(group, newM)
		if f == "" {
			continue
		}
		cur := uq[f]
		if cur == "" || guardStale(m, k, op.Key, cur, now) {
			continue
		}
		if insert {
			return &writeResult{Affected: 0}
		}
		if cur != op.Key {
			return &writeResult{Err: "duplicate"}
		}
	}
	return nil
}

// guardStale 判断守卫指向的行是否已不存在（且无存活墓碑）。
func guardStale(m *memState, k writeKeys, opKey, cur string, now int64) bool {
	if !strings.HasSuffix(k.row, opKey) || !strings.HasSuffix(k.tomb, opKey) {
		return false
	}
	rowKey := strings.TrimSuffix(k.row, opKey) + cur
	tombKey := strings.TrimSuffix(k.tomb, opKey) + cur
	return len(m.hashes[rowKey]) == 0 && !m.alive(tombKey, now)
}

// applyRow 应用整行（save/insert/cas 共用）：版本+1、清墓碑、键集、秘密、
// 索引、守卫、脏账一次完成。校验已在前置步骤通过。
func applyRow(m *memState, k writeKeys, op *writeOp, old, newM map[string]string, now int64) *writeResult {
	ver := rowVer(old) + 1
	newM[versionField] = strconv.FormatInt(ver, 10)
	m.hashes[k.row] = newM
	delete(m.strs, k.tomb)
	delete(m.expiry, k.tomb)
	m.set(k.keySet)[op.Key] = struct{}{}
	if len(op.Secret) > 0 {
		sec := m.hash(k.secret)
		for c, v := range op.Secret {
			sec[c] = v
		}
	}
	maintainIndexes(m, k, op, old, newM)
	uq := m.hash(k.uq)
	for _, group := range op.UQ {
		oldF := guardField(group, old)
		newF := guardField(group, newM)
		if newF == "" {
			if oldF != "" {
				delete(uq, oldF)
			}
			continue
		}
		if oldF != "" && oldF != newF {
			delete(uq, oldF)
		}
		uq[newF] = op.Key
	}
	m.zset(k.dirty)[op.Key] = float64(op.Due)
	return &writeResult{Affected: 1, Ver: ver, Row: copyMap(newM)}
}

// maintainIndexes 索引增删：旧值 entry 删、新值 entry 建（空值不建）。
func maintainIndexes(m *memState, k writeKeys, op *writeOp, old, newM map[string]string) {
	for i, ix := range op.Idx {
		if i >= len(k.ix) {
			break
		}
		z := m.zset(k.ix[i])
		oldVal := old[ix.Col]
		newVal := newM[ix.Col]
		if ix.Kind == "num" {
			if oldVal != "" {
				delete(z, op.Key)
			}
			if newVal != "" {
				if n, err := strconv.ParseFloat(newVal, 64); err == nil {
					z[op.Key] = n
				}
			}
		} else {
			if oldVal != "" {
				delete(z, oldVal+"\x00"+op.Key)
			}
			if newVal != "" {
				z[newVal+"\x00"+op.Key] = 0
			}
		}
	}
}

// removeIndexes 删除行时清掉全部索引 entry。
func removeIndexes(m *memState, k writeKeys, op *writeOp, old map[string]string) {
	for i, ix := range op.Idx {
		if i >= len(k.ix) {
			break
		}
		z := m.zset(k.ix[i])
		v := old[ix.Col]
		if v == "" {
			continue
		}
		if ix.Kind == "num" {
			delete(z, op.Key)
		} else {
			delete(z, v+"\x00"+op.Key)
		}
	}
}

// rowVer 读行版本（缺失=0）。
func rowVer(h map[string]string) int64 {
	n, _ := strconv.ParseInt(h[versionField], 10, 64)
	return n
}

// evalConds 全部 conds 命中且每个 anyOf 组至少一条命中。
func evalConds(old map[string]string, conds []condDesc, anyOf [][]condDesc) bool {
	for _, c := range conds {
		if !evalCond(old, c) {
			return false
		}
	}
	for _, group := range anyOf {
		hit := false
		for _, c := range group {
			if evalCond(old, c) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// evalCond 单条件对 live 行求值（Lua evalCond 镜像）。
func evalCond(old map[string]string, c condDesc) bool {
	v := old[c.Col]
	missing := v == ""
	switch c.Op {
	case "IS NULL":
		return missing
	case "IS NOT NULL":
		return !missing
	}
	if missing {
		return false
	}
	switch c.Op {
	case "IN":
		for _, val := range c.Vals {
			if matchVal(v, val, c.Type) {
				return true
			}
		}
		return false
	case "LIKE":
		return likeMatch(v, c.Val)
	case "=", "<>", ">", ">=", "<", "<=":
		return cmpVal(v, c.Val, c.Type, c.Op)
	}
	return false
}

// evalExpectAll CAS 期望全部命中。
func evalExpectAll(old map[string]string, expect map[string]expectDesc) bool {
	for col, e := range expect {
		v := old[col]
		missing := v == ""
		switch e.Mode {
		case "null":
			if !missing {
				return false
			}
		case "in":
			if missing {
				return false
			}
			hit := false
			for _, val := range e.Vals {
				if matchVal(v, val, e.Type) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		case "eq":
			if missing || !matchVal(v, e.Val, e.Type) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// matchVal 单值相等：num 走 float64（与 Lua tonumber 同精度域），str 走串相等。
func matchVal(v, val, typ string) bool {
	if typ == "num" {
		a, err1 := strconv.ParseFloat(v, 64)
		b, err2 := strconv.ParseFloat(val, 64)
		return err1 == nil && err2 == nil && a == b
	}
	return v == val
}

// cmpVal 比较运算（num 数值 / str 字典序）。
func cmpVal(v, val, typ, op string) bool {
	if typ == "num" {
		a, err1 := strconv.ParseFloat(v, 64)
		b, err2 := strconv.ParseFloat(val, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		switch op {
		case "=":
			return a == b
		case "<>":
			return a != b
		case ">":
			return a > b
		case ">=":
			return a >= b
		case "<":
			return a < b
		case "<=":
			return a <= b
		}
		return false
	}
	switch op {
	case "=":
		return v == val
	case "<>":
		return v != val
	case ">":
		return v > val
	case ">=":
		return v >= val
	case "<":
		return v < val
	case "<=":
		return v <= val
	}
	return false
}

// likeMatch SQL LIKE（%/_ 通配，ASCII 大小写不敏感）；与 Lua 侧逐字符同构转换。
func likeMatch(s, pat string) bool {
	s = strings.ToLower(s)
	pat = strings.ToLower(pat)
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		ch := pat[i]
		switch ch {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// copyMap 浅拷贝 string map（nil 安全）。
func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// dueMembers 脏账到期成员（score 升序、同分 member 升序，与 ZRANGEBYSCORE 一致）。
func (m *memState) dueMembers(key string, maxScore int64, count int) []dirtyMember {
	var out []dirtyMember
	for member, score := range m.zsets[key] {
		if int64(score) <= maxScore {
			out = append(out, dirtyMember{Member: member, Score: int64(score)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].Member < out[j].Member
	})
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out
}
