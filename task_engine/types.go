package taskengine

import (
	"fmt"
	"strings"
	"time"

	"touchgocore/list"
)

// 周期：何时重置进度。
const (
	// CycleOnce 一次性任务，永不复位。
	CycleOnce = "ONCE"
	// CycleDaily 每日任务，UTC 跨日复位。
	CycleDaily = "DAILY"
	// CycleWeekly 每周任务，ISO 周（周一 00:00 UTC）复位。
	CycleWeekly = "WEEKLY"
	// CycleScheduled 投放窗口驱动的任务，cycle_key 恒为 sched:{id}。
	CycleScheduled = "SCHEDULED"
)

// 发放模式决定达标后落哪个状态，以及引擎是否调用 Options.rewardFn：
// AUTO 先发奖再落 claimed，CLAIM 只落 completed 等宿主自行发放。
const (
	// GrantAuto 达标时引擎调用 rewardFn，随后落 claimed。
	GrantAuto = "AUTO"
	// GrantClaim 达标只落 completed，引擎不发奖。
	GrantClaim = "CLAIM"
)

// 进度状态。
const (
	// ProgressActive 进行中。
	ProgressActive = "active"
	// ProgressCompleted 已达成、未结算。
	ProgressCompleted = "completed"
	// ProgressClaimed 已结算（AUTO 达成时写入）。
	ProgressClaimed = "claimed"
)

// 进度口径。
const (
	// ProgressTypeInvite ProgressParmInvite 直接覆盖进度。
	ProgressTypeInvite = "INVITE"
	// ProgressTypeAdd ProgressParmAdd 累加到进度。
	ProgressTypeAdd = "ADD"
)

// TargetCount 对比条件。
const (
	// TargetCountTypeGT progress > target。
	TargetCountTypeGT = ">"
	// TargetCountTypeEQ progress == target。
	TargetCountTypeEQ = "="
	// TargetCountTypeLT progress < target。
	TargetCountTypeLT = "<"
)

// ProgressParm 一次进度上报的参数。
type ProgressParm struct {
	// Trigger 事件触发点；OnEvent 用它扇出。
	Trigger string
	// ProgressParmInvite 全局数据覆盖值。
	ProgressParmInvite int64
	// ProgressParmAdd 累计增量。
	ProgressParmAdd int64
}

// TaskDef 对应表 task_def。
//
// Trigger 决定 OnEvent 扇出；ProgressType 决定进度口径；TargetCount 是达成阈值。
type TaskDef struct {
	// ID 主键，自增；任务定义以此唯一定位。
	ID int64 `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	// Title 会员端展示标题。
	Title string `gorm:"column:title" json:"title"`
	// Desc 说明文案，原样返回。
	Desc string `gorm:"column:desc" json:"desc"`
	// Trigger 事件触发点。OnEvent 按它扇出。
	Trigger string `gorm:"column:trigger" json:"trigger"`
	// Cycle 周期：ONCE/DAILY/WEEKLY/SCHEDULED，决定 cycle_key 与重置时机。
	Cycle string `gorm:"column:cycle" json:"cycle"`
	// GrantMode 结算模式：AUTO 达成即落 claimed，CLAIM 落 completed。
	GrantMode string `gorm:"column:grant_mode" json:"grantMode"`
	// ProgressType 进度口径：INVITE 用 ProgressParmInvite 覆盖，ADD 用 ProgressParmAdd 累加。
	ProgressType string `gorm:"column:progress_type" json:"progressType"`
	// Reward 奖励配置（JSON），引擎不解析，由宿主 reward 函数消费。
	Reward string `gorm:"column:reward_json" json:"reward_json"`
	// TargetCount 达成阈值：与 Progress 按 TargetCountType 对比；<=0 按 1。
	TargetCount float64 `gorm:"column:target_count" json:"targetCount"`
	// TargetCountType 对比条件：> / = / <；空按 =。
	TargetCountType string `gorm:"column:target_count_type" json:"targetCountType"`
	// CycleLimit 保留字段；完成阈值以 TargetCount 为准。
	CycleLimit int64 `gorm:"column:cycle_limit" json:"cycleLimit"`
	// Enabled 1=启用；GetTask 与 OnEvent 只处理启用行。
	Enabled int8 `gorm:"column:enabled" json:"enabled"`
	// StartAt 投放开始时间（UTC）；早于它不推进也不展示。
	StartAt *time.Time `gorm:"column:start_at" json:"startAt"`
	// EndAt 投放结束时间（UTC）；晚于它不推进也不展示。
	EndAt *time.Time `gorm:"column:end_at" json:"endAt"`
	// PreTaskID 前置任务 ID，0=无；前置本周期 completed/claimed 才放行。
	PreTaskID int64 `gorm:"column:pre_task_id" json:"preTaskId"`
	// Group 列表分组标签，空=未分组。
	Group string `gorm:"column:group" json:"group"`
	// ClearOnCycle 保留字段；引擎按 cycle_key 自然隔离，不消费此列。
	ClearOnCycle int8 `gorm:"column:clear_on_cycle" json:"clearOnCycle"`
	// NextDayClaim 1=只能领「前一天完成」的那一份；仅 CLAIM+DAILY 有意义，引擎不读取。
	NextDayClaim int8 `gorm:"column:next_day_claim" json:"nextDayClaim"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	// UpdatedAt 最后更新时间。
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`
}

// TableName 表名。
func (TaskDef) TableName() string { return "task_def" }

// TaskProgress 对应表 task_progress。
//
// 每个 (member,task,cycle) 一行；同一个 key 取 id 最大的一行即最新状态。
// AUTO 达标写 claimed，CLAIM 达标写 completed；两者都是终态，不再降级。
type TaskProgress struct {
	// ID 主键，自增；同键多行时 id 最大的一行即最新状态。
	ID int64 `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	// MemberID 会员 ID。
	MemberID int64 `gorm:"column:member_id" json:"memberId"`
	// TaskID 对应的 task_def.id。
	TaskID int64 `gorm:"column:task_id" json:"taskId"`
	// CycleKey 周期片，由 TaskCycleKey 生成（once / 日期 / ISO 周 / sched:{id}）。
	CycleKey string `gorm:"column:cycle_key" json:"cycleKey"`
	// Progress 当前进度值；由 ProgressParm 按 ProgressType 计算。
	Progress int64 `gorm:"column:progress" json:"progress"`
	// Status 状态：active / completed / claimed。
	Status string `gorm:"column:status" json:"status"`
	// SnapshotJSON 最近一次 ProgressParm 的 JSON 快照，仅供宿主排查。
	SnapshotJSON string `gorm:"column:snapshot_json" json:"snapshotJson"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	// UpdatedAt 最后更新时间。
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`

	// tasknode 挂入 Engine.tasklist 的节点（非列字段）。
	tasknode list.INode `gorm:"-" json:"-"`
	// triggernode 挂入 Engine.triggerlist 的节点（非列字段）。
	triggernode list.INode `gorm:"-" json:"-"`
	// complete 已经完成的任务
	completenode list.INode `gorm:"-" json:"-"`
	// 已领取的任务
	claimednode list.INode `gorm:"-" json:"-"`
}

// 获取任务配置
func (self *TaskProgress) GetDef() *TaskDef {
	if d, h := taskglobal.getDef(self.TaskID); h {
		return d
	}
	return nil
}

// TableName 表名。
func (TaskProgress) TableName() string { return "task_progress" }

// TaskProgressDone 前置闸 / 完成通知 / 限次共用：completed 与 claimed 都算已结束。
func TaskProgressDone(status string) bool {
	return status == ProgressCompleted || status == ProgressClaimed
}

// TaskCycleKey 进度唯一键中的周期片。
//
//	ONCE      → "once"
//	DAILY     → YYYY-MM-DD（UTC）
//	WEEKLY    → YYYY-Www（ISO 周，周一 00:00 UTC）
//	SCHEDULED → sched:{taskID}
func TaskCycleKey(cycle string, now time.Time, taskID int64) string {
	now = now.UTC()
	switch cycle {
	case CycleOnce:
		return "once"
	case CycleDaily:
		return now.Format("2006-01-02")
	case CycleWeekly:
		y, w := StartOfWeekUTC(now).ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case CycleScheduled:
		return fmt.Sprintf("sched:%d", taskID)
	default:
		return now.Format("2006-01-02")
	}
}

// StartOfWeekUTC 本周一 00:00 UTC。
func StartOfWeekUTC(t time.Time) time.Time {
	t = t.UTC()
	// d 为当天 00:00（UTC）。
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	// off 为距本周一的天数偏移（Sunday=0 需回退 6 天）。
	off := int(d.Weekday() - time.Monday)
	if off < 0 {
		off += 7
	}
	return d.AddDate(0, 0, -off)
}

// EffectiveTargetCount 达成阈值；<=0 按 1。
func EffectiveTargetCount(n float64) float64 {
	// 未配置或非法阈值按 1，避免任务永不达成或除零语义。
	if n < 1 {
		return 1
	}
	return n
}

// TargetReached 按对比条件判断进度是否达成；空/未知按 "="。
func TargetReached(progress int64, target float64, op string) bool {
	target = EffectiveTargetCount(target)
	got := float64(progress)
	switch strings.TrimSpace(op) {
	case TargetCountTypeGT:
		// 严格大于。
		return got > target
	case TargetCountTypeLT:
		// 严格小于。
		return got < target
	default:
		// 空值与未知值统一按「等于目标」处理。
		return got == target
	}
}
