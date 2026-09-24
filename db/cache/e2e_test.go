//go:build e2e

// 真连接烟测：本地无 Redis/MySQL 实例，默认不编译不运行。
// 有环境时（先建好表 tg_cache_e2e_user: 列 k(主键)+name）：
//
//	go test -tags e2e ./db/cache/ -run E2E -v
//
// 连接参数按环境变量注入，缺省指向本机默认端口/账号。
package cache_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"touchgocore/config"
	cachepkg "touchgocore/db/cache"
	mysqldb "touchgocore/db/mysql"
)

type e2eUser struct {
	K    string `gorm:"primaryKey;column:k" json:"k"`
	Name string `json:"name"`
}

func (e2eUser) TableName() string { return "tg_cache_e2e_user" }

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestE2ERedisMySQLRoundTrip(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: getenv("TG_E2E_REDIS_ADDR", "127.0.0.1:6379")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis 不可用，跳过 e2e: %v", err)
	}
	client, err := mysqldb.NewClient(&config.MySqlDBConfig{
		Host:     getenv("TG_E2E_MYSQL_HOST", "127.0.0.1:3306"),
		Username: getenv("TG_E2E_MYSQL_USER", "root"),
		Password: os.Getenv("TG_E2E_MYSQL_PASS"),
		DBName:   getenv("TG_E2E_MYSQL_DB", "test"),
	})
	if err != nil {
		t.Skipf("mysql 不可用，跳过 e2e: %v", err)
	}
	defer client.Close()

	idKey := getenv("TG_E2E_MYSQL_ID", "e2e-1")
	repo := mysqldb.NewRepository[e2eUser](client)
	keyToID := func(k string) any { return k }
	kv := cachepkg.NewRedisStore(rdb, 16)
	cfg := cachepkg.DefaultConfig()
	cfg.TTL = 2 * time.Second
	c, err := cachepkg.New[string, e2eUser]("e2e", kv,
		cachepkg.WithConfig[string, e2eUser](cfg),
		cachepkg.WithLoader[string, e2eUser](cachepkg.MysqlSource[e2eUser, string](repo, keyToID)),
		cachepkg.WithSaver[string, e2eUser](cachepkg.MysqlBatchSink[e2eUser, string](repo, keyToID, "k")),
	)
	if err != nil {
		t.Fatal(err)
	}

	// 写线：同步写 Redis 即返回，Flush 后经 UpsertBatch 落库
	if err := c.Write(ctx, idKey, &e2eUser{K: idKey, Name: "e2e"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("final flush: %v", err)
	}
	// 读线：先绕开缓存确认库里真有这行，再经缓存读回一致
	direct, err := repo.FindByID(ctx, idKey)
	if err != nil {
		t.Fatalf("落库验证失败: %v", err)
	}
	v, err := c.GetOrLoad(ctx, idKey)
	if err != nil || *v != *direct {
		t.Fatalf("Redis 读回不一致: v=%v direct=%v err=%v", v, direct, err)
	}
	// 清理：tombstone 落库删除 + 缓存已同步 DEL
	if err := c.Remove(ctx, idKey); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
