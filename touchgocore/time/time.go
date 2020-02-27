package time

import (
	"payserver/syncmap"
	"time"
)

//GetCurrTs return current timestamps
func GetCurrTs() int64 {
	return time.Now().Unix()
}

func GetCurrFormatTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func ToUTCFormatTime(sec int64) (dateStr string) {
	//https://www.jb51.net/article/158799.htm
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

const DEFAULT_MAX_OVER_TIME int64 = (86400 * 60)

//时间接口
type ITimer interface {
	MaxOverTimeOut() bool
	IsOverTime() bool
	TimeOut() bool
	NextTime() bool
	OverTimer(bool, *CTimerManager)

	Tick()
}

//时间接口
type ITimerObj struct {
	m_NextTime    int64
	m_OverTime    int64
	m_LastNum     int
	m_WaitTime    int64
	m_MaxOverTime int64
	m_LoopNum     int
	m_eTimerState eTimerState //开启状态
	TickFn        func()
}

func (this *ITimerObj) Tick() {
	this.TickFn()
}

//开始下一个时间节点
func (this *ITimerObj) NextTime() bool {
	if this.m_WaitTime == 0 || this.m_MaxOverTime == 0 {
		return false
	}

	//时间到了，或者循环次数完了
	if this.IsOverTime() {
		return false
	}
	this.m_NextTime = time.Now().Unix() + this.m_WaitTime
	this.m_LastNum--
	return true
}

//是否到了结束
func (this *ITimerObj) IsOverTime() bool {
	if (this.m_LoopNum > 0 && this.m_LastNum == 0) || this.MaxOverTimeOut() {
		return true
	}
	return false
}

//时间到了没
func (this *ITimerObj) TimeOut() bool {
	curTime := time.Now().Unix()
	return this.m_NextTime <= curTime
}
func (this *ITimerObj) MaxOverTimeOut() bool {
	curTime := time.Now().Unix()
	return this.m_OverTime <= curTime
}

//结束时间计数
func (this *ITimerObj) OverTimer(isTick bool, pManager *CTimerManager) {
	if this.m_eTimerState != eTimerState_Start {
		return
	}

	this.m_eTimerState = eTimerState_Stop

	if isTick {
		this.Tick()
	}

	pManager.DelTimer(this)
}

//设置开关
func (this *ITimerObj) SetTime(wait_time int64, max_over_time int64, loop_num int) {
	this.m_WaitTime = wait_time
	this.m_LoopNum = loop_num
	this.m_MaxOverTime = max_over_time
	this.m_eTimerState = eTimerState_Stop
}

//开始时间计数
func (this *ITimerObj) StartTimer(pManager *CTimerManager) {
	if this.m_eTimerState != eTimerState_Stop {
		return
	}

	this.m_LastNum = this.m_LoopNum
	this.m_OverTime = time.Now().Unix() + this.m_MaxOverTime
	if this.NextTime() {
		pManager.AddTimer(this)
		this.m_eTimerState = eTimerState_Start
	}
}

//获取剩余时间
func (this *ITimerObj) GetLastTime() int64 {
	if this.MaxOverTimeOut() {
		return 0
	}

	if this.m_WaitTime == 0 || this.m_MaxOverTime == 0 {
		return 0
	}

	return this.m_NextTime - time.Now().Unix()
}

//新添加时间管理器
type CTimerManager struct {
	timerList *syncmap.Map
	havedData bool
}

var TimerManager_ *CTimerManager = &CTimerManager{
	timerList: &syncmap.Map{},
	havedData: false,
}

//添加数据
func (this *CTimerManager) AddTimer(timer ITimer) {
	this.timerList.Store(timer, true)
	this.havedData = true
}

//删除数据
func (this *CTimerManager) DelTimer(timer ITimer) {
	this.timerList.Delete(timer)
	this.havedData = bool(this.timerList.Length() > 0)
}

//开启计时器
func (this *CTimerManager) Tick() {
	for {
		if !this.havedData {
			time.Sleep(time.Millisecond * 10)
			continue
		}

		TimerList := *this.timerList //拷贝计时器数据
		//清空原来的计时器数据
		this.timerList = &syncmap.Map{}

		//检查数据
		for TimerList.Length() > 0 {
			//取新的头
			var pTimer ITimer = nil
			TimerList.Range(func(k, v interface{}) bool {
				pTimer = k.(ITimer)
				TimerList.Delete(k)
				return false
			})

			//时间判断
			if pTimer.MaxOverTimeOut() {
				pTimer.OverTimer(true, this)
				continue
			}
			if pTimer.TimeOut() {
				if pTimer.IsOverTime() {
					pTimer.OverTimer(true, this)
					continue
				} else {
					pTimer.Tick()
					if !pTimer.NextTime() {
						continue
					}
				}
			}
			this.AddTimer(pTimer)
		}
		time.Sleep(time.Millisecond * 100)
	}
}
