package config

import (
	"os"
	"path/filepath"
	"testing"
)

// snapshotPathState / restorePathState / setConfigDirFlagForTest 是测试对包级路径状态的
// 唯一入口：这些字段已由 _stateMu 保护，测试也不能绕过锁直接赋值，否则与
// 并发用例（concurrency_state_test.go）同时在飞时会自己造成竞争。
func snapshotPathState() (base, conf, defFile, flagDir string) {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _basePath, _confDir, _defaultFile, _configDirFlag
}

func restorePathState(base, conf, defFile, flagDir string) {
	_stateMu.Lock()
	_basePath, _confDir, _defaultFile, _configDirFlag = base, conf, defFile, flagDir
	_stateMu.Unlock()
}

func setConfigDirFlagForTest(dir string) {
	_stateMu.Lock()
	_configDirFlag = dir
	_stateMu.Unlock()
}

func withConfigPaths(t *testing.T) {
	t.Helper()
	prevBase, prevConf, prevFile, prevFlag := snapshotPathState()
	t.Cleanup(func() {
		restorePathState(prevBase, prevConf, prevFile, prevFlag)
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
	setConfigDirFlagForTest(conf)
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
	setConfigDirFlagForTest(conf)
	cfg := &Cfg{}
	if err := cfg.LoadWithError("UnknownServer"); err == nil {
		t.Fatal("expected empty ini name error")
	}
}
