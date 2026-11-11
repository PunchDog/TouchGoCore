package dbcache

// ============================================================================
// Lua 原子写脚本：Redis 权威写路径的生产实现。
//
// 与 semantics.go 的 applyWriteOp 逐条同构（唯一契约锚）；改一侧必须同步另一侧，
// TestRedisParity* 差分对拍兜底。KEYS 固定布局见 writeKeys.redisKeys：
//
//	[1]row [2]tomb [3]dirty [4]retry [5]keySet [6]secret [7]seq
//	[8]dirtyDel [9]uq [10]lock [11]inflight [12..]ix（与 op.idx 同序）
//
// ARGV[1]=writeOp JSON，ARGV[2]=txid（""=不校验锁，P1 单 op 恒 ""；P2 事务/单 op
// 行锁启用后非空，脚本校验归属并在成功后释放）。
//
// 精度纪律：累加用 HINCRBY（原生 int64）；等值/比较 num 走 tonumber（double 域，
// <2^53 安全，资金量级满足）；str/time 走规范化串比较（时间定宽 UTC，字典序=时间序）。
// ============================================================================

import (
	"context"
	"encoding/json"
	"fmt"
)

// opLogic 是单 op 的纯行逻辑（不含锁校验/释放），被 writeScript 与 txScript 共用。
// 入参 K 是该 op 的键块（K[1..11+#idx] 同序），op 是已解码的 writeOp 表。
// 改这里必须同步 semantics.go 的 applyOne；TestRedisParity* 差分对拍兜底。
const opLogic = `
local function applyOp(K, op)
local function toMap(h)
  local m = {}
  for i = 1, #h, 2 do m[h[i]] = h[i+1] end
  return m
end
local function copyMap(m)
  local out = {}
  for k, v in pairs(m) do out[k] = v end
  return out
end
local function rowVer(m)
  return tonumber(m['__ver'] or '0') or 0
end
local function guardField(cols, m)
  local parts = {}
  for _, col in ipairs(cols) do
    local v = m[col]
    if not v or v == '' then return '' end
    parts[#parts+1] = tostring(#col) .. ':' .. col .. tostring(#v) .. ':' .. v
  end
  return table.concat(parts)
end
local function matchVal(v, val, typ)
  if typ == 'num' then
    local a, b = tonumber(v), tonumber(val)
    return a ~= nil and b ~= nil and a == b
  end
  return v == val
end
local function cmpVal(v, val, typ, o)
  if typ == 'num' then
    local a, b = tonumber(v), tonumber(val)
    if a == nil or b == nil then return false end
    if o == '=' then return a == b end
    if o == '<>' then return a ~= b end
    if o == '>' then return a > b end
    if o == '>=' then return a >= b end
    if o == '<' then return a < b end
    if o == '<=' then return a <= b end
    return false
  end
  if o == '=' then return v == val end
  if o == '<>' then return v ~= val end
  if o == '>' then return v > val end
  if o == '>=' then return v >= val end
  if o == '<' then return v < val end
  if o == '<=' then return v <= val end
  return false
end
local function likeMatch(s, pat)
  s = string.lower(s)
  pat = string.lower(pat)
  local out = {}
  for i = 1, #pat do
    local ch = pat:sub(i, i)
    if ch == '%' then out[#out+1] = '.*'
    elseif ch == '_' then out[#out+1] = '.'
    elseif ch:match('[%(%)%.%%%+%-%*%?%[%]%^%$]') then out[#out+1] = '%' .. ch
    else out[#out+1] = ch end
  end
  return string.find(s, '^' .. table.concat(out) .. '$') ~= nil
end
local function evalCond(old, c)
  local v = old[c.col]
  local missing = (v == nil or v == '')
  if c.op == 'IS NULL' then return missing end
  if c.op == 'IS NOT NULL' then return not missing end
  if missing then return false end
  if c.op == 'IN' then
    for _, val in ipairs(c.vals or {}) do
      if matchVal(v, val, c.type) then return true end
    end
    return false
  end
  if c.op == 'LIKE' then return likeMatch(v, c.val or '') end
  return cmpVal(v, c.val or '', c.type, c.op)
end
local function evalConds(old, conds, anyOf)
  for _, c in ipairs(conds or {}) do
    if not evalCond(old, c) then return false end
  end
  for _, group in ipairs(anyOf or {}) do
    local hit = false
    for _, c in ipairs(group) do
      if evalCond(old, c) then hit = true; break end
    end
    if not hit then return false end
  end
  return true
end
local function evalExpectAll(old, expect)
  if not expect then return true end
  for col, e in pairs(expect) do
    local v = old[col]
    local missing = (v == nil or v == '')
    if e.mode == 'null' then
      if not missing then return false end
    elseif e.mode == 'in' then
      if missing then return false end
      local hit = false
      for _, val in ipairs(e.vals or {}) do
        if matchVal(v, val, e.type) then hit = true; break end
      end
      if not hit then return false end
    elseif e.mode == 'eq' then
      if missing or not matchVal(v, e.val, e.type) then return false end
    else
      return false
    end
  end
  return true
end
local function maintainIndexes(old, newM)
  for i, ix in ipairs(op.idx or {}) do
    local key = K[11 + i]
    local oldVal = old[ix.col]
    local newVal = newM[ix.col]
    if ix.kind == 'num' then
      if oldVal and oldVal ~= '' then redis.call('ZREM', key, op.key) end
      if newVal and newVal ~= '' then
        local n = tonumber(newVal)
        if n then redis.call('ZADD', key, n, op.key) end
      end
    else
      if oldVal and oldVal ~= '' then redis.call('ZREM', key, oldVal .. '\0' .. op.key) end
      if newVal and newVal ~= '' then redis.call('ZADD', key, 0, newVal .. '\0' .. op.key) end
    end
  end
end
local function guardStale(cur)
  -- 守卫可能比行活得更久（裸 SQL 删行不清 Redis 守卫）：行 hash 不存在且墓碑
  -- 不在存活期时视为陈旧，放行并由 applyRow 覆盖，避免同键重建被永久占住。
  local rowPrefix = string.sub(K[1], 1, #K[1] - #op.key)
  local tombPrefix = string.sub(K[2], 1, #K[2] - #op.key)
  if redis.call('EXISTS', rowPrefix .. cur) == 1 then return false end
  if redis.call('EXISTS', tombPrefix .. cur) == 1 then return false end
  return true
end
local function checkGuards(old, newM, insert)
  for _, group in ipairs(op.uq or {}) do
    local f = guardField(group, newM)
    if f ~= '' then
      local cur = redis.call('HGET', K[9], f)
      if cur and cur ~= '' and not guardStale(cur) then
        if insert then return {affected = 0} end
        if cur ~= op.key then return {err = 'duplicate'} end
      end
    end
  end
  return nil
end
local function applyRow(old, newM)
  local ver = rowVer(old) + 1
  newM['__ver'] = tostring(ver)
  redis.call('DEL', K[1])
  local flat = {}
  for k, v in pairs(newM) do flat[#flat+1] = k; flat[#flat+1] = v end
  if #flat > 0 then redis.call('HSET', K[1], unpack(flat)) end
  redis.call('DEL', K[2])
  redis.call('SADD', K[5], op.key)
  if op.secret then
    local sf = {}
    for k, v in pairs(op.secret) do sf[#sf+1] = k; sf[#sf+1] = v end
    if #sf > 0 then redis.call('HSET', K[6], unpack(sf)) end
  end
  maintainIndexes(old, newM)
  for _, group in ipairs(op.uq or {}) do
    local oldF = guardField(group, old)
    local newF = guardField(group, newM)
    if newF == '' then
      if oldF ~= '' then redis.call('HDEL', K[9], oldF) end
    else
      if oldF ~= '' and oldF ~= newF then redis.call('HDEL', K[9], oldF) end
      redis.call('HSET', K[9], newF, op.key)
    end
  end
  redis.call('ZADD', K[3], op.due, op.key)
  return {affected = 1, ver = ver, row = newM}
end

local old = toMap(redis.call('HGETALL', K[1]))
local res
if op.op == 'save' then
  if op.nx and (next(old) or redis.call('EXISTS', K[2]) == 1) then
    res = {affected = 0, ver = rowVer(old), row = old}
  else
    local newM = copyMap(op.fields or {})
    local g = checkGuards(old, newM, false)
    if g then res = g else res = applyRow(old, newM) end
  end
elseif op.op == 'insert' then
  if next(old) then
    res = {affected = 0, ver = rowVer(old), row = old}
  else
    local newM = copyMap(op.fields or {})
    local g = checkGuards(old, newM, true)
    if g then res = g else res = applyRow(old, newM) end
  end
elseif op.op == 'increment' then
  if not next(old) or not evalConds(old, op.conds, op.anyOf) then
    res = {affected = 0}
  else
    redis.call('HINCRBY', K[1], op.column, op.delta or 0)
    redis.call('HINCRBY', K[1], '__ver', 1)
    local newM = toMap(redis.call('HGETALL', K[1]))
    maintainIndexes(old, newM)
    redis.call('ZADD', K[3], op.due, op.key)
    res = {affected = 1, ver = rowVer(newM), row = newM}
  end
elseif op.op == 'max' then
  if not next(old) or not evalConds(old, op.conds, op.anyOf) then
    res = {affected = 0}
  else
    local cur = old[op.column] or ''
    local cand = op.max or ''
    local cn, dn = tonumber(cur), tonumber(cand)
    local pick = cur
    if cn == nil then pick = cand
    elseif dn ~= nil and dn > cn then pick = cand end
    redis.call('HSET', K[1], op.column, pick)
    redis.call('HINCRBY', K[1], '__ver', 1)
    local newM = toMap(redis.call('HGETALL', K[1]))
    maintainIndexes(old, newM)
    redis.call('ZADD', K[3], op.due, op.key)
    res = {affected = 1, ver = rowVer(newM), row = newM}
  end
elseif op.op == 'cas' then
  if not next(old) or not evalExpectAll(old, op.expect) or not evalConds(old, op.conds, op.anyOf) then
    res = {affected = 0}
  else
    local newM = copyMap(old)
    for c, v in pairs(op.set or {}) do newM[c] = v end
    local g = checkGuards(old, newM, false)
    if g then res = g else res = applyRow(old, newM) end
  end
elseif op.op == 'delete' then
  local existed = next(old) ~= nil
  if not op.noTomb then
    if op.tombTTL and op.tombTTL > 0 then
      redis.call('SET', K[2], '1', 'PX', op.tombTTL)
    else
      redis.call('SET', K[2], '1')
    end
  end
  for i, ix in ipairs(op.idx or {}) do
    local key = K[11 + i]
    local v = old[ix.col]
    if v and v ~= '' then
      if ix.kind == 'num' then redis.call('ZREM', key, op.key)
      else redis.call('ZREM', key, v .. '\0' .. op.key) end
    end
  end
  for _, group in ipairs(op.uq or {}) do
    local f = guardField(group, old)
    if f ~= '' then redis.call('HDEL', K[9], f) end
  end
  redis.call('SREM', K[5], op.key)
  redis.call('DEL', K[1])
  redis.call('DEL', K[6])
  redis.call('ZREM', K[3], op.key)
  redis.call('HDEL', K[4], op.key)
  redis.call('ZREM', K[11], op.key)
  if op.sink and existed then redis.call('ZADD', K[8], op.due, op.key) end
  res = {affected = existed and 1 or 0}
else
  res = {err = 'bad_op'}
end
return res
end
`

// writeScript 单 op 权威写：可选锁校验（txid）→ applyOp → 成功释放锁。
const writeScript = opLogic + `
local op = cjson.decode(ARGV[1])
local txid = ARGV[2]
if txid ~= '' then
  if redis.call('GET', KEYS[10]) ~= txid then return cjson.encode({err = 'lock_lost'}) end
end
local res = applyOp(KEYS, op)
if res.err == nil and txid ~= '' then redis.call('DEL', KEYS[10]) end
return cjson.encode(res)
`

// txScript 多 op 原子批：ARGV[1]={txid,ops:[writeOp...]}，KEYS 是各 op 键块按序
// 拼接（每块 11+#idx 个，与 writeKeys.redisKeys 同序）。先校验全部行锁归属，
// 再逐 op 复用 applyOp（每 op 重新 HGETALL，同行多 op 自然链式读己写）；任一 op
// 返回 err 立即中止。调用方（txState）已在 Go 侧对 overlay 预校验过条件/守卫并持锁，
// 正常情况下批内不会触发 err；lock_lost 在任何写之前检出，保证零副作用。
const txScript = opLogic + `
local p = cjson.decode(ARGV[1])
local txid = p.txid or ''
local off = 1
local blocks = {}
for i, op in ipairs(p.ops) do
  local n = 11 + #(op.idx or {})
  local K = {}
  for j = 1, n do K[j] = KEYS[off + j - 1] end
  off = off + n
  blocks[i] = K
  if txid ~= '' then
    if redis.call('GET', K[10]) ~= txid then return cjson.encode({err = 'lock_lost', index = i}) end
  end
end
local results = {}
for i, op in ipairs(p.ops) do
  local res = applyOp(blocks[i], op)
  if res.err then return cjson.encode({err = res.err, index = i}) end
  results[i] = res
end
if txid ~= '' then
  for _, K in ipairs(blocks) do redis.call('DEL', K[10]) end
end
return cjson.encode({results = results})
`

// applyWrite 把一次写交给 kvStore 原子执行并解析结果。P1 单 op 不取行锁
// （txid=""）；P2 启用 TxRowLocks 后单 op 与事务批共用锁协议。
func (e *Engine) applyWrite(ctx context.Context, b *binding, key string, op *writeOp) (*writeResult, error) {
	op.Key = key
	op.Idx = b.idxDescs()
	op.UQ = b.uniques
	k := b.writeKeysFor(key)
	payload, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("dbcache: marshal write op: %w", err)
	}
	raw, err := e.store.ApplyWrite(ctx, k.redisKeys(), string(payload), "")
	if err != nil {
		return nil, fmt.Errorf("dbcache: write %s/%s: %w", b.table, key, err)
	}
	var res writeResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, fmt.Errorf("dbcache: decode write result: %w", err)
	}
	switch res.Err {
	case "":
		return &res, nil
	case "duplicate":
		return nil, fmt.Errorf("%w: %s/%s", ErrDuplicateKey, b.table, key)
	case "lock_lost":
		return nil, fmt.Errorf("%w: %s/%s lock lost", ErrTxConflict, b.table, key)
	default:
		return nil, fmt.Errorf("dbcache: write %s/%s: %s", b.table, key, res.Err)
	}
}

// txWrite 是事务批里的一条写：绑定 + 行键 + op。
type txWrite struct {
	b   *binding
	key string
	op  *writeOp
}

// applyTx 把一批写交给 kvStore 原子执行（txScript）。txid 非空时脚本校验并释放
// 行锁（锁键 = lockPrefix+key，调用方须先 TryLock 取得）。返回逐 op 结果。
func (e *Engine) applyTx(ctx context.Context, txid string, writes []txWrite) ([]*writeResult, error) {
	payload := txPayload{Txid: txid, Ops: make([]*writeOp, len(writes))}
	var keys []string
	for i, w := range writes {
		w.op.Key = w.key
		w.op.Idx = w.b.idxDescs()
		w.op.UQ = w.b.uniques
		k := w.b.writeKeysFor(w.key)
		k.lock = w.b.keys.lockPrefix + w.key
		payload.Ops[i] = w.op
		keys = append(keys, k.redisKeys()...)
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("dbcache: marshal tx payload: %w", err)
	}
	raw, err := e.store.ApplyTx(ctx, keys, string(rawPayload))
	if err != nil {
		return nil, fmt.Errorf("dbcache: tx apply: %w", err)
	}
	var res txResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, fmt.Errorf("dbcache: decode tx result: %w", err)
	}
	switch res.Err {
	case "":
		return res.Results, nil
	case "duplicate":
		return nil, fmt.Errorf("%w: tx op %d", ErrDuplicateKey, res.Index)
	case "lock_lost":
		return nil, fmt.Errorf("%w: tx lock lost", ErrTxConflict)
	default:
		return nil, fmt.Errorf("dbcache: tx apply: %s (op %d)", res.Err, res.Index)
	}
}
