// Package ledger 是链上出款通道的防双付台账：同一个单号绝不能对应两笔链上交易。
//
// 它从 tron 包抽出（原 tron/idempotent.go）是因为本仓现在有多条会自己动公链的通道
// （TRON/USDT 与 TON），而这件事的正确性与是哪条链无关：先记台账再动钱、终局之后
// 不再动笔。链相关的只有「这一笔的原始交易长什么样」，那留在各通道包里。
//
// 键必须有链维度：只按 order_no 建键的话，一张 TON 单会撞见同单号 TRON 单的结论，
// 那条结论会把「TON 这单从没发过」读成「已经在链上 settle 了」——反过来也一样。
// 各通道持有一个自己的 Ledger 实例（New(chain)），跨链因此天然隔离；Journal 的持久
// 实现里同样带 chain 一列，免得下游只建一张按单号唯一索引的表就把两条链挤成一行。
package ledger

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// State 是台账条目所处阶段。
type State string

const (
	// StatePrepared：已组好并签名，还没交给节点。此刻钱一定没动，同单号可以整笔替换重发。
	StatePrepared State = "prepared"
	// StateBroadcast：已交给节点（不论结论读回没有）。此刻不得重签，只能按 txid 查证。
	StateBroadcast State = "broadcast"
	// StateSettled：链上已有终局（成功、失败或过期落块）。再动一次就是双付或改写既成事实。
	StateSettled State = "settled"
	// StateRejected：节点在入池前就拒了（签名、过期、超限一类）。钱一定没动，
	// 因此它不是终局：同单号允许重建一笔新的再发，但重建前这一笔不会再被广播。
	StateRejected State = "rejected"
)

// Entry 是「这一单号对应哪笔链上交易」的记录。
//
// 字段都是定位信息（链、单号、txid、收端、合约、金额、上限），不含凭证。RawTx 是已签名
// 交易，可以持久化、也足够重广播，但反推不出私钥；它同样不许进日志与 RawNote。
type Entry struct {
	Chain    string
	OrderNo  string
	TxID     string
	RawTx    []byte
	To       string
	Contract string
	Amount   int64
	FeeLimit int64
	State    State
	// ExpiresAtMS 是这笔交易的链上有效期（毫秒）。过了它就不可能再上链，
	// StateBroadcast 才有理由降级为失败而不是永远挂着。
	ExpiresAtMS int64
	UpdatedAtMS int64
}

// Terminal 该条是否已终局。
func (e *Entry) Terminal() bool { return e != nil && e.State == StateSettled }

// Expired 这笔未上链的交易是否已过链上有效期（只在没进块时才有意义）。
func (e *Entry) Expired(now time.Time) bool {
	return e != nil && e.ExpiresAtMS > 0 && now.UnixMilli() >= e.ExpiresAtMS
}

// Journal 是出款台账的存储契约，供下游注入持久实现。
//
// 只有 Put/Get 两个动作、键只有（链、单号）两列是刻意的：幂等要求「按单号能查到那一笔」，
// 查不到就等于允许再写一笔。同链同单号后写覆盖前写。
type Journal interface {
	Put(entry *Entry) error
	Get(chain, orderNo string) (*Entry, bool)
}

// ErrAlreadySettled 这一单号已终局，再动就是双付。
var ErrAlreadySettled = errors.New("该单号在链上已有终局，不得再次出资")

// ErrJournalUnavailable 台账写不进去：此时必须放弃广播，不能「记不上但照发」。
var ErrJournalUnavailable = errors.New("出款台账不可写，已放弃广播")

// memJournal 是进程内默认存储。
//
// 它是「默认能用」而不是「默认够用」：重启即丢，之后同单号能否安全取决于链上反查
// （各通道包的 lookupLedgerByChain）。要跨重启保住幂等，下游必须 SetJournal 注入持久实现。
type memJournal struct {
	mu   sync.RWMutex
	data map[string]*Entry
}

func newMemJournal() *memJournal { return &memJournal{data: map[string]*Entry{}} }

// journalKey 把两个维度拼成一个键。用 \x00 而不是 ':'：单号里可能带冒号，
// 拼出来的键会串位，那时「查得到」与「查的是另一条链同一单号」就分不出来了。
func journalKey(chain, orderNo string) string { return chain + "\x00" + orderNo }

func (m *memJournal) Put(entry *Entry) error {
	if entry == nil || entry.OrderNo == "" {
		return errors.New("出款台账：条目缺少单号")
	}
	cp := *entry
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[journalKey(entry.Chain, entry.OrderNo)] = &cp
	return nil
}

func (m *memJournal) Get(chain, orderNo string) (*Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.data[journalKey(chain, orderNo)]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// Ledger 是某一条链的台账门面：存储可替换，本进程内「按单号串行」不可替换。
//
// 分段锁存在的理由是并发而不是持久化：同单号的两个调用若各自先查台账再组笔，
// 两边都会看到「没记过」，于是各签一笔——那是双付最短的一条路。
type Ledger struct {
	chain    string
	mu       sync.RWMutex
	store    Journal
	flightMu sync.Mutex
	flights  map[string]*sync.Mutex
}

// New 建一条链的台账。chain 是定位标识（tron / ton），同时是存储键的第一维。
func New(chain string) *Ledger {
	return &Ledger{chain: chain, store: newMemJournal(), flights: map[string]*sync.Mutex{}}
}

// Chain 返回这条台账归属的链标识。
func (l *Ledger) Chain() string { return l.chain }

// SetJournal 替换台账的存储，返回被换下的那一个；传 nil 即换回进程内实现。
//
// 只换存储、不换分段锁：后者管的是本进程内的并发正确性，下游的库表实现既看不到也不该被要求实现。
func (l *Ledger) SetJournal(j Journal) Journal {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.store
	if j == nil {
		l.store = newMemJournal()
		return old
	}
	l.store = j
	return old
}

// Get 按单号取这一条链上的台账条目。
func (l *Ledger) Get(orderNo string) (*Entry, bool) {
	l.mu.RLock()
	store := l.store
	l.mu.RUnlock()
	return store.Get(l.chain, orderNo)
}

// Put 落台账。链标识由台账自己盖（不让调用方漏盖出一个跨链可见的条目），
// 写失败被包装成 ErrJournalUnavailable，调用方据此放弃动钱。
func (l *Ledger) Put(entry *Entry) error {
	if entry == nil || entry.OrderNo == "" {
		return ErrJournalUnavailable
	}
	stamped := *entry
	stamped.Chain = l.chain
	l.mu.RLock()
	store := l.store
	l.mu.RUnlock()
	if err := store.Put(&stamped); err != nil {
		return fmt.Errorf("%w: %v", ErrJournalUnavailable, err)
	}
	return nil
}

// WithLock 在同一单号上串行执行 fn。
//
// fn 里必须自己先看台账再看网关：锁只保证「同时只有一个人在处理这个单号」，
// 不保证「进来时上一条已被处理完」——那是台账与链上查证要回答的。
func (l *Ledger) WithLock(orderNo string, fn func() error) error {
	l.flightMu.Lock()
	lock, ok := l.flights[orderNo]
	if !ok {
		lock = &sync.Mutex{}
		l.flights[orderNo] = lock
	}
	l.flightMu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// NowMS 统一台账与链上有效期字段的时间口径（毫秒时间戳）。
func NowMS(t time.Time) int64 { return t.UnixMilli() }
