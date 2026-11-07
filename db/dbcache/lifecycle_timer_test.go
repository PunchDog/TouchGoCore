package dbcache

import (
	"context"
	"testing"
	"time"

	"touchgocore/localtimer"
)

func TestStartStopWiresPeriodicFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localtimer.Run(ctx)
	defer localtimer.TimeStop(context.Background())

	e, fs, _, sink, b := newTestEngine(t)
	b.interval = 20 * time.Millisecond
	e.cfg.FlushInterval = 20 * time.Millisecond
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.save[testUser](ctx, "test_user", "7", &testUser{ID: 7, Name: "timer"}, false); err != nil {
		t.Fatalf("save: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fs.dirtyCount(b.keys.dirty) == 0 && len(sink.snapshot()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := fs.dirtyCount(b.keys.dirty); n != 0 {
		t.Fatalf("周期定时器未清空脏账，剩余 %d", n)
	}
	if rows := sink.snapshot(); len(rows) != 1 || rows[0].Name != "timer" {
		t.Fatalf("周期定时器落库内容=%v", rows)
	}
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
