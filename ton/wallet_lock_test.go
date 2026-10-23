package ton

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	tongo "github.com/tonkeeper/tongo/ton"

	"touchgocore/ledger"
)

// gatedSeqnoGateway 让第一次 seqno 读停在闸上，并模拟「受理成功即推进 seqno」。
// 这样两笔不同订单的并发出款能确定性地暴露「是否共读了同一个序号」。
type gatedSeqnoGateway struct {
	*fakeTONGateway
	firstEntered chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

func (g *gatedSeqnoGateway) seqno(_ context.Context, _ tongo.AccountID) (uint32, error) {
	n := g.calls.Add(1)
	if n == 1 {
		close(g.firstEntered)
		<-g.releaseFirst
	}
	g.fakeTONGateway.mu.Lock()
	defer g.fakeTONGateway.mu.Unlock()
	return g.fakeTONGateway.seqnoVal, g.fakeTONGateway.seqnoErr
}

func (g *gatedSeqnoGateway) sendExtMessage(ctx context.Context, payload []byte) (*tonBroadcast, error) {
	res, err := g.fakeTONGateway.sendExtMessage(ctx, payload)
	if err == nil && res != nil && res.Accepted {
		g.fakeTONGateway.mu.Lock()
		g.fakeTONGateway.seqnoVal++
		g.fakeTONGateway.mu.Unlock()
	}
	return res, err
}

func (c *tonSignCounter) seqnoSnapshot() []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint32, len(c.seqnos))
	copy(out, c.seqnos)
	return out
}

// TestDifferentOrdersSerializeOnWalletSeqno F5：不同单号并发出款时，第二单必须
// 等第一单完成「seqno → 签名 → 广播」后再读序号，不能两单签同一个 seqno。
func TestDifferentOrdersSerializeOnWalletSeqno(t *testing.T) {
	gw := &gatedSeqnoGateway{
		fakeTONGateway: &fakeTONGateway{},
		firstEntered:   make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	c, sign := newTestTONChannel(t, gw, nil)

	aDone := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(aDone)
		_, _ = c.Withdraw(context.Background(), newTONOrder("WD-LOCK-A"))
	}()
	<-gw.firstEntered

	go func() {
		defer close(bDone)
		_, _ = c.Withdraw(context.Background(), newTONOrder("WD-LOCK-B"))
	}()

	// B 应该卡在 tonWalletMu 上，而不是去读第二个 seqno。
	select {
	case <-bDone:
		t.Fatal("B 在第一单未放行时就完成了")
	case <-time.After(150 * time.Millisecond):
	}
	if got := gw.calls.Load(); got != 1 {
		t.Fatalf("第二单没被钱包锁挡住：seqno 读了 %d 次", got)
	}

	close(gw.releaseFirst)
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Fatal("A 未完成")
	}
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("B 未完成")
	}

	seqnos := sign.seqnoSnapshot()
	if len(seqnos) != 2 || seqnos[0] != 0 || seqnos[1] != 1 {
		t.Fatalf("两单没有拿到连续且不同的 seqno: %v", seqnos)
	}
}

// TestPreparedRebroadcastTakesWalletLock prepared 分支不签名，但重发同一条报文
// 也会占用钱包序号；不拿钱包锁会让新单同时读到同一个 seqno。
func TestPreparedRebroadcastTakesWalletLock(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-PREP-LOCK")
	if err := c.led.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "prepared-hash", RawTx: []byte{1, 2, 3},
		From: c.wallet, To: o.To.Address, Amount: o.Amount,
		State:       ledger.StatePrepared,
		ExpiresAtMS: tonTestClock.Add(time.Minute).UnixMilli(),
		UpdatedAtMS: tonTestClock.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	tonWalletMu.Lock()
	locked := true
	defer func() {
		if locked {
			tonWalletMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Withdraw(context.Background(), o)
	}()
	select {
	case <-done:
		t.Fatal("prepared 重发没有等待钱包锁")
	case <-time.After(150 * time.Millisecond):
	}
	if gw.sendCount() != 0 {
		t.Fatalf("钱包锁被占着时仍然广播了 %d 次", gw.sendCount())
	}
	tonWalletMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放钱包锁后 prepared 重发没有完成")
	}
	if gw.sendCount() != 1 {
		t.Fatalf("prepared 重发次数=%d，期望 1", gw.sendCount())
	}
}

// TestQueryOrderDoesNotTakeWalletLock 查单是只读路径，不能因为钱包正在出款而被串行。
func TestQueryOrderDoesNotTakeWalletLock(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	if err := c.led.Put(&ledger.Entry{
		OrderNo: "WD-QUERY-LOCK", TxID: "query-hash", From: c.wallet,
		To: tonAcctBounceable, Amount: 1_000_000_000, State: ledger.StateBroadcast,
	}); err != nil {
		t.Fatal(err)
	}

	tonWalletMu.Lock()
	defer tonWalletMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryOrder(context.Background(), "WD-QUERY-LOCK")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("查单失败: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("查单被钱包锁阻塞")
	}
}
