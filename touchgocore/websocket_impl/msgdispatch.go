package impl

import "github.com/PunchDog/TouchGoCore/touchgocore/vars"

var (
	SMsgDispatch_ SMsgDispatch
)

type FuncData struct {
	SzType string
	Fn     func(c *Connection, body []byte)
}

type SMsgDispatch struct {
	RegisterFuc map[int64]*FuncData
}

//注册函数
func (this *SMsgDispatch) Register(szType string, protocol1 int32, protocol2 int32, fn func(c *Connection, body []byte)) {
	if this.RegisterFuc == nil {
		this.RegisterFuc = make(map[int64]*FuncData)
	}

	data := &FuncData{
		SzType: szType,
		Fn:     fn,
	}
	this.RegisterFuc[int64(protocol1)<<32|int64(protocol2)] = data
}

//调度函数
func (this *SMsgDispatch) Do(protocol1 int32, protocol2 int32, c *Connection, body []byte) {
	funcdata := this.RegisterFuc[int64(protocol1)<<32|int64(protocol2)]
	if funcdata != nil {
		vars.Info("当前接收的消息是:", funcdata.SzType)
		funcdata.Fn(c, body)
	} else {
		vars.Error("没有注册的函数消息回调:%d-%d", protocol1, protocol2)
	}
}
