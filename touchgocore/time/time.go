package time

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"time"

	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
)

//GetCurrTs return current timestamps
func GetCurrTs() int64 {
	return time.Now().Unix()
}

func GetCurrFormatTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func ToUTCFormatTime(sec int64) (dateStr string) {
	now := time.Unix(sec, 0)
	utc, _ := time.LoadLocation("") //等同于"UTC"

	return now.In(utc).Format("2006-01-02 15:04:05")
}

func GetWeakDay() int32 {
	t := time.Now()
	return int32(t.Weekday())
}

func UTCToLocalTime(t time.Time) time.Time {
	_, offset := t.Zone()
	return time.Unix(t.Unix()+int64(offset), 0)
}

//是否在同一天
func GetDiffDay(day1 int64, day2 int64) int {
	return int((day2 - day1) / 86400)
}

type eTimerState int32

const (
	eTimerState_Invalid eTimerState = iota
	eTimerState_Stop
	eTimerState_Start
)

//计时器接口
type ITimer interface {
	Tick()           //执行
	over() bool      //完成了
	nextTime() int64 //切换到下一个时间
	loopdec()        //次数减一
}

//原始继承父类
type TimerObj struct {
	loop     int   //循环次数
	steptime int64 //单次循环等待时长
	maxtime  int64 //会回调的总时长
	endtime  int64 //结束时间
}

func (this *TimerObj) Init(steptime int64) {
	this.InitAll(steptime, 999999999, 99999999999)
}

func (this *TimerObj) InitAll(steptime int64, loop int, maxtime int64) {
	this.loop = loop
	this.steptime = steptime
	this.maxtime = time.Now().UnixNano()/int64(time.Millisecond) + maxtime
}

func (this *TimerObj) Tick() {
}

func (this *TimerObj) loopdec() {
	this.loop--
}

func (this *TimerObj) nextTime() int64 {
	this.endtime = time.Now().UnixNano()/int64(time.Millisecond) + this.steptime
	return this.endtime
}

func (this *TimerObj) over() bool {
	return this.loop == 0 || this.endtime >= this.maxtime
}

type TimerManager struct {
	tickMap   *syncmap.Map //数据存储(nexttime/list)
	closeTick chan byte    //结束循环
}

func (this *TimerManager) Close() {
	if this.closeTick != nil {
		close(this.closeTick)
		this.closeTick = nil
	}
}

//循环时间
func (this *TimerManager) tick() {
	for {
		select {
		case <-time.After(time.Millisecond):
			//毫秒级查询
			key := time.Now().UnixNano() / int64(time.Millisecond)
			var copylist []ITimer = nil
			this.tickMap.LoadAndFunction(key, func(l interface{}) {
				list := l.([]ITimer)
				llen := len(list)
				copylist = make([]ITimer, 0, llen)
				copy(copylist, list)
			}, true)
			if copylist != nil {
				for _, timer := range copylist {
					if !timer.over() {
						timer.Tick()
						timer.loopdec()
						endtime := timer.nextTime()
						if !timer.over() {
							this.AddTimer(timer, endtime)
						}
					}
				}
			}
		case <-this.closeTick:
			return
		}
	}
}

//添加到列表
func (this *TimerManager) AddTimer(t ITimer, endtime int64) {
	var list []ITimer = nil
	if l, ok := this.tickMap.Load(endtime); ok {
		list = l.([]ITimer)
	} else {
		list = make([]ITimer, 0)
	}
	list = append(list, t)
	this.tickMap.Store(endtime, list)
}

var _timerManager map[*TimerManager]bool = nil
var _defaultTimerManager *TimerManager = nil

func NewTimerManager() *TimerManager {
	if _timerManager == nil {
		_timerManager = make(map[*TimerManager]bool)
	}
	mgr := &TimerManager{
		closeTick: make(chan byte, 1),
		tickMap:   &syncmap.Map{},
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

//添加个定时器
func AddTimer(timer ITimer) {
	endtime := timer.nextTime()
	_defaultTimerManager.AddTimer(timer, endtime)
}
