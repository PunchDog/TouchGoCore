package tron

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// 本文件只管一件事：同一个 order_no 绝不能对应两笔链上交易。
//
// 供应商链路里 order_no 是幂等键、由对方保证；直接上链后没有别人替我们保证——
// TRON 用 refBlock 防重放而不是账户 nonce，同一单号重新组一笔就是一笔全新的钱。
// 所以幂等必须在本地做实：先记台账再动钱，且终局之后不再动笔。

// entryState 是台账条目所处阶段。
type entryState string

const (
	// statePrepared：已组好并签名，还没交给节点。此刻钱一定没动，同单号可以整笔替换重发。
	statePrepared entryState = "prepared"
	// stateBroadcast：已交给节点（不论结论读回没有）。此刻不得重签，只能按 txid 查证。
	stateBroadcast entryState = "broadcast"
	// stateSettled：链上已有终局（成功、失败或过期落块）。再动一次就是双付或改写既成事实。
	stateSettled entryState = "settled"
	// stateRejected：节点在入池前就拒了（签名、TAPOS、过期、超限）。钱一定没动，
	// 因此它不是终局：同单号允许重建一笔新的再发，但重建前这一笔不会再被广播。
	stateRejected entryState = "rejected"
)

// ledgerEntry 是「这一单号对应哪笔链上交易」的记录。
//
// 字段都是定位信息（单号、txid、收端、合约、金额、上限），不含凭证。RawTx 是已签名交易，
// 可以持久化、也足够重广播，但反推不出私钥；它同样不许进日志与 RawNote。
type ledgerEntry struct {
	OrderNo  string
	TxID     string
	RawTx    []byte
	To       string
	Contract string
	Amount   int64
	FeeLimit int64
	State    entryState
	// ExpiresAtMS 是这笔交易的链上有效期（毫秒）。过了它就不可能再上链，
	// stateBroadcast 才有理由降级为失败而不是永远挂着。
	ExpiresAtMS int64
	UpdatedAtMS int64
}

// terminal 该条是否已终局。
func (e *ledgerEntry) terminal() bool { return e != nil && e.State == stateSettled }

// expired 这笔未上链的交易是否已过链上有效期（只在没进块时才有意义）。
func (e *ledgerEntry) expired(now time.Time) bool {
	return e != nil && e.ExpiresAtMS > 0 && now.UnixMilli() >= e.ExpiresAtMS
}

// Journal 是出款台账的存储契约，供下游注入持久实现。
//
// 只有 Put/Get 两个动作、键只有 order_no 一列是刻意的：幂等要求「按单号能查到那一笔」，
// 查不到就等于允许再写一笔。同单号后写覆盖前写。
type Journal interface {
	Put(entry *ledgerEntry) error
	Get(orderNo string) (*ledgerEntry, bool)
}

// ErrOrderAlreadySettled 这一单号已终局，再动就是双付。
var ErrOrderAlreadySettled = errors.New("该单号在链上已有终局，不得再次出资")

// ErrJournalUnavailable 台账写不进去：此时必须放弃广播，不能「记不上但照发」。
var ErrJournalUnavailable = errors.New("出款台账不可写，已放弃广播")

// memJournal 是进程内默认存储。
//
// 它是「默认能用」而不是「默认够用」：重启即丢，之后同单号能否安全取决于链上反查
// （见 onchain.go 的 lookupOnChain）。要跨重启保住幂等，下游必须 SetJournal 注入持久实现。
type memJournal struct {
	mu   sync.RWMutex
	data map[string]*ledgerEntry
}

func newMemJournal() *memJournal { return &memJournal{data: map[string]*ledgerEntry{}} }

func (m *memJournal) Put(entry *ledgerEntry) error {
	if entry == nil || entry.OrderNo == "" {
		return errors.New("出款台账：条目缺少单号")
	}
	cp := *entry
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[entry.OrderNo] = &cp
	return nil
}

func (m *memJournal) Get(orderNo string) (*ledgerEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.data[orderNo]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// ledger 是台账门面：存储可替换，本进程内「按单号串行」不可替换。
//
// 分段锁存在的理由是并发而不是持久化：同单号的两个调用若各自先查台账再组笔，
// 两边都会看到「没记过」，于是各签一笔——那是双付最短的一条路。
type ledger struct {
	mu       sync.RWMutex
	store    Journal
	flightMu sync.Mutex
	flights  map[string]*sync.Mutex
}

func newLedger() *ledger { return &ledger{store: newMemJournal(), flights: map[string]*sync.Mutex{}} }

var defaultLedger = newLedger()

// SetJournal 替换默认台账的存储，返回被换下的那一个；传 nil 即换回进程内实现。
//
// 只换存储、不换分段锁：后者管的是本进程内的并发正确性，下游的库表实现既看不到也不该被要求实现。
func SetJournal(j Journal) Journal { return defaultLedger.set(j) }

func (l *ledger) set(j Journal) Journal {
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

func (l *ledger) get(orderNo string) (*ledgerEntry, bool) {
	l.mu.RLock()
	store := l.store
	l.mu.RUnlock()
	return store.Get(orderNo)
}

// put 落台账。写失败被包装成 ErrJournalUnavailable，调用方据此放弃动钱。
func (l *ledger) put(entry *ledgerEntry) error {
	if entry == nil || entry.OrderNo == "" {
		return ErrJournalUnavailable
	}
	l.mu.RLock()
	store := l.store
	l.mu.RUnlock()
	if err := store.Put(entry); err != nil {
		return fmt.Errorf("%w: %v", ErrJournalUnavailable, err)
	}
	return nil
}

// withLock 在同一单号上串行执行 fn。
//
// fn 里必须自己先看台账再看网关：锁只保证「同时只有一个人在处理这个单号」，
// 不保证「进来时上一条已被处理完」——那是台账与链上查证要回答的。
func (l *ledger) withLock(orderNo string, fn func() error) error {
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

// nowMS 统一台账与链上 expiration 的时间口径（毫秒时间戳）。
func nowMS(t time.Time) int64 { return t.UnixMilli() }
