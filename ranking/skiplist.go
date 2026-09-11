package ranking

import (
	"touchgocore/random"
)

// 跳跃表最大层数
const SkipListMaxLevel = 32

// 随机概率
const SkipListProbability = 250

// 比较函数，用于降序排序（值大的在前面）
func compareDesc(a, b int64) bool {
	return a > b
}

// 比较函数，用于跳跃表的降序排序（值大的在前面）
// 跳跃表插入时，会寻找第一个不小于新节点的位置
func less(a, b int64) bool {
	return a < b
}

// SkipListLevel 跳跃表层结构
type SkipListLevel struct {
	Forward *SkipListNode
	Span    int32
}

// SkipListNode 跳跃表节点
type SkipListNode struct {
	Key   int64
	Value *RankInfo
	Level []SkipListLevel
}

// SkipList 跳跃表结构
type SkipList struct {
	Header *SkipListNode
	Tail   *SkipListNode
	Length int32
	Level  int32
}

func newSkipListNode(level int32, key int64, val *RankInfo) *SkipListNode {
	node := new(SkipListNode)
	node.Key = key
	node.Value = val
	node.Level = make([]SkipListLevel, level)
	for i := level - 1; i >= 0; i-- {
		node.Level[i].Forward = nil
	}
	return node
}

func newSkipList() *SkipList {
	sl := new(SkipList)
	sl.Header = newSkipListNode(SkipListMaxLevel, -1, nil)
	for i := 0; i < SkipListMaxLevel; i++ {
		sl.Header.Level[i].Forward = nil
		sl.Header.Level[i].Span = 0
	}
	sl.Tail = nil
	sl.Level = 1
	return sl
}

func randomLevel() int32 {
	lvl := int32(1)
	for random.NextInt64()%10000 < SkipListProbability && lvl < SkipListMaxLevel {
		lvl++
	}
	return lvl
}

func (sl *SkipList) insert(key int64, val *RankInfo) {
	if sl == nil || sl.Header == nil || val == nil {
		return
	}
	var update [SkipListMaxLevel]*SkipListNode
	var rank [SkipListMaxLevel]int32
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		if i == sl.Level-1 {
			rank[i] = 0
		} else {
			rank[i] = rank[i+1]
		}
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			// 优化比较逻辑，减少函数调用
			forwardKey := forward.Key
			// 实现降序排序：值大的排在前面
			if forwardKey > key {
				rank[i] += x.Level[i].Span
				x = forward
				continue
			}
			if forwardKey < key {
				break
			}
			// 相同key的情况，比较时间戳和ID
			forwardVal := forward.Value
			if forwardVal.Timestamp < val.Timestamp ||
				(forwardVal.Timestamp == val.Timestamp && forwardVal.ID < val.ID) {
				rank[i] += x.Level[i].Span
				x = forward
				continue
			}
			break
		}
		update[i] = x
	}

	level := randomLevel()
	if level > sl.Level {
		for i := sl.Level; i < level; i++ {
			rank[i] = 0
			update[i] = sl.Header
			update[i].Level[i].Span = sl.Length
		}
		sl.Level = level
	}

	x = newSkipListNode(level, key, val)
	for i := int32(0); i < level; i++ {
		forward := update[i].Level[i].Forward
		x.Level[i].Forward = forward
		update[i].Level[i].Forward = x

		// 优化跨度计算，减少算术运算
		origSpan := update[i].Level[i].Span
		newRank := rank[0] - rank[i] + 1
		x.Level[i].Span = origSpan - newRank + 1
		update[i].Level[i].Span = newRank
	}

	for i := level; i < sl.Level; i++ {
		update[i].Level[i].Span++
	}
	sl.Length++
}

func (sl *SkipList) remove(key int64, val *RankInfo) bool {
	var update [SkipListMaxLevel]*SkipListNode
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			forwardKey := forward.Key
			// 实现降序排序：值大的排在前面
			if forwardKey > key {
				x = forward
				continue
			}
			if forwardKey < key {
				break
			}
			// 相同key的情况，比较时间戳和ID
			forwardVal := forward.Value
			if forwardVal.Timestamp < val.Timestamp ||
				(forwardVal.Timestamp == val.Timestamp && forwardVal.ID < val.ID) {
				x = forward
				continue
			}
			break
		}
		update[i] = x
	}
	x = x.Level[0].Forward
	if x != nil && x.Key == key {
		// 优化比较逻辑，减少函数调用
		xVal := x.Value
		if xVal.Timestamp == val.Timestamp && xVal.ID == val.ID {
			// delete node
			for i := int32(0); i < sl.Level; i++ {
				if update[i].Level[i].Forward == x {
					update[i].Level[i].Span += x.Level[i].Span - 1
					update[i].Level[i].Forward = x.Level[i].Forward
				} else {
					update[i].Level[i].Span--
				}
			}

			for sl.Level > 1 && sl.Header.Level[sl.Level-1].Forward == nil {
				sl.Level--
			}
			sl.Length--
			return true
		}
	}
	return false
}

func (sl *SkipList) search(key int64, val *RankInfo) bool {
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			forwardKey := forward.Key
			// 实现降序排序：值大的排在前面
			if forwardKey > key {
				x = forward
				continue
			}
			if forwardKey < key {
				break
			}
			// 相同key的情况，比较时间戳和ID
			forwardVal := forward.Value
			if forwardVal.Timestamp < val.Timestamp ||
				(forwardVal.Timestamp == val.Timestamp && forwardVal.ID < val.ID) {
				x = forward
				continue
			}
			break
		}
	}
	x = x.Level[0].Forward
	return x != nil && x.Key == key &&
		x.Value.Timestamp == val.Timestamp &&
		x.Value.ID == val.ID
}

func (sl *SkipList) rank(key int64, val *RankInfo) int32 {
	rank := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			forwardKey := forward.Key
			// 实现降序排序：值大的排在前面
			if forwardKey > key {
				rank += x.Level[i].Span
				x = forward
				continue
			}
			if forwardKey < key {
				break
			}
			// 相同key的情况，比较时间戳和ID
			forwardVal := forward.Value
			if forwardVal.Timestamp < val.Timestamp ||
				(forwardVal.Timestamp == val.Timestamp && forwardVal.ID < val.ID) {
				rank += x.Level[i].Span
				x = forward
				continue
			}
			break
		}
	}
	x = x.Level[0].Forward
	if x != nil && x.Key == key &&
		x.Value.Timestamp == val.Timestamp &&
		x.Value.ID == val.ID {
		return rank
	}
	return -1
}

func (sl *SkipList) searchByRank(rank int32) (int64, *RankInfo) {
	visited := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			newVisited := visited + x.Level[i].Span
			if newVisited > rank {
				break
			}
			visited = newVisited
			x = forward
			if visited == rank {
				return x.Key, x.Value
			}
		}
	}
	return -1, nil
}

func (sl *SkipList) getFirstByRank(rank int32) *SkipListNode {
	visited := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for {
			forward := x.Level[i].Forward
			if forward == nil {
				break
			}
			newVisited := visited + x.Level[i].Span
			if newVisited > rank {
				break
			}
			visited = newVisited
			x = forward
			if visited == rank {
				return x
			}
		}
	}
	return nil
}

func copyValue(v *RankInfo) *RankInfo {
	val := &RankInfo{
		ID:        v.ID,
		Value:     v.Value,
		Rank:      v.Rank,
		Timestamp: v.Timestamp,
	}
	return val
}

func (sl *SkipList) searchByRankRange(min, max int32) []*RankInfo {
	// 计算结果数量，预分配切片容量
	rangeSize := max - min + 1
	res := make([]*RankInfo, 0, rangeSize)
	st := sl.getFirstByRank(min)
	if st == nil {
		return res
	}

	rank := min
	for i := st; rank <= max && i != nil; i = i.Level[0].Forward {
		i.Value.Rank = rank
		val := copyValue(i.Value)
		res = append(res, val)
		rank++
	}
	return res
}

func (sl *SkipList) foreach(do func(int64, interface{})) {
	x := sl.Header
	for i := x.Level[0].Forward; i != nil; i = i.Level[0].Forward {
		do(i.Key, i.Value)
	}
}
