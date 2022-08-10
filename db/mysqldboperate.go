package db

import (
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
)

//接口
type IDbOperate interface {
	GetDbOperateType() EDBType
	lock()
	rlock()
	unlock()
	runlock()
	Query() interface{}
	Write() error
}

//此函数主要用于更新，所以数据类都必须继承这个函数
type DBCacheData struct {
	value      interface{} // *map[string]interface{}/*[]map[string]interface{}
	updateTime int64       //更新时间
	weight     int         //引用计数，作自动释放用的
}

func (this *DBCacheData) Update(new interface{}) {
	if new == nil {
		return
	}
	switch this.value.(type) {
	case *map[string]interface{}:
		m := this.value.(*map[string]interface{})
		u := new.(*map[string]interface{})
		for k, v := range *u {
			(*m)[k] = v
		}
	case *[]map[string]interface{}:
		this.value = new
	}
}

//缓存文件
var cacheData_ *syncmap.Map = &syncmap.Map{}

//锁信息
var cacheLock_ *syncmap.Map = &syncmap.Map{}

//父类
type DbOperateObj struct {
	condition_ *Condition //操作数据
}

func (this *DbOperateObj) SetCondition(condition *Condition) {
	this.condition_ = condition
}

func (this *DbOperateObj) GetDbOperateType() EDBType {
	return this.condition_.types
}

var wait *sync.WaitGroup = &sync.WaitGroup{}

//锁
func (this *DbOperateObj) lock() {
	//等待所有任务完成
	wait.Wait()
	//创建全局等待
	wait.Add(1)
	var lock *sync.RWMutex = nil
	if l, ok := cacheLock_.Load(this.condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.condition_.cacheKey, lock)
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
	if l, ok := cacheLock_.Load(this.condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.condition_.cacheKey, lock)
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
	if l, ok := cacheLock_.Load(this.condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.condition_.cacheKey, lock)
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
	if l, ok := cacheLock_.Load(this.condition_.cacheKey); ok {
		lock = l.(*sync.RWMutex)
	} else {
		lock = new(sync.RWMutex)
		cacheLock_.Store(this.condition_.cacheKey, lock)
	}
	//等待完成
	wait.Done()
	//锁我要锁的东西
	lock.RUnlock()
}

//虚函数
func (this *DbOperateObj) Query() interface{} {
	//查缓存
	if b := this.cache(nil, this.GetDbOperateType()); b != nil {
		data := b.value.(*DBCacheData)
		data.weight++
		return data.value
	}

	//查DB
	db, _ := NewDbMysql(config.Cfg_.MySql)
	var ret *DBResult = nil
	var err error = nil
	switch this.GetDbOperateType() {
	case EDBType_Query:
		ret, err = db.SetCondition(this.condition_).Query()
	case EDBType_Query_Count:
		ret, err = db.SetCondition(this.condition_).QueryCount()
	case EDBType_Query_Sum:
		if this.condition_.values == nil {
			err = &util.Error{ErrMsg: "没有需要SUM的字段"}
		} else if len(*this.condition_.values) > 0 {
			err = &util.Error{ErrMsg: "目前只支持单字段SUM"}
		} else {
			for k, _ := range *this.condition_.values {
				ret, err = db.SetCondition(this.condition_).QuerySum(k)
			}
		}
	case EDBType_Query_Max:
		if this.condition_.values == nil {
			err = &util.Error{ErrMsg: "没有需要Max的字段"}
		} else if len(*this.condition_.values) > 0 {
			err = &util.Error{ErrMsg: "目前只支持单字段Max"}
		} else {
			for k, _ := range *this.condition_.values {
				ret, err = db.SetCondition(this.condition_).QueryMax(k)
			}
		}
	}
	if err == nil {
		if ret.Count() == 1 {
			if this.condition_.cacheKey != "" {
				this.cache(&DBCacheData{value: ret.GetOne().row}, EDBType_Insert)
			}
			return ret.GetOne().row
		} else if ret.Count() > 1 {
			if this.condition_.cacheKey != "" {
				this.cache(&DBCacheData{value: ret.GetAll().rows}, EDBType_Insert)
			}
			return ret.GetAll().rows
		}
	}
	return nil
}

func (this *DbOperateObj) write() error {
	db, _ := NewDbMysql(config.Cfg_.MySql)
	switch this.GetDbOperateType() {
	case EDBType_Insert:
		return db.SetCondition(this.condition_).Insert()
	case EDBType_Update:
		return db.SetCondition(this.condition_).Update()
	case EDBType_Delete:
		return db.SetCondition(this.condition_).Del()
	}
	return nil
}

//虚函数
func (this *DbOperateObj) Write() error {
	err := make(chan error, 1)
	//尝试改内存数据,有缓存的，可以开多线程写，没有缓存的必须单线程
	if this.cache(&DBCacheData{value: this.condition_.values}, this.GetDbOperateType()) != nil {
		go func() {
			err <- this.write()
		}()
	} else {
		err <- this.write()
	}
	return <-err
}

//缓存操作
func (this *DbOperateObj) cache(new *DBCacheData, op EDBType) *DBCacheData {
	if this.condition_ != nil {
		//查询
		if op == EDBType_Query || op == EDBType_Query_Count || op == EDBType_Query_Max || op == EDBType_Query_Sum {
			if this.condition_.cacheKey != "" {
				if p, ok := cacheData_.Load(this.condition_.cacheKey); ok {
					return p.(*DBCacheData)
				}
			}
		} else if op == EDBType_Insert || op == EDBType_Update {
			if this.condition_.cacheKey != "" {
				new.weight = 6 //初始权重保证缓存半小时
				p, ok := cacheData_.LoadOrStore(this.condition_.cacheKey, new)
				if ok {
					up := p.(*DBCacheData)
					up.Update(&new.value)
					up.weight++
					return up
				}
			}
		} else if op == EDBType_Delete {
			cacheData_.Delete(this.condition_.cacheKey)
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
var dbWriteList_ chan SDBOperate = make(chan SDBOperate, 100000)

//启动操作
func MySqlRun() {
	//读线程
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

	//写线程
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

	//缓存线程
	go func() {
		//每分钟检查一次缓存数据,作定时刷新引用计数
		for {
			select {
			case <-time.After(time.Minute):
				cacheData_.Range(func(key, value interface{}) bool {
					d := value.(*DBCacheData)
					if (d.weight <= 6 && time.Now().Unix()-d.updateTime < 300) || d.weight > 6 {
						d.weight--
						d.updateTime = time.Now().Unix() //刷新时间
						if d.weight <= 0 {
							cacheData_.Delete(key)
						}
					}
					return true
				})
			}
		}
	}()
}

//加入任务列表
func AddDbEvent(idboper IDbOperate) chan interface{} {
	event := SDBOperate{IDBOper: idboper}
	event.ChanData = make(chan interface{}, 1)
	if idboper.GetDbOperateType() == EDBType_Query || idboper.GetDbOperateType() == EDBType_Query_Count || idboper.GetDbOperateType() == EDBType_Query_Sum || idboper.GetDbOperateType() == EDBType_Query_Max {
		dbReadList_ <- event
	} else {
		dbWriteList_ <- event
	}
	return event.ChanData
}
