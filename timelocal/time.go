package timelocal

import (
	"reflect"
	"sync"
	"time"

	"touchgocore/util"
	"touchgocore/vars"
)

// 时间到了后执行的函数
var timerChannel_ chan ITimer

const (
	MAX_TIMER_CHANNEL_NUM     int64 = 100000
	MAX_ADD_TIMER_CHANNEL_NUM int64 = 10000
)

const (
	TIME_TYPE_MS      int8 = iota // 毫秒
	TIMER_TYPE_SECOND             //秒
	TIMER_TYPE_MINUTE             //分钟
	TIMER_TYPE_HOUR               //小时
)

// 计时器接口
type ITimer interface {
	//执行
	Tick()
	//从管理器中移除
	Remove()
	//获取uid
	GetUid() int64
	//获取父类指针
	GetParent() *Timer
	//是否还有下一次
	next() bool
}

// 计时器父类
type Timer struct {
	util.ListNode
	//唯一id
	uid int64
	//下次执行时间
	nextTime int64
	//间隔时间
	interval int64
	//执行次数
	count int64
	//管理器指针
	mgr *TimerManager
	//时间类型
	timeType int8
}

// 从管理器中移除
func (this *Timer) Delete() {
	p := this.GetParent()
	if p == nil {
		return
	}
	this.mgr.wheel[p.timeType].wheelLocks.Lock()
	defer this.mgr.wheel[p.timeType].wheelLocks.Unlock()
	this.Remove() //从管理器中移除
}

// next
func (this *Timer) next() bool {
	//如果次数为-1，则一直执行,如果次数不为-1，则次数减1
	if this.count != -1 {
		this.count--
		if this.count == 0 {
			return false
		}
	}

	//计算下次执行时间
	this.nextTime = time.Now().UTC().UnixMilli() + this.interval
	return true
}

// 获取uid
func (this *Timer) GetUid() int64 {
	return this.uid
}

// 获取父类指针
func (this *Timer) GetParent() *Timer {
	return this
}

// 创建一个计时器,count==-1表示一直执行
func NewTimer(interval int64, count int64, cls ITimer) ITimer {
	if cls == nil || interval <= 0 {
		return nil
	}

	timer := reflect.New(reflect.TypeOf(cls).Elem()).Interface().(ITimer)

	//使用反射创建一个计时器
	obj := timer.GetParent()
	if obj == nil {
		return nil
	}

	obj.nextTime = time.Now().UTC().UnixMilli() + interval
	obj.interval = interval
	obj.count = count

	//根据时间间隔计算时间类型
	if interval < util.MILLISECONDS_OF_SECOND {
		obj.timeType = TIME_TYPE_MS
	} else if interval < util.MILLISECONDS_OF_MINUTE {
		obj.timeType = TIMER_TYPE_SECOND
	} else if interval < util.MILLISECONDS_OF_HOUR {
		obj.timeType = TIMER_TYPE_MINUTE
	} else {
		obj.timeType = TIMER_TYPE_HOUR
	}
	return timer
}

// 时间轮
type TimerWheel struct {
	//时间轮配置
	wheelConfig int64
	//时间轮
	tickWheel *util.List
	//时间轮锁
	wheelLocks *sync.Mutex
	//用于添加的channel
	addTimerChan chan ITimer
}

// 时间管理器
type TimerManager struct {
	//关闭标志
	closeTick chan byte
	//时间轮数据
	wheel []*TimerWheel
	//最大uid
	maxTimerUID int64
}

// 添加个定时器
func (self *TimerManager) AddTimer(timer ITimer) {
	p := timer.GetParent()
	if p == nil {
		return
	}

	p.uid = self.maxTimerUID
	self.maxTimerUID++
	p.mgr = self

	//清理已经有的处定时器
	timer.GetParent().Delete()
	//添加新的定时器
	self.wheel[p.timeType].addTimerChan <- timer
}

func (this *TimerManager) Close() {
	if this.closeTick != nil {
		close(this.closeTick)
	}
}

var _timerManager map[*TimerManager]bool = nil
var _defaultTimerManager *TimerManager = nil

func NewTimerManager() *TimerManager {
	if _timerManager == nil {
		timerChannel_ = make(chan ITimer, MAX_TIMER_CHANNEL_NUM)
		_timerManager = make(map[*TimerManager]bool)
	}

	// 毫秒/秒/分钟级/小时级/
	wheelConfig := []int64{1, util.MILLISECONDS_OF_SECOND, util.MILLISECONDS_OF_MINUTE, util.MILLISECONDS_OF_HOUR}
	mgr := &TimerManager{
		closeTick:   make(chan byte, 1),
		maxTimerUID: 1,
	}

	for _, v := range wheelConfig {
		// 初始化时间轮
		wheel := &TimerWheel{
			wheelConfig:  v,
			tickWheel:    util.NewList(),
			wheelLocks:   &sync.Mutex{},
			addTimerChan: make(chan ITimer, MAX_ADD_TIMER_CHANNEL_NUM),
		}

		mgr.wheel = append(mgr.wheel, wheel)
		// 启动一个协程
		go func(mgr *TimerManager, wheel *TimerWheel) {
			for {
				select {
				case <-mgr.closeTick:
					return
				case timer := <-wheel.addTimerChan:
					wheel.wheelLocks.Lock()
					wheel.tickWheel.Add(timer.(util.INode))
					wheel.wheelLocks.Unlock()
				case <-time.After(time.Duration(wheel.wheelConfig) * time.Millisecond):
					wheel.wheelLocks.Lock()
					// 遍历链表，查询时间到了的
					wheel.tickWheel.Range(func(node util.INode) bool {
						timer := node.(ITimer)
						if timer.GetParent().nextTime <= time.Now().UTC().UnixMilli() {
							node.Remove()
							timerChannel_ <- timer
						}
						return true
					})
					wheel.wheelLocks.Unlock()
				}
			}
		}(mgr, wheel)
	}

	_timerManager[mgr] = true
	return mgr
}

func Run() {
	vars.Info("启动计时器")
	_defaultTimerManager = NewTimerManager()
}

func Stop() {
	for mgr, _ := range _timerManager {
		mgr.Close()
	}
	close(timerChannel_)
	_defaultTimerManager = nil
	_timerManager = nil
}

// 添加个定时器
func AddTimer(timer ITimer) {
	_defaultTimerManager.AddTimer(timer)
}

// 主线程调用执行
func Tick() chan bool {
	select {
	case timer := <-timerChannel_:
		timer.Tick()
		if b := timer.next(); b {
			timer.GetParent().mgr.AddTimer(timer)
		}
	default:
	}
	return nil
}
