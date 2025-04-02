package util

import (
	"reflect"
	"sync"
	"time"

	"touchgocore/vars"
)

// 时间到了后执行的函数
var timerChannel_ chan ITimer

const (
	MAX_TIMER_CHANNEL_NUM     int64 = 100000
	MAX_ADD_TIMER_CHANNEL_NUM int64 = 10000
)

const (
	TIME_TYPE_MS         int8 = iota // 毫秒
	TIMER_TYPE_SECOND                //秒
	TIMER_TYPE_MINUTE                //分钟
	TIMER_TYPE_10_MINUTE             //10分钟
	TIMER_TYPE_HOUR                  //小时
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
	ListNode
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
	//管理器时间指针
	wheel *TimerWheel
}

// 从管理器中移除
func (this *Timer) Remove() {
	p := this.GetParent()
	if p == nil {
		return
	}
	if p.wheel != nil {
		this.wheel.wheelLocks.Lock()
		defer this.wheel.wheelLocks.Unlock()
	}
	this.ListNode.Remove() //从管理器中移除
}

// next
func (this *Timer) next() bool {
	//如果次数为-999999，则一直执行,如果次数不为-999999，则次数减1
	if this.count != -999999 {
		this.count--
		if this.count <= 0 {
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

// 时间类型,interval默认传参0
func (self *Timer) Type() int8 {
	return self.timer_tpye(-999999)
}
func (self *Timer) timer_tpye(interval int64) int8 {
	if interval == -999999 {
		interval = self.nextTime - time.Now().UTC().UnixMilli()
	}
	var tp int8 = TIME_TYPE_MS
	//根据时间间隔计算时间类型
	if interval < MILLISECONDS_OF_SECOND {
		tp = TIME_TYPE_MS
	} else if interval < MILLISECONDS_OF_MINUTE {
		tp = TIMER_TYPE_SECOND
	} else if interval < MILLISECONDS_OF_10_MINUTE {
		tp = TIMER_TYPE_MINUTE
	} else if interval < MILLISECONDS_OF_HOUR {
		tp = TIMER_TYPE_10_MINUTE
	} else {
		tp = TIMER_TYPE_HOUR
	}
	return tp
}

// 设置次数
func (self *Timer) SetCount(count int64) {
	self.count = count
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
	if count == -1 { //这里是保证判断不出错修正
		obj.count = -999999
	}

	return timer
}

// 时间轮
type TimerWheel struct {
	//时间轮配置
	wheelConfig int64
	//时间轮
	tickWheel *List
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
	timer.Remove()
	//添加新的定时器
	self.wheel[p.Type()].addTimerChan <- timer
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
	wheelConfig := []int64{1, MILLISECONDS_OF_SECOND, MILLISECONDS_OF_MINUTE, MILLISECONDS_OF_10_MINUTE, MILLISECONDS_OF_HOUR}
	mgr := &TimerManager{
		closeTick:   make(chan byte, 1),
		maxTimerUID: 1,
	}

	for _, v := range wheelConfig {
		// 初始化时间轮
		wheel := &TimerWheel{
			wheelConfig:  v,
			tickWheel:    NewList(),
			wheelLocks:   &sync.Mutex{},
			addTimerChan: make(chan ITimer, MAX_ADD_TIMER_CHANNEL_NUM),
		}

		mgr.wheel = append(mgr.wheel, wheel)
		// 启动一个协程
		go func(mgr *TimerManager, wheel *TimerWheel) {
			for {
				select {
				case _, ok := <-mgr.closeTick: //判断关闭：
					if !ok {
						close(wheel.addTimerChan)
						wheel.wheelLocks.Lock()
						// 遍历链表，关闭全部处理一次
						wheel.tickWheel.Range(func(node INode) bool {
							timer := node.(ITimer)
							if timer.GetParent().nextTime <= time.Now().UTC().UnixMilli() {
								timer.Tick()
							}
							return true
						})
						wheel.tickWheel.Clear()
						wheel.wheelLocks.Unlock()
						return
					}
				case timer := <-wheel.addTimerChan:
					wheel.wheelLocks.Lock()
					t := timer.GetParent()
					t.wheel = wheel
					wheel.tickWheel.Add(timer.(INode))
					wheel.wheelLocks.Unlock()
				case <-time.After(time.Duration(wheel.wheelConfig) * time.Millisecond):
					wheel.wheelLocks.Lock()
					// 遍历链表，查询时间到了的
					wheel.tickWheel.Range(func(node INode) bool {
						timer := node.(ITimer)
						t := timer.GetParent()
						//时间到了
						if timer.GetParent().nextTime <= time.Now().UTC().UnixMilli() {
							node.GetNode().Remove()
							timerChannel_ <- timer
						} else if tp := t.timer_tpye(t.nextTime - time.Now().UTC().UnixMilli() - 5); tp != t.timer_tpye(wheel.wheelConfig) { //时间没到，但需要切换tick组了
							node.GetNode().Remove()
							t.wheel = nil
							mgr.wheel[tp].addTimerChan <- timer //切换到下一个tick组
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

func TimeRun() {
	vars.Info("启动计时器")
	_defaultTimerManager = NewTimerManager()
}

func TimeStop() {
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
func TimeTick() chan bool {
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
