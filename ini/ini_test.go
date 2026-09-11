package ini

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NonExistent(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "no_such.ini"))
	if err == nil {
		t.Fatal("加载不存在文件应报错")
	}
}

func TestLoad_BasicINI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.ini")
	content := "[GLOBAL]\ndebug = true\nfps = 60\n[GAME]\nname = tcore\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试 ini 失败: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := cfg.GetString("GLOBAL", "debug", "false"); got != "true" {
		t.Fatalf("GLOBAL.debug=%s want=true", got)
	}
	if got := cfg.GetString("GAME", "name", ""); got != "tcore" {
		t.Fatalf("GAME.name=%s want=tcore", got)
	}
}

func TestIniParser_GetString_Default(t *testing.T) {
	p := &IniParser{} // 未 Load
	if got := p.GetString("X", "y", "fallback"); got != "fallback" {
		t.Fatalf("未 Load 应返回默认值，实际: %s", got)
	}
	if got := p.GetInt32("X", "y", 99); got != 99 {
		t.Fatalf("未 Load 应返回默认 int32，实际: %d", got)
	}
}

func TestLoadConfigByNoSectionName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.ini")
	if err := os.WriteFile(path, []byte("[A]\nk=1\n[B]\nk=2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadConfigByNoSectionName(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["A"]["k"] != "1" || m["B"]["k"] != "2" {
		t.Fatalf("LoadConfigByNoSectionName 解析异常: %v", m)
	}
}
