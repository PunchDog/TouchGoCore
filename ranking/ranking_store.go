package ranking

import (
	"encoding/gob"
	"log"
	"os"
	"sync"
)

// ==================== 排名持久化（gob dump） ====================

// 从dump加载排名模块
func LoadRanking(filename string) *RankTree {
	f, err := os.OpenFile(filename, os.O_RDONLY, 0666)
	if err != nil {
		log.Println("Open map file", err.Error())
		return nil
	}
	defer f.Close()

	// 创建gob解码器
	dec := gob.NewDecoder(f)

	// 先解码排名信息数量
	var count int32
	if err := dec.Decode(&count); err != nil {
		log.Println("Load Ranking count error:", err.Error())
		return nil
	}

	// 创建排名树
	rt := NewRankTree()

	// 解码所有排名信息
	for i := int32(0); i < count; i++ {
		var info RankInfo
		if err := dec.Decode(&info); err != nil {
			log.Println("Load Ranking info error:", err.Error())
			return nil
		}
		// 重新构建跳跃表
		rt.Sl.insert(info.Value, &info)
		rt.EntryMapping.Store(info.ID, &info)
	}

	return rt
}

// dump排名模块
func SaveRanking(rt *RankTree, filename string) bool {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Println(err.Error())
		return false
	}
	defer f.Close()

	// 创建gob编码器
	enc := gob.NewEncoder(f)

	// 写操作，需要互斥锁
	rt.rwMutex.RLock()
	defer rt.rwMutex.RUnlock()

	// 收集所有排名信息
	infos := make([]*RankInfo, 0, rt.Sl.Length)
	rt.Sl.foreach(func(key int64, value interface{}) {
		if info, ok := value.(*RankInfo); ok {
			infos = append(infos, info)
		}
	})

	// 编码排名信息数量
	if err := enc.Encode(int32(len(infos))); err != nil {
		log.Println("Dump encode count err:", err.Error())
		return false
	}

	// 编码所有排名信息
	for _, info := range infos {
		if err := enc.Encode(info); err != nil {
			log.Println("Dump encode info err:", err.Error())
			return false
		}
	}

	return true
}

// ==================== 全局排名树集合（基于 DB 数据） ====================

type DbRankInfo struct {
	Type      int64
	Id        int64
	Val       int64
	Timestamp int64
	TempName  string
}

var (
	RTS     map[int64]*RankTree
	RTSLock sync.RWMutex
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
	RTSLock.RLock()
	for Type, rt := range rts {
		rt.EntryMapping.Range(func(key int64, entry *RankInfo) bool {
			info := DbRankInfo{
				Type:      Type,
				Id:        entry.ID,
				Val:       entry.Value,
				Timestamp: entry.Timestamp,
			}
			infos = append(infos, info)
			return true
		})
	}
	RTSLock.RUnlock()
	return infos
}

func Load(infos []DbRankInfo) {
	RTS = LoadRankTrees(infos)
}

func Save() []DbRankInfo {
	return saveRankTrees(RTS)
}

func GetRankTree(rtype int64) *RankTree {
	RTSLock.Lock()
	defer RTSLock.Unlock()
	if RTS == nil {
		RTS = make(map[int64]*RankTree)
	}
	rt, ok := RTS[rtype]
	if !ok {
		rt = NewRankTree()
		RTS[rtype] = rt
	}
	return rt
}

func GetAllRankTree() map[int64]*RankTree {
	RTSLock.RLock()
	defer RTSLock.RUnlock()
	cp := make(map[int64]*RankTree, len(RTS))
	for k, v := range RTS {
		cp[k] = v
	}
	return cp
}

func HasRankTree(rtype int64) bool {
	RTSLock.RLock()
	defer RTSLock.RUnlock()
	_, ok := RTS[rtype]
	return ok
}

func ResetRankTree(rtype int64) {
	RTSLock.Lock()
	defer RTSLock.Unlock()
	if RTS == nil {
		RTS = make(map[int64]*RankTree)
	}
	RTS[rtype] = NewRankTree()
}
