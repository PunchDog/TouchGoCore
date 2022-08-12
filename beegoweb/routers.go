package beegoweb

import (
	"fmt"
	"reflect"
	"strings"
	"touchgocore/util"

	"github.com/astaxie/beego"
)

//创建一个beego的回调函数
type Controller struct {
	BaseController
	requesttomethodname map[string]string
	callClass           interface{}
}

func (this *Controller) Index() {
	this.Layout = "base/layout.html"
	this.LayoutSections = map[string]string{}
	this.LayoutSections["PageHead"] = "base/page_header.html"

	// token, err := this.Ctx.Request.Cookie("token")
	// nowTime, _ := this.Ctx.Request.Cookie("nowTime")
	// randNum, _ := this.Ctx.Request.Cookie("randNum")

	// if err != nil {
	// 	this.TplName = "login.html"
	// 	return
	// }

	// time, _ := strconv.Atoi(nowTime.Value)
	// num, _ := strconv.Atoi(randNum.Value)

	// this.Data["TabalTag"] = "#troop"

	// if checkTokenByString(int64(num), int64(time), token.Value) {
	// 	this.TplName = "tool.html"
	// 	this.LayoutSections["Header"] = "base/header.html"
	// 	RepeatSubmit = false
	// 	return
	// } else {
	// 	this.TplName = "login.html"
	// }
}

//用来做消息回调的
func (self *Controller) CallBack() {
	if methodname, h := self.requesttomethodname[self.Ctx.Request.RequestURI]; h {
		//获取类数据
		rcvr := reflect.ValueOf(self.callClass)
		//获取函数反射
		method := rcvr.MethodByName(methodname)
		//回调
		method.Call([]reflect.Value{reflect.ValueOf(&self.Controller)})
	}
}

/*注册一个类的所有函数为beego协议回调函数
所有函数的结构都为:func(*beego.Controller)
所有函数生成的协议格式都是:/类名/函数名(字符都会转换成小写)
例：type Test struct{} func TestMethod(*beego.Controller){}
生成的协议为:/test/testmethod
*/
func RegisterRouter(class interface{}) {
	sname, mnames := util.GetClassName(class)
	con := &Controller{
		callClass:           class,
		requesttomethodname: make(map[string]string),
	}
	for _, mname := range mnames {
		callbackmsg := fmt.Sprintf("/%s/%s", strings.ToLower(sname), strings.ToLower(mname))
		con.requesttomethodname[callbackmsg] = mname
		beego.Router(callbackmsg, con, "*:CallBack")
	}
}
