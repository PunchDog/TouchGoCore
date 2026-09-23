package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestRegisterFuncBeforeLoad 测试在 LoadWithError 之前注册功能配置
func TestRegisterFuncBeforeLoad(t *testing.T) {
	// 创建临时目录结构
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 创建 config.ini
	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建主配置文件
	mainJSON := `{
		"log_level": "info"
	}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建功能配置文件
	type GameRules struct {
		MaxPlayers int    `json:"max_players"`
		TimeLimit  int    `json:"time_limit"`
		Mode       string `json:"mode"`
	}
	featureJSON := `{
		"max_players": 10,
		"time_limit": 300,
		"mode": "classic"
	}`
	if err := os.WriteFile(filepath.Join(featureDir, "game_rules.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// 重置全局状态
	resetFeatureState()
	_configDirFlag = confDir

	// 在 LoadWithError 之前注册
	rules := &GameRules{}
	if err := RegisterFunc("game_rules", rules); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// 加载配置
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证 struct 被正确填充
	if rules.MaxPlayers != 10 {
		t.Errorf("MaxPlayers = %d, want 10", rules.MaxPlayers)
	}
	if rules.TimeLimit != 300 {
		t.Errorf("TimeLimit = %d, want 300", rules.TimeLimit)
	}
	if rules.Mode != "classic" {
		t.Errorf("Mode = %q, want %q", rules.Mode, "classic")
	}

	// 验证 GetFeatureConfig
	v, ok := GetFeatureConfig("game_rules")
	if !ok {
		t.Fatal("GetFeatureConfig returned false")
	}
	if v != rules {
		t.Error("GetFeatureConfig returned different pointer")
	}

	// 验证 IsFeatureLoaded
	if !IsFeatureLoaded("game_rules") {
		t.Error("IsFeatureLoaded returned false")
	}
}

// TestRegisterFuncWithNil 测试注册 nil 时按 map[string]any 读取
func TestRegisterFuncWithNil(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{
		"key1": "value1",
		"key2": 42,
		"key3": true
	}`
	if err := os.WriteFile(filepath.Join(featureDir, "dynamic_config.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	_configDirFlag = confDir

	// 注册 nil
	if err := RegisterFunc("dynamic_config", nil); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证 map[string]any
	m, ok := GetFeatureConfigMap("dynamic_config")
	if !ok {
		t.Fatal("GetFeatureConfigMap returned false")
	}
	if m["key1"] != "value1" {
		t.Errorf("key1 = %v, want %q", m["key1"], "value1")
	}
	if m["key2"] != float64(42) {
		t.Errorf("key2 = %v, want 42", m["key2"])
	}
	if m["key3"] != true {
		t.Errorf("key3 = %v, want true", m["key3"])
	}
}

// TestRegisterFuncAfterLoad 测试在 LoadWithError 之后注册功能配置
func TestRegisterFuncAfterLoad(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{"reward": 100}`
	if err := os.WriteFile(filepath.Join(featureDir, "reward.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	_configDirFlag = confDir

	// 先加载主配置
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证功能配置未加载
	if IsFeatureLoaded("reward") {
		t.Error("reward should not be loaded yet")
	}

	// 之后注册
	type RewardConfig struct {
		Reward int `json:"reward"`
	}
	reward := &RewardConfig{}
	if err := RegisterFunc("reward", reward); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// 验证立即加载
	if !IsFeatureLoaded("reward") {
		t.Error("reward should be loaded after RegisterFunc")
	}
	if reward.Reward != 100 {
		t.Errorf("Reward = %d, want 100", reward.Reward)
	}
}

// TestRegisterFuncDuplicate 测试重复注册返回错误
func TestRegisterFuncDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	_configDirFlag = confDir

	// 第一次注册
	if err := RegisterFunc("test", nil); err != nil {
		t.Fatalf("First RegisterFunc failed: %v", err)
	}

	// 第二次注册应该返回错误
	if err := RegisterFunc("test", nil); err == nil {
		t.Error("Second RegisterFunc should return error")
	}
}

// TestFeatureConfigMissing 测试 JSON 文件不存在时的错误处理
func TestFeatureConfigMissing(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	_configDirFlag = confDir

	// 注册不存在的配置
	if err := RegisterFunc("missing", nil); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// LoadWithError 应该返回错误
	cfg := &Cfg{}
	err := cfg.LoadWithError("GateWayServer")
	if err == nil {
		t.Error("LoadWithError should return error for missing config")
	}
}

// TestFeatureConfigAbsolutePath 测试绝对路径
func TestFeatureConfigAbsolutePath(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	absFeatureDir := filepath.Join(tmpDir, "absolute_features")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(absFeatureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=` + absFeatureDir + `
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{"value": "absolute"}`
	if err := os.WriteFile(filepath.Join(absFeatureDir, "abs_config.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	_configDirFlag = confDir

	type AbsConfig struct {
		Value string `json:"value"`
	}
	absCfg := &AbsConfig{}
	if err := RegisterFunc("abs_config", absCfg); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	if absCfg.Value != "absolute" {
		t.Errorf("Value = %q, want %q", absCfg.Value, "absolute")
	}

	// 验证 GetFeatureDir 返回绝对路径
	if GetFeatureDir() != absFeatureDir {
		t.Errorf("GetFeatureDir = %q, want %q", GetFeatureDir(), absFeatureDir)
	}
}

// TestNormalizeJSONName 测试文件名规范化
func TestNormalizeJSONName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"game_rules", "game_rules.json"},
		{"game_rules.json", "game_rules.json"},
		{"GAME_RULES.JSON", "GAME_RULES.JSON"},
		{"config", "config.json"},
		{"  test  ", "test.json"},
	}

	for _, tt := range tests {
		result := normalizeJSONName(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeJSONName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// resetFeatureState 重置功能配置全局状态（仅用于测试）
func resetFeatureState() {
	_confDirField = ""
	_featureDir = ""
	_featureDirSet = false
	_featureReg = sync.Map{}
	_featureData = sync.Map{}
	_featureLoaded = sync.Map{}
}
