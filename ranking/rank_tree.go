package ranking

import (
	"sync"

	"touchgocore/syncmap"
)

// RankInfo 排名信息结构体
type RankInfo struct {
	ID        int64
	Value     int64
	Rank      int32
	Timestamp int64
}

// Compare 比较两个RankInfo对象
func (info1 *RankInfo) Compare(info2 *RankInfo) int {
	if info1.ID == info2.ID {
		return 0
	}
	if info1.Timestamp < info2.Timestamp {
		return -1
	} else if info1.Timestamp > info2.Timestamp {
		return 1
	}
	if info1.ID < info2.ID {
		return -1
	} else if info1.ID > info2.ID {
		return 1
	}
	return 0
}

// RankTree 基于跳跃表的排名树
type RankTree struct {
	Sl *SkipList
	//EntryMapping map[int64]*RankInfo
	EntryMapping *syncmap.Map[int64, *RankInfo]
	// 读写锁，保护跳跃表的并发访问
	rwMutex sync.RWMutex
}

func NewRankTree() *RankTree {
	rt := new(RankTree)
	rt.ensure()
	return rt
}

func (rt *RankTree) ensure() {
	if rt.EntryMapping == nil {
		rt.EntryMapping = syncmap.NewMap[int64, *RankInfo]()
	}
	if rt.Sl == nil || rt.Sl.Header == nil {
		rt.Sl = newSkipList()
	}
}

// 添加新排名信息
func (rt *RankTree) AddRankInfo(uid int64, val int64, timestamp int64) {
	// 写操作，需要互斥锁
	rt.rwMutex.Lock()
	defer rt.rwMutex.Unlock()
	rt.ensure()

	var info *RankInfo
	if loaded, has := rt.EntryMapping.Load(uid); has {
		info = loaded
		if info.Value == val {
			return // 相同值，不需要更新
		}
		// 先删除旧值，再更新
		rt.Sl.remove(info.Value, info)
		info.Value = val
		info.Timestamp = timestamp
	} else {
		// 只在需要时创建新对象
		info = &RankInfo{
			ID:        uid,
			Value:     val,
			Timestamp: timestamp,
		}
	}
	rt.Sl.insert(info.Value, info)
	rt.EntryMapping.Store(uid, info)
}

// 删除排名信息
func (rt *RankTree) RemoveRankInfo(uid int64) bool {
	// 写操作，需要互斥锁
	rt.rwMutex.Lock()
	defer rt.rwMutex.Unlock()
	rt.ensure()

	if info, has := rt.EntryMapping.Load(uid); has {
		rt.Sl.remove(info.Value, info)
		rt.EntryMapping.Delete(uid)
		return true
	}
	return false
}

// 更新排名信息
func (rt *RankTree) UpdateRankInfo(uid int64, val int64, timestamp int64) {
	// 直接调用AddRankInfo，它已经包含了更新逻辑
	rt.AddRankInfo(uid, val, timestamp)
}

// 查询用户排名。
//
// 返回值是内部条目的【值拷贝】：
//  1. 读锁下绝不写共享的 *RankInfo（旧实现 info.Rank = ... 与并发查询/持久化
//     构成数据竞争），Rank 只写在副本上；
//  2. 不外泄内部指针，调用方修改返回值不会破坏树内状态。
func (rt *RankTree) QueryRankInfo(uid int64) *RankInfo {
	if rt == nil {
		return nil
	}
	// 读操作，需要读锁
	rt.rwMutex.RLock()
	defer rt.rwMutex.RUnlock()
	if rt.Sl == nil || rt.EntryMapping == nil {
		return nil
	}

	info, has := rt.EntryMapping.Load(uid)
	if !has {
		return nil
	}
	rank := rt.Sl.rank(info.Value, info)
	cp := *info
	cp.Rank = rank + 1
	return &cp
}

// 查询指定范围排名（返回元素为值拷贝，见 searchByRankRange）
func (rt *RankTree) QueryByRankRange(min, max int32) []*RankInfo {
	if rt == nil {
		return nil
	}
	// 读操作，需要读锁
	rt.rwMutex.RLock()
	defer rt.rwMutex.RUnlock()

	if rt.Sl == nil {
		return nil
	}
	if min > max {
		return nil
	}
	if min <= 0 {
		min = 1
	}
	if max > rt.Sl.Length {
		max = rt.Sl.Length
	}
	return rt.Sl.searchByRankRange(min, max)
}

// 根据排名查询信息（返回值拷贝，避免外泄内部指针）
func (rt *RankTree) QueryByRank(rank int32) *RankInfo {
	if rt == nil {
		return nil
	}
	// 读操作，需要读锁
	rt.rwMutex.RLock()
	defer rt.rwMutex.RUnlock()

	if rt.Sl == nil {
		return nil
	}
	key, val := rt.Sl.searchByRank(rank)
	if key < 0 || val == nil {
		return nil
	}
	cp := *val
	return &cp
}

// 获取排名长度
func (rt *RankTree) RankLength() int32 {
	if rt == nil {
		return 0
	}
	// 读操作，需要读锁
	rt.rwMutex.RLock()
	defer rt.rwMutex.RUnlock()

	if rt.Sl == nil {
		return 0
	}
	return rt.Sl.Length
}
