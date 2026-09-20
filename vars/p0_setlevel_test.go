package vars

import (
	"testing"
	"time"
)

// callWithTimeout 在 d 内执行 fn，超时视为死锁
func callWithTimeout(t *testing.T, d time.Duration, name string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s 返回错误: %v", name, err)
		}
	case <-time.After(d):
		t.Fatalf("%s 死锁：未在 %v 内返回", name, d)
	}
}

func TestLoggerManager_SetLevelNoDeadlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.Async = false
	cfg.LogLevel = LogLevelInfo
	lm, err := NewLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewLoggerManager: %v", err)
	}
	defer lm.Close()

	callWithTimeout(t, 3*time.Second, "LoggerManager.SetLevel", func() error {
		return lm.SetLevel(LogLevelWarn)
	})
	callWithTimeout(t, 3*time.Second, "LoggerManager.SetLevel(off)", func() error {
		return lm.SetLevel(LogLevelOff)
	})
}

func TestChannelLoggerManager_SetLevelNoDeadlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.Async = false
	cfg.LogLevel = LogLevelInfo
	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	defer m.Close()

	callWithTimeout(t, 3*time.Second, "ChannelLoggerManager.SetLevel", func() error {
		return m.SetLevel(LogLevelWarn)
	})
}

func TestChannelLoggerManager_SetLevelAsyncNoDeadlock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogPath = t.TempDir()
	cfg.Async = true
	cfg.AsyncBufferSize = 128
	m, err := NewChannelLoggerManager(cfg)
	if err != nil {
		t.Fatalf("NewChannelLoggerManager: %v", err)
	}
	if err := m.initWithChannel(); err != nil {
		t.Fatalf("initWithChannel: %v", err)
	}
	defer m.Close()

	callWithTimeout(t, 3*time.Second, "ChannelLoggerManager.SetLevel(async)", func() error {
		return m.SetLevel(LogLevelError)
	})
}
