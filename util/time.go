package util

import (
	"reflect"
	"regexp"
	"strconv"
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
	//添加自己的指针
	addMe(ITimer)
	remove(cleanPool bool)
}

// timer内存池
type TimerPool struct {
	//内存池
	pool map[reflect.Type]*sync.Pool
}

func (self *TimerPool) Get(cls ITimer) ITimer {
	tp := reflect.TypeOf(cls).Elem()
	if _, ok := self.pool[tp]; !ok {
		pool := &sync.Pool{
			New: func() interface{} {
				return reflect.New(tp).Interface().(ITimer)
			},
		}
		self.pool[tp] = pool
	}
	return self.pool[tp].Get().(ITimer)
}

func (self *TimerPool) Put(cls ITimer) {
	tp := reflect.TypeOf(cls).Elem()
	if _, ok := self.pool[tp]; !ok {
		return
	}
	self.pool[tp].Put(cls)
}

var _timerPool = &TimerPool{
	pool: make(map[reflect.Type]*sync.Pool),
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
	//本体
	self ITimer
}

func (this *Timer) addMe(t ITimer) {
	this.self = t
}

// 从管理器中移除
func (this *Timer) remove(cleanPool bool) {
	p := this.GetParent()
	if p == nil {
		return
	}
	if p.wheel != nil {
		this.wheel.wheelLocks.Lock()
		defer this.wheel.wheelLocks.Unlock()
	}
	this.ListNode.Remove() //从管理器中移除

	if cleanPool {
		_timerPool.Put(this.self)
	}
}

func (this *Timer) Remove() {
	this.remove(true)
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

	timer := _timerPool.Get(cls)

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
	timer.remove(false)
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

// 时间类型///////////////////////////////////////////////////////////////////////////////////
// 返回unix时间戳。
func CurrentMS() int64 {
	return time.Now().UnixMilli()
}

// 返回unix时间戳。
func CurrentS() int64 {
	return time.Now().Unix()
}

// 毫秒转时间字符串
func Ms2StrTime(ms int64) string {
	msTime := time.UnixMilli(ms)
	return msTime.Format("2006-01-02 15:04:05")
}

// 秒转时间字符串
func S2StrTime(sec int64) string {
	msTime := time.Unix(sec, 0)
	return msTime.Format("2006-01-02 15:04:05")
}

// 这个时间对应的0点
func Time2Midnight(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

// 从一个毫秒时间戳获得当前时区的本日凌晨时间。
func Ms2Midnight(t int64) time.Time {
	midTime := Time2Midnight(time.Unix(t/1000, t%1000))
	return midTime
}

// 字符串时间转换时间戳 date format: "2006-01-02 13:04:00"
func S2UnixTime(value string) int64 {
	re := regexp.MustCompile(`([\d]+)-([\d]+)-([\d]+) ([\d]+):([\d]+):([\d]+)`)
	slices := re.FindStringSubmatch(value)
	if slices == nil || len(slices) != 7 {
		vars.Error("time[%s] format error, expect format: 2006-01-02 13:04:00...", value)
		return 0
	}
	year, _ := strconv.Atoi(slices[1])
	month, _ := strconv.Atoi(slices[2])
	day, _ := strconv.Atoi(slices[3])
	hour, _ := strconv.Atoi(slices[4])
	min, _ := strconv.Atoi(slices[5])
	sec, _ := strconv.Atoi(slices[6])
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	return t.UnixMilli()
}

// 下一个0点
func NextMidnight(t int64) int64 {
	midTime := Time2Midnight(time.UnixMilli(t))
	return midTime.UnixMilli() + MILLISECONDS_OF_DAY
}

// 从一个毫秒时间戳获取下一个准点时间。
func NextHour(t int64) int64 {
	t1 := time.UnixMilli(t)
	year, month, day := t1.Date()
	hour, _, _ := t1.Clock()
	t2 := time.Date(year, month, day, hour+1, 0, 0, 0, t1.Location())
	return t2.UnixMilli()
}

// 同一个星期
func InSameWeek(t1, t2 int64) bool {
	if t1 == 0 || t2 == 0 {
		return false
	}
	y1, w1 := time.UnixMilli(t1).ISOWeek()
	y2, w2 := time.UnixMilli(t2).ISOWeek()
	return y1 == y2 && w1 == w2
}

// 同一个月
func InSameMonth(t1, t2 int64) bool {
	if t1 == 0 || t2 == 0 {
		return false
	}
	y1, m1, _ := time.UnixMilli(t1).Date()
	y2, m2, _ := time.UnixMilli(t2).Date()
	return y1 == y2 && m1 == m2
}

// 是否在同一天
func GetDiffDay(day1 int64, day2 int64) bool {
	tm1 := time.UnixMilli(day1)
	tm2 := time.UnixMilli(day2)
	return tm1.Year() == tm2.Year() && tm1.Month() == tm2.Month() && tm1.Day() == tm2.Day()
}

///////////////////////////////////////////////////////////////////////////////////////////////////
