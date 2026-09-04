package util

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

type memVTStore struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

func newMemVTStore() *memVTStore {
	return &memVTStore{data: make(map[string]map[string]string)}
}

func (m *memVTStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.data[key]
	if src == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func (m *memVTStore) HSet(_ context.Context, key string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := m.data[key]
	if dst == nil {
		dst = make(map[string]string)
		m.data[key] = dst
	}
	for k, v := range fields {
		dst[k] = vtFieldString(v)
	}
	return nil
}

func (m *memVTStore) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func vtFieldString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

func TestVirtualTimeRedisKeyIncludesGameGroup(t *testing.T) {
	prev := GameGroup
	GameGroup = "unit"
	t.Cleanup(func() { GameGroup = prev })
	want := VirtualTimeKey + ":unit"
	if VirtualTimeRedisKey() != want {
		t.Fatalf("key=%s want=%s", VirtualTimeRedisKey(), want)
	}
}

func TestResetVirtualTimeUsesGroupKey(t *testing.T) {
	prev := GameGroup
	GameGroup = "g1"
	t.Cleanup(func() {
		GameGroup = prev
		useVirtualTimeBackend(nil)
	})
	store := newMemVTStore()
	useVirtualTimeBackend(store)
	ctx := context.Background()
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(ctx, now, now+time.Hour.Nanoseconds()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[VirtualTimeRedisKey()]; !ok {
		t.Fatal("expected group key written")
	}
	if _, ok := store.data[VirtualTimeKey]; ok {
		t.Fatal("should not write bare VirtualTimeKey")
	}
	if err := ResetVirtualTime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data[VirtualTimeRedisKey()]; ok {
		t.Fatal("reset should delete group key")
	}
}

func TestCurrentTimeUsesCacheNotBackendEveryCall(t *testing.T) {
	prev := GameGroup
	GameGroup = "cache"
	t.Cleanup(func() {
		GameGroup = prev
		useVirtualTimeBackend(nil)
		SetVirtualTimeCacheTTL(defaultVTCacheTTL)
	})
	store := newMemVTStore()
	useVirtualTimeBackend(store)
	SetVirtualTimeCacheTTL(time.Hour)
	now := time.Now().UnixNano()
	if err := SetVirtualTimeData(context.Background(), now, now+2*time.Hour.Nanoseconds()); err != nil {
		t.Fatal(err)
	}
	got := CurrentTime()
	if got.Sub(time.Now()) < time.Hour {
		t.Fatalf("expected ~2h offset, got %v", got)
	}
	// wipe backend; cache should still apply offset
	store.mu.Lock()
	store.data = map[string]map[string]string{}
	store.mu.Unlock()
	got2 := CurrentTime()
	if got2.Sub(time.Now()) < time.Hour {
		t.Fatal("cache miss after backend wipe")
	}
}

func TestCurrentTimeWithoutBackendIsRealTime(t *testing.T) {
	useVirtualTimeBackend(nil)
	delta := time.Since(CurrentTime())
	if delta < 0 {
		delta = -delta
	}
	if delta > time.Second {
		t.Fatalf("expected real time, drift=%v", delta)
	}
}
