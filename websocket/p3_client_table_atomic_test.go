package websocket

// 阶段13复核整改 F4：客户端表换绑必须原子。
//
// S69 把 clientMap 从具体类型 *syncmap.Map 换成窄接口 clientTable，图的是「热点表可换
// 分片实现」。代价是接口值占两个字（itab + 数据指针）：普通赋值不原子，而派发路径每条
// 消息都要读这张表，读到「新 itab 配旧数据指针」就是野指针。现在表装在堆上、
// 换绑是一次原子指针交换（见 client.go 的 clientMapHolder）。
//
// 说明：本机无 gcc，跑不了 go test -race，因此这条用例是「并发冒烟」而不是先红后绿 ——
// 撕裂读需要极特定的指令交错，压力用例能覆盖换绑期间的可用性，但无法在旧写法上稳定变红。
// 判绿依据是结构：读写全部经由 atomic.Pointer，读侧只会看到换过或没换过的完整表。

import (
	"sync"
	"testing"

	"touchgocore/syncmap"
)

func TestClientMapSwapIsAtomic(t *testing.T) {
	prev := loadClientMap()
	t.Cleanup(func() { storeClientMap(prev) })

	// 两张预先装满同一批 uid 的表：任一时刻读到的表都必须能查到这批 uid，
	// 且返回的实例只可能是这两张表里装的那 8 个之一（撕裂读会给出别的对象）。
	uids := []int64{7001, 7002, 7003, 7004}
	known := make(map[*Client]bool, 2*len(uids))
	mk := func() *syncmap.ShardedMap[int64, *Client] {
		m := syncmap.NewShardedMap[int64, *Client](0)
		for _, uid := range uids {
			c := &Client{UID: uid, remoteAddr: "bare://atomic"}
			c.initChannels()
			m.Store(uid, c)
			known[c] = true
		}
		return m
	}
	a, b := mk(), mk()
	// 先装好表再起读协程：否则读侧第一拍就可能在换绑之前读到「未注入」
	storeClientMap(a)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i&1 == 0 {
				storeClientMap(a)
			} else {
				storeClientMap(b)
			}
		}
	}()

	// 读侧：每条都要拿到一张查得到全部 uid 的完整表（拿到半张/野指针会 panic 或漏查）
	var readers sync.WaitGroup
	for r := 0; r < 8; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 3000; i++ {
				table := loadClientMap()
				if table == nil {
					t.Error("✘ 换绑期间读到 nil 表")
					return
				}
				for _, uid := range uids {
					c, ok := table.Load(uid)
					if !ok || c == nil || c.UID != uid {
						t.Errorf("✘ 读到不完整的表: uid=%d ok=%v c=%v", uid, ok, c)
						return
					}
					// GetClient 自己会再取一次快照，可能读到另一张表：
					// 判据只能是「两张表里装过的那两个实例之一」，不是同一个指针。
					if got := GetClient(uid); got == nil || !known[got] || got.UID != uid {
						t.Errorf("✘ GetClient 返回了不属于任一表的实例: uid=%d got=%p", uid, got)
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	wg.Wait()
}
