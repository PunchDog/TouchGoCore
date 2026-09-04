package config

import (
	"os"
	"path/filepath"
	"testing"
)

func withConfigPaths(t *testing.T) {
	t.Helper()
	prevBase, prevConf, prevFile, prevFlag := _basePath, _confDir, _defaultFile, _configDirFlag
	t.Cleanup(func() {
		_basePath, _confDir, _defaultFile, _configDirFlag = prevBase, prevConf, prevFile, prevFlag
	})
}

func writeTempConf(t *testing.T) (base, conf string) {
	t.Helper()
	base = t.TempDir()
	conf = filepath.Join(base, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := "[GateWayServer]\nini=gate.json\n"
	if err := os.WriteFile(filepath.Join(conf, "config.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	json := `{"log_level":"info","map_path":"off"}`
	if err := os.WriteFile(filepath.Join(conf, "gate.json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, conf
}

func TestApplyConfigDirAsConfFolder(t *testing.T) {
	withConfigPaths(t)
	base, conf := writeTempConf(t)
	applyConfigDir(conf)
	if GetConfDir() != conf {
		if abs, _ := filepath.Abs(conf); GetConfDir() != abs {
			t.Fatalf("conf dir=%s want %s", GetConfDir(), conf)
		}
	}
	if filepath.Base(GetDefaultFile()) != "config.ini" {
		t.Fatalf("ini=%s", GetDefaultFile())
	}
	if GetBasePath() != filepath.Dir(GetConfDir()) {
		t.Fatalf("base=%s dir(conf)=%s", GetBasePath(), filepath.Dir(GetConfDir()))
	}
	if GetBasePath() != base && GetBasePath() != filepath.Clean(base) {
		abs, _ := filepath.Abs(base)
		if GetBasePath() != abs {
			t.Fatalf("base path=%s want %s", GetBasePath(), base)
		}
	}
}

func TestApplyConfigDirAsParentOfConf(t *testing.T) {
	withConfigPaths(t)
	base, conf := writeTempConf(t)
	applyConfigDir(base)
	got, _ := filepath.Abs(conf)
	if GetConfDir() != got && GetConfDir() != conf {
		t.Fatalf("conf dir=%s want %s", GetConfDir(), conf)
	}
}

func TestLoadWithErrorFromConfDir(t *testing.T) {
	withConfigPaths(t)
	_, conf := writeTempConf(t)
	_configDirFlag = conf
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("log_level=%s", cfg.LogLevel)
	}
}

func TestLoadWithErrorMissingServerSection(t *testing.T) {
	withConfigPaths(t)
	_, conf := writeTempConf(t)
	_configDirFlag = conf
	cfg := &Cfg{}
	if err := cfg.LoadWithError("UnknownServer"); err == nil {
		t.Fatal("expected empty ini name error")
	}
}
