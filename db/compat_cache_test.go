package db

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// 兼容性编译断言：两级缓存是纯追加能力，原有独立构造接口
// （NewRedis/NewMySql/NewRepository[T]/NewMongoDB）必须仍然可用。
// 这里只取函数引用与方法表达式，不建立任何真实连接。
var (
	_ = NewRedis
	_ = NewMySql
	_ = NewMongoDB
	_ = NewRepository[testVal]

	// 写线批量落库的新入口（纯追加）
	_ func(*Repository[testVal], context.Context, []*testVal, []string) error = (*Repository[testVal]).UpsertBatch

	// 门面签名稳定性断言
	_ func(*Redis) KV               = NewRedisKV
	_ func(redis.Cmdable, int) KV   = NewRedisKVStore
	_ func(CacheConfig) LayerOption = WithLayerConfig
	_ func() Codec                  = NewJSONCodec
	_ func() CacheConfig            = DefaultCacheConfig
	_ func(*CacheLayer, string) int = layerLenForTest
)

type testVal struct {
	N int `json:"n"`
}

func layerLenForTest(l *CacheLayer, _ string) int {
	return len(l.Stats())
}

func TestLegacyApisStillExported(t *testing.T) {
	// 装配路径存活检查：Layer 构造不连库；kv 为 nil 时注册应报错而非 panic
	l := NewCacheLayer(nil)
	if l == nil {
		t.Fatal("NewCacheLayer 返回 nil")
	}
	if _, err := OpenCache[string, testVal](l, "x"); err == nil {
		t.Fatal("kv 为 nil 时 OpenCache 应报错")
	}
	if got := layerLenForTest(l, ""); got != 0 {
		t.Fatalf("注册失败不应留下句柄: %d", got)
	}
}
