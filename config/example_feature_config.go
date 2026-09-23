//go:build ignore

package main

import (
	"fmt"
	"log"

	"touchgocore/config"
)

// ============================================
// 功能配置注册系统使用示例
// ============================================

// 1. 定义功能配置的结构体
type GameRulesConfig struct {
	MaxPlayers int    `json:"max_players"` // 最大玩家数
	TimeLimit  int    `json:"time_limit"`  // 时间限制（秒）
	Mode       string `json:"mode"`        // 游戏模式
	EnableAI   bool   `json:"enable_ai"`   // 是否启用AI
}

type RewardConfig struct {
	LoginReward    int `json:"login_reward"`     // 登录奖励
	FirstWinReward int `json:"first_win_reward"` // 首胜奖励
	DailyLimit     int `json:"daily_limit"`      // 每日上限
}

type FeatureFlags struct {
	EnableNewUI     bool `json:"enable_new_ui"`
	EnableBetaMode  bool `json:"enable_beta_mode"`
	MaintenanceMode bool `json:"maintenance_mode"`
}

func main() {
	// ============================================
	// 方式一：在 LoadWithError 之前注册（推荐）
	// ============================================

	// 注册 struct 类型的配置
	gameRules := &GameRulesConfig{}
	if err := config.RegisterFunc("game_rules", gameRules); err != nil {
		log.Fatalf("注册 game_rules 失败: %v", err)
	}

	rewardCfg := &RewardConfig{}
	if err := config.RegisterFunc("reward_config", rewardCfg); err != nil {
		log.Fatalf("注册 reward_config 失败: %v", err)
	}

	// 注册 map[string]any 类型的配置（传 nil）
	if err := config.RegisterFunc("feature_flags", nil); err != nil {
		log.Fatalf("注册 feature_flags 失败: %v", err)
	}

	// 加载主配置（会自动加载所有已注册的功能配置）
	if err := config.Cfg_.LoadWithError("GateWayServer"); err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 使用 struct 配置
	fmt.Printf("游戏规则: 最大玩家=%d, 时间限制=%d秒, 模式=%s\n",
		gameRules.MaxPlayers, gameRules.TimeLimit, gameRules.Mode)

	fmt.Printf("奖励配置: 登录奖励=%d, 首胜奖励=%d\n",
		rewardCfg.LoginReward, rewardCfg.FirstWinReward)

	// 使用 map 配置
	if flags, ok := config.GetFeatureConfigMap("feature_flags"); ok {
		fmt.Printf("功能开关: %v\n", flags)
		if enableNewUI, ok := flags["enable_new_ui"].(bool); ok {
			fmt.Printf("新UI已启用: %v\n", enableNewUI)
		}
	}

	// ============================================
	// 方式二：在 LoadWithError 之后注册（动态加载）
	// ============================================

	// 假设运行时需要根据条件加载额外配置
	dynamicCfg := &FeatureFlags{}
	if err := config.RegisterFunc("dynamic_flags", dynamicCfg); err != nil {
		log.Printf("注册 dynamic_flags 失败: %v", err)
	} else {
		// 立即加载完成
		fmt.Printf("动态配置已加载: 新UI=%v, Beta模式=%v\n",
			dynamicCfg.EnableNewUI, dynamicCfg.EnableBetaMode)
	}

	// ============================================
	// 其他辅助函数
	// ============================================

	// 检查配置是否已加载
	if config.IsFeatureLoaded("game_rules") {
		fmt.Println("game_rules 已加载")
	}

	// 获取功能配置文件夹路径
	fmt.Printf("功能配置目录: %s\n", config.GetFeatureDir())

	// 通用获取接口（返回 any）
	if v, ok := config.GetFeatureConfig("reward_config"); ok {
		if rc, ok := v.(*RewardConfig); ok {
			fmt.Printf("通过 GetFeatureConfig 获取: 每日限制=%d\n", rc.DailyLimit)
		}
	}
}

// ============================================
// 对应的配置文件示例
// ============================================

/*
config.ini:
[GLOBAL]
debug=true
fps=120

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs

feature_configs/game_rules.json:
{
    "max_players": 10,
    "time_limit": 300,
    "mode": "classic",
    "enable_ai": true
}

feature_configs/reward_config.json:
{
    "login_reward": 100,
    "first_win_reward": 500,
    "daily_limit": 10000
}

feature_configs/feature_flags.json:
{
    "enable_new_ui": true,
    "enable_beta_mode": false,
    "maintenance_mode": false
}
*/
