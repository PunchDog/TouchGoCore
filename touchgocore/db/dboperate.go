package db

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"sync"
)

//操作枚举
type eDBType int

const (
	EDBType_Query eDBType = iota + 1
	EDBType_Insert
	EDBType_Update
	EDBType_Delete
)

//接口
type IDbOperate interface {
	GetDbOperateType() eDBType
	lock()
	rlock()
	unlock()
	runlock()
	Query() IDBCacheData
	Write() int
}

//此函数主要用于更新，所以数据类都必须继承这个函数
type IDBCacheData interface {
	Update(IDBCacheData)
	Get(key string) interface{}              //获取单个数据
	GetAll(fn func(k string, v interface{})) //获取所有数据
}

//缓存文件
var cacheData_ *syncmap.Map = &syncmap.Map{}

//锁信息
var cacheLock_ *syncmap.Map = &syncmap.Map{}

//父类
type DbOperateObj struct {
	Type_      eDBType    //操作类型
	Condition_ *Condition //操作数据
}

func (this *DbOperateObj) SetCondition(condition *Condition) {
	this.Condition_ = condition
}

func (this *DbOperateObj) GetDbOperateType() eDBType {
	return this.Type_
}

var wait *sync.WaitGroup = &sync.WaitGroup{}

//锁
func (this *DbOperateObj) lock() {
	//等待所有任务完成
	wait.Wait()
	//创建全局等待
	wait.Add(1)
	var lock *sync.RWMutex = nil
	if l, ok := cacheLock_.Load(this.Condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.Condition_.cacheKey, lock)
	}
	//等待完成
	wait.Done()
	//锁我要锁的东西
	lock.Lock()
}

func (this *DbOperateObj) rlock() {
	//等待所有任务完成
	wait.Wait()
	//创建全局等待
	wait.Add(1)
	var lock *sync.RWMutex = nil
	if l, ok := cacheLock_.Load(this.Condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.Condition_.cacheKey, lock)
	}
	//等待完成
	wait.Done()
	//锁我要锁的东西
	lock.RLock()
}

func (this *DbOperateObj) unlock() {
	//等待所有任务完成
	wait.Wait()
	//创建全局等待
	wait.Add(1)
	var lock *sync.RWMutex = nil
	if l, ok := cacheLock_.Load(this.Condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.Condition_.cacheKey, lock)
	}
	//等待完成
	wait.Done()
	//锁我要锁的东西
	lock.Unlock()
}

func (this *DbOperateObj) runlock() {
	//等待所有任务完成
	wait.Wait()
	//创建全局等待
	wait.Add(1)
	var lock *sync.RWMutex = nil
	if l, ok := cacheLock_.Load(this.Condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.Condition_.cacheKey, lock)
	}
	//等待完成
	wait.Done()
	//锁我要锁的东西
	lock.RUnlock()
}

//虚函数
func (this *DbOperateObj) Query() IDBCacheData {
	////查缓存
	//if b := this.cache(nil); b != nil {
	//	return b
	//}
	//
	////查DB
	//db, _ := NewDbMysql(config.Cfg_.Db)
	//ret, err := db.SetCondition(this.Condition_).Query()
	//if err == nil {
	//
	//}
	return nil
}

//虚函数
func (this *DbOperateObj) Write() int {
	return 0
}

//缓存操作
func (this *DbOperateObj) cache(newp IDBCacheData) IDBCacheData {
	if this.Condition_ != nil {
		//查询
		if this.Type_ == EDBType_Query {
			if p, ok := cacheData_.Load(this.Condition_.cacheKey); ok {
				return p.(IDBCacheData)
			}
		} else if this.Type_ == EDBType_Insert || this.Type_ == EDBType_Update {
			p, ok := cacheData_.LoadOrStore(this.Condition_.cacheKey, newp)
			if ok {
				oldp := p.(IDBCacheData)
				oldp.Update(newp)
			}
		} else if this.Type_ == EDBType_Delete {
			cacheData_.Delete(this.Condition_.cacheKey)
		}
	}
	return nil
}

type SDBOperate struct {
	ChanData chan interface{}
	IDBOper  IDbOperate
}

//所有操作的列表
var dbReadList_ chan SDBOperate = make(chan SDBOperate, 100000)
var dbWriteList_ chan SDBOperate = make(chan SDBOperate, 10000)

//启动操作
func Run() {
	go func() {
		for {
			select {
			case s := <-dbReadList_:
				//读线程可以多查
				go func() {
					op := s.IDBOper
					op.rlock()
					defer op.runlock()
					s.ChanData <- op.Query()
				}()
			}
		}
	}()

	go func() {
		for {
			select {
			case s := <-dbWriteList_:
				//读线程可以多查
				func() {
					op := s.IDBOper
					op.lock()
					defer op.unlock()
					s.ChanData <- op.Write()
				}()
			}
		}
	}()
}

//加入任务列表
func AddDbEvent(idboper IDbOperate) chan interface{} {
	event := SDBOperate{IDBOper: idboper}
	event.ChanData = make(chan interface{}, 1)
	ret := event.ChanData
	if idboper.GetDbOperateType() == EDBType_Query {
		dbReadList_ <- event
	} else {
		dbWriteList_ <- event
	}
	return ret
}
