package websocket

// 阶段13（S69）客户端表注入点回归。
//
// clientMap 从具体类型换成窄接口 clientTable，图的是「热点表可以换分片实现而不动调用方」。
// 换实现的风险点只有三处：查表命中、关服时的遍历（回调里会删正在遍历的键）、
// 以及注入守护（传 nil 不能把已绑好的表抹掉）。这里逐条钉住。

import (
	"testing"

	"touchgocore/syncmap"
)

func TestClientTableAcceptsShardedMap(t *testing.T) {
	prev := clientMap
	t.Cleanup(func() { clientMap = prev })

	sharded := syncmap.NewShardedMap[int64, *Client](0)
	UseShardedClientMap(sharded)

	uids := []int64{4201, 4202, 4203}
	for _, uid := range uids {
		c := &Client{UID: uid, remoteAddr: "bare://sharded"}
		c.initChannels()
		sharded.Store(uid, c)
		if got := GetClient(uid); got != c {
			t.Fatalf("✘ 分片表注入后 GetClient(%d) 未命中: %v", uid, got)
		}
	}
	if got := GetClient(9999); got != nil {
		t.Fatal("✘ 不存在的 uid 竟返回了客户端")
	}

	// 关服路径（shutdownWebsocket）就是「Range 里把每个客户端 Close 掉」，
	// 而 Close 会 Delete 自己这个键：逐片快照必须扛住边遍历边删。
	seen := 0
	clientMap.Range(func(uid int64, c *Client) bool {
		seen++
		clientMap.Delete(uid)
		return true
	})
	if seen != len(uids) {
		t.Fatalf("✘ 分片表遍历条数不符: seen=%d want=%d", seen, len(uids))
	}
	if sharded.Length() != 0 {
		t.Fatalf("✘ 遍历内删除后仍有残留: %d", sharded.Length())
	}

	// 注入守护：误传 nil 不能让表变成「谁都不在」。判据不能只看 clientMap 是否 == nil ——
	// 装了空指针的接口并不等于 nil，所以必须真的查一次：守护失效时这一步直接 panic。
	UseClientMap(nil)
	UseShardedClientMap(nil)
	keep := &Client{UID: 4210, remoteAddr: "bare://guard"}
	keep.initChannels()
	clientMap.Store(keep.UID, keep)
	if got := GetClient(keep.UID); got != keep {
		t.Fatalf("✘ nil 注入后查表失配: %v", got)
	}
}

// TestClientTableAcceptsZeroValueShardedMap 宿主常把表当结构体字段直接声明
// （var m syncmap.ShardedMap[...]），零值必须可用，否则注入点等于强制走构造函数。
func TestClientTableAcceptsZeroValueShardedMap(t *testing.T) {
	prev := clientMap
	t.Cleanup(func() { clientMap = prev })

	var zero syncmap.ShardedMap[int64, *Client]
	UseShardedClientMap(&zero)

	c := &Client{UID: 5201, remoteAddr: "bare://zero"}
	c.initChannels()
	zero.Store(c.UID, c)
	if GetClient(5201) != c {
		t.Fatal("✘ 零值分片表读写不通")
	}
	clientMap.Delete(5201)
	if zero.Length() != 0 {
		t.Fatalf("✘ 零值分片表删除后计数不符: %d", zero.Length())
	}
}
