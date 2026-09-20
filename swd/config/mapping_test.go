package config

import (
	"sync"
	"testing"

	"touchgocore/swd/config/mappingdata"
)

// TestGetGlobalMappingLoadsEmbeddedDefaults 全局配置首次读取即带有内嵌数据表
func TestGetGlobalMappingLoadsEmbeddedDefaults(t *testing.T) {
	cfg := GetGlobalMapping()

	if len(cfg.GetFullWidthToHalf()) == 0 {
		t.Fatal("全局映射表默认为空")
	}
	if got := cfg.GetNumberStyle()['①']; got != '1' {
		t.Errorf("① 应映射到 1，实际 %q", got)
	}
	if len(cfg.GetHalfToFullWidth()) == 0 {
		t.Error("半角转全角反向表未生成")
	}
}

// TestSetGlobalMappingTakesEffect 历史缺陷：单例用 sync.Once 初始化，Set 之后再读会被吞掉
func TestSetGlobalMappingTakesEffect(t *testing.T) {
	original := GetGlobalMapping()
	t.Cleanup(func() { SetGlobalMapping(original) })

	custom := NewMappingConfig()
	custom.SetHomophone(map[rune][]rune{'发': {'法'}})
	SetGlobalMapping(custom)

	if got := GetGlobalMapping(); got != custom {
		t.Fatal("SetGlobalMapping 之后读到的不是同一份配置")
	}
	if len(GetGlobalMapping().GetFullWidthToHalf()) != 0 {
		t.Error("自定义配置不应混入内嵌默认表")
	}

	// 传 nil 表示退回内嵌默认表
	SetGlobalMapping(nil)
	if len(GetGlobalMapping().GetFullWidthToHalf()) == 0 {
		t.Error("置 nil 后未退回内嵌默认表")
	}
}

// TestNewDefaultMappingConfigIndependent 默认配置必须复制数据表，避免与内嵌表共享底层 map
func TestNewDefaultMappingConfigIndependent(t *testing.T) {
	cfg := NewDefaultMappingConfig()
	homophones := cfg.GetHomophone()['发']
	if len(homophones) == 0 {
		t.Fatal("默认配置未加载同音字表")
	}

	cfg.SetHomophone(map[rune][]rune{})
	if len(mappingdata.Default().Homophone['发']) == 0 {
		t.Error("内嵌数据表被外部配置修改污染")
	}
}

// TestGlobalMappingConcurrent 并发读写全局配置不应崩溃或数据竞争
func TestGlobalMappingConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = len(GetGlobalMapping().GetPinyin())
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		cfg := NewDefaultMappingConfig()
		for j := 0; j < 50; j++ {
			SetGlobalMapping(cfg)
		}
	}()
	wg.Wait()
}
