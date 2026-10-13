package config

import (
	"encoding/json"
	"os"
	"testing"
)

// TestExampleJsonPostgresBinds 示例里的 postgres 段必须逐字段绑定到 PostgreDBConfig。
// encoding/json 对未知键静默丢弃：键名写错的配置看着生效、实则永远走默认值。
func TestExampleJsonPostgresBinds(t *testing.T) {
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if cfg.Postgre == nil {
		t.Fatal(`example.json 缺少 "postgres" 配置段`)
	}
	pg := cfg.Postgre
	// 逐字段核对示例值确实落进了结构体（任一 json tag 写错都会在这里露馅）
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"pg_host", pg.Host, "127.0.0.1"},
		{"pg_port", pg.Port, 5432},
		{"pg_username", pg.Username, "postgres"},
		{"pg_password", pg.Password, "change-me"},
		{"pg_dbname", pg.DBName, "example"},
		{"pg_sslmode", pg.SSLMode, "disable"},
		{"pg_timezone", pg.TimeZone, "UTC"},
		{"pg_schema", pg.Schema, "public"},
		{"pg_max_idle_conns", pg.MaxIdleConns, 100},
		{"pg_max_open_conns", pg.MaxOpenConns, 500},
		{"pg_conn_max_lifetime", pg.ConnMaxLifetime, 86400},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 绑定错: 期望 %v, 得 %v", c.name, c.want, c.got)
		}
	}
}
