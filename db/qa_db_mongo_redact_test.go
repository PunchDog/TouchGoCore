package db

import (
	"fmt"
	"strings"
	"testing"

	"touchgocore/config"
)

// ==================== P1：Mongo 凭证不得进日志 / dbmap 键 ====================
//
// 事故形状：newMongoDB 用 fmt.Sprintf(cfg.MongoUpUrl, user, pass, host, db)
// 拼出含密码的整串 URL，既作 dbmap.Global 的键，又被 vars.Info 原样打进日志。
// 修复：连接键改为 mongoPoolKey（host|db|replicaSet|user|sha256(pass)前8字节），
// 日志只打 host+db。vars 无日志捕获设施，故对「键构造」做纯函数断言。

func testMongoCfg(user, pass string) *config.MongoDBConfig {
	return &config.MongoDBConfig{
		Host:           "10.0.0.1:27017",
		Username:       user,
		Password:       pass,
		DBName:         "game",
		MongoUpUrl:     "mongodb://%s:%s@%s/%s",
		ReplicaSetName: "rs0",
	}
}

func TestMongoPoolKey_NoCredentials(t *testing.T) {
	const pass = "p@ssw0rd!secret"
	cfg := testMongoCfg("admin", pass)
	key := mongoPoolKey(cfg)
	if strings.Contains(key, pass) {
		t.Fatalf("连接键含密码明文: %q", key)
	}
	if strings.Contains(key, "admin") {
		t.Fatalf("连接键含用户名（口径：只留 host/db 级标识与密码哈希）: %q", key)
	}
	if !strings.Contains(key, cfg.Host) || !strings.Contains(key, cfg.DBName) {
		t.Fatalf("连接键应含 host/db 便于排障: %q", key)
	}
}

func TestMongoPoolKey_StableAndDistinct(t *testing.T) {
	base := testMongoCfg("admin", "pass1")
	if mongoPoolKey(base) != mongoPoolKey(base) {
		t.Fatal("同配置键不稳定：同一实例将命中不了同一连接")
	}
	// 密码轮换 → 必须换键（否则复用旧凭证的连接）
	rotated := testMongoCfg("admin", "pass2")
	if mongoPoolKey(base) == mongoPoolKey(rotated) {
		t.Fatal("密码不同键却相同")
	}
	// 不同实例不撞键
	otherHost := testMongoCfg("admin", "pass1")
	otherHost.Host = "10.0.0.2:27017"
	if mongoPoolKey(base) == mongoPoolKey(otherHost) {
		t.Fatal("host 不同键却相同")
	}
	otherDB := testMongoCfg("admin", "pass1")
	otherDB.DBName = "pay"
	if mongoPoolKey(base) == mongoPoolKey(otherDB) {
		t.Fatal("db 不同键却相同")
	}
	otherRS := testMongoCfg("admin", "pass1")
	otherRS.ReplicaSetName = "rs1"
	if mongoPoolKey(base) == mongoPoolKey(otherRS) {
		t.Fatal("replicaSet 不同键却相同")
	}
}

// 反证：生产 URL 构造确实把密码拼进了整串（脱敏必要性），而连接键不得等于该串。
func TestMongoPoolKey_NotRawURL(t *testing.T) {
	cfg := testMongoCfg("admin", "pass1")
	rawURL := fmt.Sprintf(cfg.MongoUpUrl, cfg.Username, cfg.Password, cfg.Host, cfg.DBName)
	if !strings.Contains(rawURL, "pass1") {
		t.Fatal("测试前提不成立：URL 模板应含密码占位")
	}
	if mongoPoolKey(cfg) == rawURL {
		t.Fatal("连接键就是含密码的原始 URL")
	}
}
