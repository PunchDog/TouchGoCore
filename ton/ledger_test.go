package ton

import (
	"sync"
	"testing"

	"touchgocore/ledger"
)

type mapJournal struct {
	mu sync.Mutex
	m  map[string]*ledger.Entry
}

func newMapJournal() *mapJournal { return &mapJournal{m: map[string]*ledger.Entry{}} }

func (j *mapJournal) Put(e *ledger.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := *e
	j.m[e.Chain+"\x00"+e.OrderNo] = &cp
	return nil
}

func (j *mapJournal) Get(chain, orderNo string) (*ledger.Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.m[chain+"\x00"+orderNo]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

func TestSetJournalSwapsStorage(t *testing.T) {
	t.Cleanup(func() { SetJournal(nil) })
	j := newMapJournal()
	if old := SetJournal(j); old == nil {
		t.Fatal("SetJournal 应返回被换下的旧存储")
	}
	entry := &ledger.Entry{OrderNo: "O-J1", Amount: 1, State: ledger.StatePrepared}
	if err := tonLedger.Put(entry); err != nil {
		t.Fatal(err)
	}
	if got, ok := j.Get("ton", "O-J1"); !ok || got.Amount != 1 {
		t.Fatalf("条目没有写进注入的 journal: %+v found=%v", got, ok)
	}
	SetJournal(nil)
	if _, ok := tonLedger.Get("O-J1"); ok {
		t.Fatal("换回进程内实现后仍读得到旧 journal 的条目")
	}
}

// TestConstructorUsesPackageLedger 生产构造必须复用包级台账；每次 Start 新建一本
// 空台账会让同进程内的在途单在重启后消失（F4）。
func TestConstructorUsesPackageLedger(t *testing.T) {
	SetJournal(nil)
	t.Cleanup(func() { SetJournal(nil) })
	ch := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch.Close() })
	if ch.led != tonLedger {
		t.Fatal("通道没有使用包级 tonLedger")
	}
}

// TestJournalSurvivesChannelRestart 台账是包级实例，重建通道后仍能读到之前的条目。
func TestJournalSurvivesChannelRestart(t *testing.T) {
	SetJournal(newMapJournal())
	t.Cleanup(func() { SetJournal(nil) })

	ch1 := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch1.Close() })
	if err := ch1.led.Put(&ledger.Entry{OrderNo: "O-PERSIST", Amount: 7, State: ledger.StatePrepared}); err != nil {
		t.Fatal(err)
	}

	ch2 := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch2.Close() })
	got, ok := ch2.led.Get("O-PERSIST")
	if !ok || got.Amount != 7 {
		t.Fatalf("重建通道后台账条目丢失: %+v found=%v", got, ok)
	}
}

func TestErrorAliases(t *testing.T) {
	if ErrOrderAlreadySettled != ledger.ErrAlreadySettled {
		t.Fatal("ErrOrderAlreadySettled 与 ledger.ErrAlreadySettled 不是同一个值")
	}
	if ErrJournalUnavailable != ledger.ErrJournalUnavailable {
		t.Fatal("ErrJournalUnavailable 与 ledger.ErrJournalUnavailable 不是同一个值")
	}
}
