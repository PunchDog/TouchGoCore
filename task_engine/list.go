package taskengine

import (
	"context"
	"strings"
	"time"

	"touchgocore/util"
)

// GetTask 查询会员任务。
//
// 任务对象常驻内存（Reload 时已合并本周期进度），返回的就是引擎索引里的对象本身，
// 其链表归属即当前状态。
// taskID > 0 返回单条（1 元素切片）；定义不存在返回 ErrDefMissing，
// 未启用返回 ErrDisabled，窗口外返回空切片。
// taskID == 0 返回 tasklist 上的全部可见任务（claimed 只在 claimedtasklist，故不在此列）；
// group 非空时只返回该分组。
func (e *Engine) GetTask(ctx context.Context, taskID int64, group string) ([]*TaskProgress, error) {
	now := util.CurrentTime()
	// 读之前先把跨周期的终态任务复活，避免昨天的 claimed 永远占着终态链。
	e.rollCycle(now)

	// 单条查询。
	if taskID > 0 {
		t := e.teak_progress(taskID)
		if t != nil {
			def := t.GetDef()
			if def == nil {
				return nil, ErrDefMissing
			}
			if !defInWindow(def, now) {
				return []*TaskProgress{}, nil
			}
			return []*TaskProgress{t}, nil
		}
		// 常驻索引里没有（Reload 后新增或已停用）：回源定义区分缺失 / 停用 / 窗口外。
		def, err := loadDef(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if def == nil {
			return nil, ErrDefMissing
		}
		if def.Enabled != 1 {
			return nil, ErrDisabled
		}
		if !defInWindow(def, now) {
			return []*TaskProgress{}, nil
		}
		// 定义可用但尚未常驻：合成一个临时对象，进度按本周期回源。
		cur, err := e.progressOf(ctx, def, now)
		if err != nil {
			return nil, err
		}
		return []*TaskProgress{tempTask(def, e.memberID, cur, now)}, nil
	}

	// 列表查询：遍历全部任务链。
	group = strings.TrimSpace(group)
	out := make([]*TaskProgress, 0)
	for _, t := range tasksOf(e.all_task_progress()) {
		def := t.GetDef()
		if def == nil || !defInWindow(def, now) {
			continue
		}
		if group != "" && def.Group != group {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// progressOf 读某任务本周期的进度行。
func (e *Engine) progressOf(ctx context.Context, def *TaskDef, now time.Time) (*TaskProgress, error) {
	cycle := TaskCycleKey(def.Cycle, now, def.ID)
	return loadProgressRow(ctx, e.memberID, def.ID, cycle)
}
