package taskengine

import (
	"context"
	"sort"
	"time"
)

// fakeStore 内存版 Store，按「每周期一行」语义实现，供引擎单测使用。
type fakeStore struct {
	defs   map[int64]*TaskDef
	rows   []TaskProgress
	nextID int64
}

// newFakeStore 用给定定义构造内存存储。
func newFakeStore(defs ...*TaskDef) *fakeStore {
	f := &fakeStore{defs: map[int64]*TaskDef{}, nextID: 1}
	for _, d := range defs {
		f.defs[d.ID] = d
	}
	return f
}

// allocID 分配下一个自增行 id。
func (f *fakeStore) allocID() int64 {
	id := f.nextID
	f.nextID++
	return id
}

// FindDefByID 按 id 返回定义副本；不存在返回 (nil, nil)。
func (f *fakeStore) FindDefByID(_ context.Context, id int64) (*TaskDef, error) {
	if d := f.defs[id]; d != nil {
		cp := *d
		return &cp, nil
	}
	return nil, nil
}

// listDefs 过滤启用定义并按 id 升序返回。
func (f *fakeStore) listDefs(pred func(*TaskDef) bool) []TaskDef {
	out := make([]TaskDef, 0)
	for _, d := range f.defs {
		if d.Enabled == 1 && pred(d) {
			out = append(out, *d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListEnabledDefs 返回全部启用定义。
func (f *fakeStore) ListEnabledDefs(context.Context) ([]TaskDef, error) {
	return f.listDefs(func(*TaskDef) bool { return true }), nil
}

// HasDoneProgress 判断该任务本周期是否已有终态行。
func (f *fakeStore) HasDoneProgress(_ context.Context, memberID, taskID int64, cycleKey string) (bool, error) {
	for i := range f.rows {
		p := &f.rows[i]
		if p.MemberID == memberID && p.TaskID == taskID && p.CycleKey == cycleKey && TaskProgressDone(p.Status) {
			return true, nil
		}
	}
	return false, nil
}

// ListProgressByMember 返回会员全部进度行。
func (f *fakeStore) ListProgressByMember(_ context.Context, memberID int64) ([]TaskProgress, error) {
	out := make([]TaskProgress, 0)
	for i := range f.rows {
		if f.rows[i].MemberID == memberID {
			out = append(out, f.rows[i])
		}
	}
	return out, nil
}

// LoadProgress 返回同周期最新一行。
func (f *fakeStore) LoadProgress(_ context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error) {
	return f.latest(memberID, taskID, cycleKey), nil
}

// SaveProgress 直接 upsert，模拟缓存层定时回写。
func (f *fakeStore) SaveProgress(_ context.Context, p *TaskProgress) error {
	f.upsert(p, time.Now().UTC())
	return nil
}

// WithTx 提供回滚语义：回调出错时恢复进度行与自增计数。
func (f *fakeStore) WithTx(_ context.Context, fn func(tx Tx) error) error {
	rowsSnapshot := append([]TaskProgress(nil), f.rows...)
	nextSnapshot := f.nextID
	if err := fn(&fakeTx{store: f}); err != nil {
		f.rows = rowsSnapshot
		f.nextID = nextSnapshot
		return err
	}
	return nil
}

// latest 取同键 id 最大的一行副本；无行返回 nil。
func (f *fakeStore) latest(memberID, taskID int64, cycleKey string) *TaskProgress {
	var out *TaskProgress
	for i := range f.rows {
		p := &f.rows[i]
		if p.MemberID == memberID && p.TaskID == taskID && p.CycleKey == cycleKey {
			if out == nil || p.ID > out.ID {
				out = p
			}
		}
	}
	if out == nil {
		return nil
	}
	cp := *out
	return &cp
}

// upsert 按 (member, task, cycle) 有行更新、无行插入。
func (f *fakeStore) upsert(p *TaskProgress, now time.Time) {
	for i := range f.rows {
		r := &f.rows[i]
		if r.MemberID == p.MemberID && r.TaskID == p.TaskID && r.CycleKey == p.CycleKey {
			r.Progress = p.Progress
			r.Status = p.Status
			r.SnapshotJSON = p.SnapshotJSON
			r.UpdatedAt = now
			return
		}
	}
	p.ID = f.allocID()
	p.CreatedAt = now
	p.UpdatedAt = now
	f.rows = append(f.rows, *p)
}

// fakeTx 内存事务：直接读写 fakeStore。
type fakeTx struct {
	store *fakeStore
}

// LoadProgress 事务内读取同周期最新一行。
func (t *fakeTx) LoadProgress(_ context.Context, memberID, taskID int64, cycleKey string) (*TaskProgress, error) {
	return t.store.latest(memberID, taskID, cycleKey), nil
}

// UpsertProgress 事务内 upsert 进度行。
func (t *fakeTx) UpsertProgress(_ context.Context, p *TaskProgress, now time.Time) error {
	t.store.upsert(p, now)
	return nil
}
