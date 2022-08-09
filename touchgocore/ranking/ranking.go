package ranking

import (
	"bytes"
	"encoding/gob"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"
)

// 跳跃表最大层数
const SKIPLIST_MAXLEVEL = 32

// 随机概率
const SKIPLIST_P = 0.25

type RankInfo struct {
	Id        int64
	Val       int64
	Rank      int32
	Timestamp int64
}

func less(a, b int64) bool {
	return a > b
}

func (info1 *RankInfo) cmp(info2 *RankInfo) int {
	if info1.Id == info2.Id {
		return 0
	}
	if info1.Timestamp < info2.Timestamp {
		return -1
	} else if info1.Timestamp > info2.Timestamp {
		return 1
	}
	if info1.Id < info2.Id {
		return -1
	} else if info1.Id > info2.Id {
		return 1
	}
	return 0
}

type skiplistlevel struct {
	Forward *Skiplistnode
	Span    int32
}

type Skiplistnode struct {
	Key   int64
	Val   *RankInfo
	Level []skiplistlevel
}

type Skiplist struct {
	Header *Skiplistnode
	Tail   *Skiplistnode
	Length int32
	Level  int32
}

func newSkiplistnode(level int32, key int64, val *RankInfo) *Skiplistnode {
	node := new(Skiplistnode)
	node.Key = key
	node.Val = val
	node.Level = make([]skiplistlevel, level)
	for i := level - 1; i >= 0; i-- {
		node.Level[i].Forward = nil
	}
	return node
}

func newSkiplist() *Skiplist {
	sl := new(Skiplist)
	sl.Header = newSkiplistnode(SKIPLIST_MAXLEVEL, -1, nil)
	for i := 0; i < SKIPLIST_MAXLEVEL; i++ {
		sl.Header.Level[i].Forward = nil
		sl.Header.Level[i].Span = 0
	}
	sl.Tail = nil
	sl.Level = 1
	return sl
}

func randomLevel() int32 {
	lvl := int32(1)
	rand.Seed(time.Now().UnixNano())
	for rand.Float32() < SKIPLIST_P && lvl < SKIPLIST_MAXLEVEL {
		lvl++
	}
	return lvl
}

func (sl *Skiplist) insert(key int64, val *RankInfo) {
	var update [SKIPLIST_MAXLEVEL]*Skiplistnode
	var rank [SKIPLIST_MAXLEVEL]int32
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		if i == sl.Level-1 {
			rank[i] = 0
		} else {
			rank[i] = rank[i+1]
		}
		for x.Level[i].Forward != nil &&
			(less(x.Level[i].Forward.Key, key) ||
				(x.Level[i].Forward.Key == key &&
					x.Level[i].Forward.Val.cmp(val) < 0)) {
			rank[i] += x.Level[i].Span
			x = x.Level[i].Forward
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

	x = newSkiplistnode(level, key, val)
	for i := int32(0); i < level; i++ {
		x.Level[i].Forward = update[i].Level[i].Forward
		update[i].Level[i].Forward = x

		x.Level[i].Span = update[i].Level[i].Span - (rank[0] - rank[i])
		update[i].Level[i].Span = rank[0] - rank[i] + 1
	}

	for i := level; i < sl.Level; i++ {
		update[i].Level[i].Span++
	}
	sl.Length++
}

func (sl *Skiplist) remove(key int64, val *RankInfo) bool {
	var update [SKIPLIST_MAXLEVEL]*Skiplistnode
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for x.Level[i].Forward != nil &&
			(less(x.Level[i].Forward.Key, key) ||
				(x.Level[i].Forward.Key == key &&
					x.Level[i].Forward.Val.cmp(val) < 0)) {
			x = x.Level[i].Forward
		}
		update[i] = x
	}
	x = x.Level[0].Forward
	if x != nil && x.Key == key && x.Val.cmp(val) == 0 {
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
	return false
}

func (sl *Skiplist) search(key int64, val *RankInfo) bool {
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for x.Level[i].Forward != nil &&
			(less(x.Level[i].Forward.Key, key) ||
				(x.Level[i].Forward.Key == key &&
					x.Level[i].Forward.Val.cmp(val) < 0)) {
			//x.Level[i].Forward.Val.(int) < val.(int))) {
			x = x.Level[i].Forward
		}
	}
	x = x.Level[0].Forward
	return x != nil && x.Key == key && x.Val.cmp(val) == 0
}

func (sl *Skiplist) rank(key int64, val *RankInfo) int32 {
	rank := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for x.Level[i].Forward != nil &&
			(less(x.Level[i].Forward.Key, key) ||
				(x.Level[i].Forward.Key == key &&
					x.Level[i].Forward.Val.cmp(val) < 0)) {
			//x.Level[i].Forward.Val.(int) < val.(int))) {
			rank += x.Level[i].Span
			x = x.Level[i].Forward
		}
	}
	x = x.Level[0].Forward
	if x != nil && x.Key == key && x.Val.cmp(val) == 0 {
		return rank
	}
	return -1
}

func (sl *Skiplist) searchByRank(rank int32) (int64, *RankInfo) {
	visited := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for x.Level[i].Forward != nil && (visited+x.Level[i].Span) <= rank {
			visited += x.Level[i].Span
			x = x.Level[i].Forward
		}
		if visited == rank {
			return x.Key, x.Val
		}
	}
	return -1, nil
}

func (sl *Skiplist) getFirstByRank(rank int32) *Skiplistnode {
	visited := int32(0)
	x := sl.Header
	for i := sl.Level - 1; i >= 0; i-- {
		for x.Level[i].Forward != nil && (visited+x.Level[i].Span) <= rank {
			visited += x.Level[i].Span
			x = x.Level[i].Forward
		}
		if visited == rank {
			return x
		}
	}
	return nil
}

func copyValue(v *RankInfo) *RankInfo {
	val := &RankInfo{
		Id:        v.Id,
		Val:       v.Val,
		Rank:      v.Rank,
		Timestamp: v.Timestamp,
	}
	return val
}

func (sl *Skiplist) searchByRankRange(min, max int32) []*RankInfo {
	res := make([]*RankInfo, 0)
	st := sl.getFirstByRank(min)
	if st == nil {
		return nil
	}

	rank := min
	for i := st; rank <= max && i != nil; i = i.Level[0].Forward {
		i.Val.Rank = rank
		val := copyValue(i.Val)
		res = append(res, val)
		rank++
	}
	return res
}

func (sl *Skiplist) foreach(do func(int64, interface{})) {
	x := sl.Header
	for i := x.Level[0].Forward; i != nil; i = i.Level[0].Forward {
		do(i.Key, i.Val)
	}
}

type RankTree struct {
	Sl *Skiplist
	//EntryMapping map[int64]*RankInfo
	EntryMapping sync.Map
}

func NewRankTree() *RankTree {
	rt := new(RankTree)
	rt.Sl = newSkiplist()
	//rt.EntryMapping = make(map[int64]*RankInfo)
	return rt
}

// 添加新排名信息
func (rt *RankTree) AddRankInfo(uid int64, val int64, timestamp int64) {
	var info *RankInfo
	//if info = rt.EntryMapping[uid]; info != nil {
	if tempV, has := rt.EntryMapping.Load(uid); has {
		info = tempV.(*RankInfo)
		if info.Val == val {
			return
		}
		rt.Sl.remove(info.Val, info)
		info.Val = val
		info.Timestamp = timestamp
	} else {
		info = new(RankInfo)
		info.Id = uid
		info.Val = val
		info.Timestamp = timestamp
	}
	rt.Sl.insert(info.Val, info)
	//rt.EntryMapping[uid] = info
	rt.EntryMapping.Store(uid, info)
}

// 删除排名信息
func (rt *RankTree) RemoveRankInfo(uid int64) bool {
	//if info := rt.EntryMapping[uid]; info != nil {
	if tempV, has := rt.EntryMapping.Load(uid); has {
		info := tempV.(*RankInfo)
		rt.Sl.remove(info.Val, info)
		//delete(rt.EntryMapping, uid)
		rt.EntryMapping.Delete(uid)
		return true
	}
	return false
}

// 更新排名信息
//TODO 名字去掉
func (rt *RankTree) UpdateRankInfo(uid int64, val int64, timestamp int64) {
	// if info := rt.EntryMapping[uid]; info == nil || info.Val != val {
	// 	rt.RemoveRankInfo(uid)
	// 	rt.AddRankInfo(uid, val, timestamp)
	// }

	tempV, has := rt.EntryMapping.Load(uid)
	if has {
		info := tempV.(*RankInfo)
		if info.Val == val {
			return
		}
	}
	rt.RemoveRankInfo(uid)
	rt.AddRankInfo(uid, val, timestamp)
}

// 查询用户排名
func (rt *RankTree) QueryRankInfo(uid int64) *RankInfo {
	var info *RankInfo
	// if info = rt.EntryMapping[uid]; info == nil {
	// 	return nil
	// }
	if tempV, has := rt.EntryMapping.Load(uid); has {
		info = tempV.(*RankInfo)
	} else {
		return nil
	}
	info.Rank = rt.Sl.rank(info.Val, info) + 1
	return info
}

// 查询指定范围排名
func (rt *RankTree) QueryByRankRange(min, max int32) []*RankInfo {
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

// 根据排名查询信息
func (rt *RankTree) QueryByRank(rank int32) *RankInfo {
	key, val := rt.Sl.searchByRank(rank)
	if key < 0 {
		return nil
	}
	return val
}

//获取排名长度
func (rt *RankTree) RankLength() int32 {
	return rt.Sl.Length
}

// 从dump加载排名模块
func LoadRanking(filename string) *RankTree {
	f, err := os.OpenFile(filename, os.O_RDONLY, 0666)
	if err != nil {
		log.Println("Open map file", err.Error())
		return nil
	}
	defer f.Close()
	info, _ := f.Stat()
	raw := make([]byte, info.Size())
	_, err = f.Read(raw)
	if err != nil {
		log.Fatalln("Read map file, err", err.Error())
		return nil
	}
	rt := new(RankTree)
	enc := gob.NewDecoder(bytes.NewReader(raw))
	err = enc.Decode(rt)
	if err != nil {
		log.Fatalln("Load Ranking error:", err.Error())
	}
	return rt
}

// dump排名模块
func SaveRanking(rt *RankTree, filename string) bool {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalln(err.Error())
		return false
	}
	defer f.Close()
	buffer := new(bytes.Buffer)
	enc := gob.NewEncoder(buffer)
	err = enc.Encode(rt)
	if err != nil {
		log.Println("Dump encode err:", err.Error())
		return false
	}
	f.Write(buffer.Bytes())
	return true
}

type DbRankInfo struct {
	Type      int64
	Id        int64
	Val       int64
	Timestamp int64
	TempName  string
}

var (
	RTS      map[int64]*RankTree
	rts_lock sync.RWMutex
)

// 从dump加载排名模块
func LoadRankTrees(infos []DbRankInfo) map[int64]*RankTree {
	// construct ranktrees
	rts := make(map[int64]*RankTree)
	var rt *RankTree
	for _, info := range infos {
		rt = rts[info.Type]
		if rt == nil {
			rt = NewRankTree()
			rts[info.Type] = rt
		}
		rt.UpdateRankInfo(info.Id, info.Val, info.Timestamp)
	}
	return rts
}

// dump排名模块
func saveRankTrees(rts map[int64]*RankTree) []DbRankInfo {
	infos := make([]DbRankInfo, 0)
	rts_lock.RLock()
	for Type, rt := range rts {
		rt.EntryMapping.Range(func(key, tempV interface{}) bool {
			entry := tempV.(*RankInfo)
			info := DbRankInfo{
				Type:      Type,
				Id:        entry.Id,
				Val:       entry.Val,
				Timestamp: entry.Timestamp,
			}
			infos = append(infos, info)
			return true
		})
		// for _, entry := range rt.EntryMapping {
		// 	info := DbRankInfo{
		// 		Type:      Type,
		// 		Id:        entry.Id,
		// 		Val:       entry.Val,
		// 		Timestamp: entry.Timestamp,
		// 	}
		// 	infos = append(infos, info)
		// }
	}
	rts_lock.RUnlock()
	return infos
}

func Load(infos []DbRankInfo) {
	RTS = LoadRankTrees(infos)
	// if RTS == nil {
	// 	RTS = make(map[int16]*RankTree)
	// 	saveRankTrees(RTS)
	// }
}

func Save() []DbRankInfo {
	return saveRankTrees(RTS)
}

func GetRankTree(rtype int64) *RankTree {
	rt, ok := RTS[rtype]
	if !ok {
		rt = NewRankTree()
		RTS[rtype] = rt
	}
	return rt
}

//获取所有的排行榜
func GetAllRankTree() *map[int64]*RankTree {
	return &RTS
}

func HasRankTree(rtype int64) bool {
	_, ok := RTS[rtype]
	if !ok {
		return false
	}

	return true
}

func ResetRankTree(rtype int64) {
	delete(RTS, rtype)
	RTS[rtype] = NewRankTree()
}
