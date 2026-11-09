package taskengine

import (
	"context"
	"fmt"
)

// RewardKey 是一次 AUTO 发奖的幂等键，由框架在达标时显式构造。
//
// 宿主应把 String() 的返回值作为发奖流水 / 去重表的唯一键：
// 同一 (会员, 任务, 周期) 无论回调被触发多少次都只发放一次。
// 字段与 TaskProgress 的 MemberID / TaskID / CycleKey 一一对应。
type RewardKey struct {
	// MemberID 会员 ID。
	MemberID int64
	// TaskID 任务定义 ID。
	TaskID int64
	// CycleKey 周期片，由 TaskCycleKey 生成。
	CycleKey string
}

// String 返回稳定去重键 "<MemberID>:<TaskID>:<CycleKey>"。
//
// 该字符串只用于幂等去重，不是缓存行键；调用方不要解析它反推字段。
func (k RewardKey) String() string {
	return fmt.Sprintf("%d:%d:%s", k.MemberID, k.TaskID, k.CycleKey)
}

// RewardFn 是 AUTO 达标时的发奖回调类型，由宿主通过 Options.RewardFn 注入。
//
// 约定：
//   - 只有 GrantMode == AUTO 且本次判定达标时回调一次，OnEvent 与 UpdateTask 两条路径一致；
//   - key 是框架盖章的幂等键，必须以 key.String() 去重，回调本身必须幂等；
//   - p 是即将落库的任务对象，奖励配置读 p.GetDef().Reward；
//   - 回调在 e.mu 与事务行锁内执行，禁止在回调内再次调用引擎处理同一任务；
//     远程发奖会拉长锁持有时间，用 ctx 施加超时 / 取消；
//   - 无返回值：applyProgress 在回调前已把 p.Status 置为 claimed，
//     让回调返回 error 并阻断落库会造成「内存终态 + DB 未落库 + 不再重发奖」的新不一致。
type RewardFn func(ctx context.Context, key RewardKey, p *TaskProgress)
