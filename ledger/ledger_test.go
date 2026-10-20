package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本包唯一新增于「从 tron 抽出」之外的语义是链维度，所以这里只测那一维，
// 加上两条状态判定（Terminal/Expired）——它们是所有通道共同的承重墙。

func TestLedgersAreChainScoped(t *testing.T) {
	tron := New("tron")
	ton := New("ton")
	entry := func(state State) *Entry {
		return &Entry{OrderNo: "WD-1", TxID: "tx-x", Amount: 1, State: state}
	}
	if err := tron.Put(entry(StateSettled)); err != nil {
		t.Fatal(err)
	}
	// 链标识由台账自己盖：调用方漏盖也不许留下一条跨链可见的裸记录。
	got, _ := tron.Get("WD-1")
	if got.Chain != "tron" {
		t.Fatalf("Put 没盖链标识，实得 %q", got.Chain)
	}
	if _, found := ton.Get("WD-1"); found {
		t.Fatal("TON 侧看见了同单号的 TRON 记录：那会把「这单从没发过」读成「链上已终局」")
	}
	if err := ton.Put(entry(StatePrepared)); err != nil {
		t.Fatal(err)
	}
	a, _ := tron.Get("WD-1")
	b, _ := ton.Get("WD-1")
	if a.State == b.State || a.Chain == b.Chain {
		t.Fatalf("两条同单号记录互相覆盖了：%+v / %+v", a, b)
	}
}

// failingStore 记录下游看到的键，用来证明持久实现拿得到链维度。
type failingStore struct {
	mu     sync.Mutex
	keys   []string
	failAt int
	puts   int
}

func (f *failingStore) Put(e *Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.failAt > 0 && f.puts >= f.failAt {
		return errors.New("落库失败")
	}
	f.keys = append(f.keys, e.Chain+"/"+e.OrderNo)
	return nil
}

func (f *failingStore) Get(chain, orderNo string) (*Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k == chain+"/"+orderNo {
			return &Entry{Chain: chain, OrderNo: orderNo}, true
		}
	}
	return nil, false
}

func TestInjectedJournalSeesChainAndOrderNo(t *testing.T) {
	store := &failingStore{}
	ton := New("ton")
	if old := ton.SetJournal(store); old == nil {
		t.Error("SetJournal 应把被换下的存储交回去")
	}
	if err := ton.Put(&Entry{OrderNo: "WD-9", TxID: "h"}); err != nil {
		t.Fatal(err)
	}
	if len(store.keys) != 1 || store.keys[0] != "ton/WD-9" {
		t.Fatalf("持久实现拿到的键缺了链维度: %v", store.keys)
	}
	// 换回进程内实现后，注入期间写的东西不该还看得见（否则同一实例被两条链共用就回来了）。
	if back := ton.SetJournal(nil); back != store {
		t.Error("换回缺省存储时没把注入的那份交回去")
	}
	if _, found := ton.Get("WD-9"); found {
		t.Error("换回进程内实现后仍读得到注入期写入的条目")
	}
}

// TestPutFailureIsJournaledUnavailable 写不进存储必须翻成「台账不可用」这一可判错误：
// 调用方据此放弃动钱，而不是拿一个裸的落库错误当「没发生」。
func TestPutFailureIsJournaledUnavailable(t *testing.T) {
	ton := New("ton")
	ton.SetJournal(&failingStore{failAt: 1})
	err := ton.Put(&Entry{OrderNo: "WD-1"})
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("应包成 ErrJournalUnavailable，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "落库失败") {
		t.Errorf("存储自己的原因要跟着交出去，否则运维无从判断是库挂了还是键冲突: %v", err)
	}
	if err := ton.Put(nil); !errors.Is(err, ErrJournalUnavailable) {
		t.Errorf("空条目应同样拒掉，实得 %v", err)
	}
	if err := ton.Put(&Entry{}); !errors.Is(err, ErrJournalUnavailable) {
		t.Errorf("缺单号的条目应同样拒掉，实得 %v", err)
	}
}

func TestEntryTerminalAndExpired(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var nilEntry *Entry
	if nilEntry.Terminal() || nilEntry.Expired(now) {
		t.Error("nil 条目既不该算终局也不该算过期")
	}
	for _, cs := range []struct {
		state State
		want  bool
	}{
		{StateSettled, true}, {StatePrepared, false}, {StateBroadcast, false}, {StateRejected, false},
	} {
		if got := (&Entry{State: cs.state}).Terminal(); got != cs.want {
			t.Errorf("%s 的终局判定 = %v，期望 %v", cs.state, got, cs.want)
		}
	}
	// 没记有效期就没有「过期」可言：判过期会让一条本来还在路上的单被重下。
	if (&Entry{ExpiresAtMS: 0, State: StateBroadcast}).Expired(now) {
		t.Error("ExpiresAtMS 为 0 的条目被判过期")
	}
	if !(&Entry{ExpiresAtMS: now.UnixMilli(), State: StatePrepared}).Expired(now) {
		t.Error("到了有效期的条目没被判过期")
	}
	if (&Entry{ExpiresAtMS: now.Add(time.Minute).UnixMilli()}).Expired(now) {
		t.Error("还没到有效期的条目被判过期")
	}
}

// TestWithLockSerializesSameOrderNo 同单号必须串行：两个调用各自「先查台账再组笔」
// 时都会看到没记过，于是各签一笔——那是双付最短的一条路。
func TestWithLockSerializesSameOrderNo(t *testing.T) {
	ton := New("ton")
	var mu sync.Mutex
	inside := 0
	maxInside := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ton.WithLock("WD-1", func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("同单号的临界区并发进入 %d 次，分段锁没起作用", maxInside)
	}
	// 不同单号之间不该互斥到串行——真机并发提现时那会把吞吐钉死。
	if err := ton.WithLock("WD-2", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// TestCopyIsolatesStore 存储交出的是副本：调用方拿着返回的条目改状态，
// 不能顺手改掉台账里那一格（终局与在途只隔一个赋值）。
func TestCopyIsolatesStore(t *testing.T) {
	ton := New("ton")
	if err := ton.Put(&Entry{OrderNo: "WD-3", State: StatePrepared}); err != nil {
		t.Fatal(err)
	}
	got, _ := ton.Get("WD-3")
	got.State = StateSettled
	again, _ := ton.Get("WD-3")
	if again.State != StatePrepared {
		t.Fatalf("改了一份读回来的副本就写进了台账，实得 %s", again.State)
	}
}
