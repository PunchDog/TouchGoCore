package localtimer

// ============================================================================
// B-F4（槽位B）：管理器路径 GetQueueStats 在通道数组为空时必须报容量 0。
//
// 缺陷形态：TimeStop 释放通道后（或未 Run 时）调用 mgr.GetQueueStats()，
// shardCapFor(0) 被钳成 1 片 → ScheduleShardCap=100000，与门面路径
// GetQueueStats「未 Run 容量统一为 0」的口径矛盾：depth/capacity 算出 0%，
// 监控看着像健康运行而不是停摆。
// ============================================================================

import (
	"testing"
)

// TestQaBQueueStatsCapZeroWithoutChannels 无通道时管理器路径与门面路径口径一致：
// ShardCap/Cap 均为 0；通道存在时仍按片数均分。
func TestQaBQueueStatsCapZeroWithoutChannels(t *testing.T) {
	mgr := NewTimerManager() // 内部 ensureTimerChannel，通道必然已建
	defer mgr.Close()

	// 基线：通道存在时按当前片数均分
	withCh := mgr.GetQueueStats()
	channels := currentTimerChannels()
	if len(channels) == 0 {
		t.Fatal("✘ 前置条件：NewTimerManager 之后通道数组不应为空")
	}
	if want := int(shardCapFor(len(channels))); withCh.ScheduleShardCap != want {
		t.Fatalf("✘ 有通道时单片容量不对: got=%d want=%d", withCh.ScheduleShardCap, want)
	}
	if withCh.ScheduleShardCap <= 0 {
		t.Fatalf("✘ 有通道时单片容量必须为正: %d", withCh.ScheduleShardCap)
	}

	// 释放通道（TimeStop 之后的形态），统计必须整组归零
	resetTimerChannel()
	defer ensureTimerChannel() // 恢复全局状态，避免污染同包其它用例

	noCh := mgr.GetQueueStats()
	if noCh.ScheduleShardCap != 0 {
		t.Fatalf("✘ 无通道时单片容量未归零（shardCapFor(0) 钳成 1 片的理论值）: %d", noCh.ScheduleShardCap)
	}
	if noCh.ScheduleCap != 0 || noCh.ScheduleLen != 0 {
		t.Fatalf("✘ 无通道时总量口径不为零: cap=%d len=%d", noCh.ScheduleCap, noCh.ScheduleLen)
	}
	if len(noCh.ScheduleShardLen) != 0 {
		t.Fatalf("✘ 无通道时逐片序列应为空: %v", noCh.ScheduleShardLen)
	}
	// 轮的序列仍要给足长度（Prometheus 序列缺失 ≠ 取值 0）
	if len(noCh.WheelLen) != DefaultWheelCount || len(noCh.WheelCap) != DefaultWheelCount {
		t.Fatalf("✘ 轮序列长度丢失: len=%d cap=%d", len(noCh.WheelLen), len(noCh.WheelCap))
	}
}
