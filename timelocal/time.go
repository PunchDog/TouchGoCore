package timelocal

import (
	"time"
	"touchgocore/syncmap"

	"touchgocore/vars"
)

var timerChannel_ chan ITimer

const (
	DEFAULT_LIST_NUM       int64 = 1000
	MAX_TIMER_CHANNEL_NUM        = 100000
	NANO_TO_MS                   = 1000000
	MILLISECONDS_OF_DAY          = 86400000
	MILLISECONDS_OF_HOUR         = 3600000
	MILLISECONDS_OF_MINUTE       = 60000
	MILLISECONDS_OF_SECOND       = 1000
)

// 计时器接口
type ITimer interface {
	Tick() //执行

	//以下是私有继承类函数，外部不可调用的
	Over() bool                        //完成了
	NextTime() int64                   //切换到下一个时间
	LoopDec()                          //次数减一
	AddtimerManager(mgr *TimerManager) //
	GetUid() int64                     //
	SetUid(id int64)                   //
}

// 原始继承父类
type TimerObj struct {
	loop         int           //循环次数
	steptime     int64         //单次循环等待时长
	maxtime      int64         //会回调的总时长
	endtime      int64         //结束时间
	timerManager *TimerManager //管理器
	uid          int64         //唯一ID
}

// 初始化间隔时间，单位毫秒
func (this *TimerObj) Init(steptime int64) {
	this.InitAll(steptime, 999999999, 99999999999)
}

// 初始化间隔时间，单位毫秒，循环次数，最大结束时间，三个数据皆不能为0
func (this *TimerObj) InitAll(steptime int64, loop int, maxtime int64) {
	this.loop = loop
	this.steptime = steptime
	this.maxtime = time.Now().UnixNano()/int64(time.Millisecond) + maxtime
}

// 删除定时器节点
func (this *TimerObj) Delete() {
	if this.timerManager != nil {
		listkey := this.endtime % DEFAULT_LIST_NUM
		this.timerManager.tickMap.LoadAndFunction(listkey, func(v interface{}, stfn func(v interface{}), delfn func()) {
			if v == nil {
				delfn()
				return
			}
			list := v.([]ITimer)
			for i, timer := range list {
				if timer.GetUid() != this.uid {
					continue
				}
				list = append(list[:i], list[i+1:]...)
				stfn(list)
				break
			}
		})
	}
}

// 重载用的函数
func (this *TimerObj) Tick() {
}

func (this *TimerObj) LoopDec() {
	this.loop--
}

func (this *TimerObj) NextTime() int64 {
	this.endtime = time.Now().UnixNano() / int64(time.Millisecond)
	this.endtime += this.steptime
	return this.endtime
}

func (this *TimerObj) Over() bool {
	return this.loop == 0 || this.endtime >= this.maxtime
}

func (this *TimerObj) AddtimerManager(mgr *TimerManager) {
	this.timerManager = mgr
}

func (this *TimerObj) GetUid() int64 {
	return this.uid
}

func (this *TimerObj) SetUid(id int64) {
	this.uid = id
}

type TimerManager struct {
	tickMap     *syncmap.Map //数据存储(listkey/list)
	maxTimerUID int64
	closeTick   chan byte //结束循环
}

func (this *TimerManager) Close() {
	if this.closeTick != nil {
		close(this.closeTick)
	}
}

// 循环时间
func (this *TimerManager) tick() {
	for {
		select {
		case <-time.After(time.Millisecond):
			//毫秒级查询
			key := (time.Now().UnixNano() / int64(time.Millisecond)) % DEFAULT_LIST_NUM
			var copylist []ITimer = nil
			this.tickMap.LoadAndFunction(key, func(v interface{}, storefn func(v1 interface{}), delfn func()) {
				if v == nil {
					return
				}
				list := v.([]ITimer)
				llen := len(list)
				copylist = make([]ITimer, llen, llen)
				copy(copylist, list)
				delfn()
			})
			if copylist != nil {
				for _, timer := range copylist {
					if !timer.Over() {
						//放到主线程去执行操作
						timerChannel_ <- timer
						//计数-
						timer.LoopDec()
						endtime := timer.NextTime() % DEFAULT_LIST_NUM
						if !timer.Over() {
							this.AddTimer(timer, endtime)
						}
					}
				}
			}
		case <-this.closeTick:
			this.closeTick = nil
			return
		}
	}
}

// 添加到列表
func (this *TimerManager) AddTimer(t ITimer, listkey int64) {
	var list []ITimer = nil
	if l, ok := this.tickMap.Load(listkey); ok {
		list = l.([]ITimer)
	} else {
		list = make([]ITimer, 0)
	}
	t.AddtimerManager(this)
	if t.GetUid() == 0 {
		this.maxTimerUID++
		t.SetUid(this.maxTimerUID)
	}
	list = append(list, t)
	this.tickMap.Store(listkey, list)
}

var _timerManager map[*TimerManager]bool = nil
var _defaultTimerManager *TimerManager = nil

func NewTimerManager() *TimerManager {
	if _timerManager == nil {
		timerChannel_ = make(chan ITimer, MAX_TIMER_CHANNEL_NUM)
		_timerManager = make(map[*TimerManager]bool)
	}
	mgr := &TimerManager{
		closeTick:   make(chan byte, 1),
		tickMap:     &syncmap.Map{},
		maxTimerUID: 0,
	}
	go mgr.tick()
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
	_defaultTimerManager = nil
	_timerManager = nil
}

// 添加个定时器
func AddTimer(timer ITimer) {
	listkey := timer.NextTime() % DEFAULT_LIST_NUM
	_defaultTimerManager.AddTimer(timer, listkey)
}

// 主线程调用执行
func Tick() chan bool {
	select {
	case timer := <-timerChannel_:
		timer.Tick()
	}
	return nil
}
