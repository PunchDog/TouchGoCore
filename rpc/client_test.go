package rpc

import (
	"testing"
	"time"

	"touchgocore/network/message"
)

func TestFailAllPendingWakesWaiter(t *testing.T) {
	c := &RpcClient{}
	ch := make(chan *message.FSMessage, 1)
	c.pending.Store(uint64(1), ch)

	done := make(chan struct{})
	go func() {
		recv := <-ch
		if recv != nil {
			t.Errorf("expected nil disconnect sentinel, got %v", recv)
		}
		close(done)
	}()

	c.failAllPending()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pending waiter was not woken")
	}
}
