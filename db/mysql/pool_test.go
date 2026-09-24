package mysql

import (
	"testing"

	"touchgocore/config"
	"touchgocore/db/dbmap"
)

func TestPoolKey_StabilityAndUniqueness(t *testing.T) {
	cfg1 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db"}
	cfg2 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db"}
	cfg3 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p2", DBName: "db"}
	cfg4 := &config.MySqlDBConfig{Host: "h:3306", Username: "u", Password: "p", DBName: "db2"}

	c1 := &Client{cfg: cfg1}
	c2 := &Client{cfg: cfg2}
	c3 := &Client{cfg: cfg3}
	c4 := &Client{cfg: cfg4}

	if c1.poolKey() != c2.poolKey() {
		t.Fatalf("同配置 poolKey 应一致: %s vs %s", c1.poolKey(), c2.poolKey())
	}
	if c1.poolKey() == c3.poolKey() {
		t.Fatal("不同密码 poolKey 应不同")
	}
	if c1.poolKey() == c4.poolKey() {
		t.Fatal("不同 DBName poolKey 应不同")
	}
}

func TestConnectOnly_MissAndInvalidEntry(t *testing.T) {
	c := &Client{cfg: &config.MySqlDBConfig{Host: "x", DBName: "y"}}

	if c.connectOnly("nonexistent-pool-key-xyz") {
		t.Fatal("未注册 key 不应命中 connectOnly")
	}

	// 注册一个非 *gorm.DB 值，connectOnly 应安全返回 false
	dbmap.Global.Store("not-gorm-key", "string-value")
	defer dbmap.Global.Delete("not-gorm-key")
	if c.connectOnly("not-gorm-key") {
		t.Fatal("类型不匹配应返回 false")
	}
}

func TestDbmapGlobalRoundtrip(t *testing.T) {
	const key = "test-roundtrip-key"
	defer dbmap.Global.Delete(key)

	dbmap.Global.Store(key, "v")
	v, ok := dbmap.Global.Load(key)
	if !ok || v != "v" {
		t.Fatalf("roundtrip 失败: %v %v", v, ok)
	}

	dbmap.Global.Delete(key)
	if _, ok := dbmap.Global.Load(key); ok {
		t.Fatal("Delete 后 Load 应返回 !ok")
	}
}
