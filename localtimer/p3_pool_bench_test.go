package localtimer

import (
	"context"
	"testing"

	"touchgocore/util"
)

// 本文件服务于 S62（对象池热路径的反射与键推导开销）。
// 变更前后各跑一次，对比数据写进提交说明。

// BenchmarkTimerPoolGetPut 串行：认领+归还一次的全部开销。
func BenchmarkTimerPoolGetPut(b *testing.B) {
	b.ReportAllocs()
	proto := &plainTimer{}
	timerPool.Put(timerPool.Get(proto))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		timerPool.Put(timerPool.Get(proto))
	}
}

// BenchmarkTimerPoolGetPutParallel 并发：池本身有锁，键推导是每请求一次的反射。
func BenchmarkTimerPoolGetPutParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		proto := &plainTimer{}
		for pb.Next() {
			timerPool.Put(timerPool.Get(proto))
		}
	})
}

// BenchmarkNewTimer 走完整入口：类型校验 + 原型构造 + 认领 + Init。
func BenchmarkNewTimer(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tm, err := NewTimer[*plainTimer](1000, -1, nil)
		if err != nil {
			b.Fatal(err)
		}
		timerPool.Put(tm)
	}
}

// BenchmarkNewTimerParallel 并发新建：多核下放大每次 reflect.New 的分配成本。
func BenchmarkNewTimerParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tm, err := NewTimer[*plainTimer](1000, -1, nil)
			if err != nil {
				b.Fatal(err)
			}
			timerPool.Put(tm)
		}
	})
}

// BenchmarkTimerAddRemove 端到端测量「新建 + 入轮 + 移除」：入轮由时间轮协程
// 执行链表 Add，摘除走 Node.Remove，正是 S63 索引开销所在的位置。
func BenchmarkTimerAddRemove(b *testing.B) {
	Run(context.Background())
	defer TimeStop(context.Background())

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tm, err := NewTimer[*plainTimer](3600*1000, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			b.Fatal(err)
		}
		tm.Remove()
	}
}

// BenchmarkPoolKeyShortName 单列键推导成本：util.GetClassName 内含 reflect.Indirect
// 与一次方法名切片分配，变更前它在 Get/Put 各跑一次。
func BenchmarkPoolKeyShortName(b *testing.B) {
	b.ReportAllocs()
	proto := &plainTimer{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name, _ := util.GetClassName(proto)
		if name == "" {
			b.Fatal("empty name")
		}
	}
}
