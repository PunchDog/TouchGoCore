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
	beego.Controller
	methodname string
	callClass  interface{}
}

//用来做消息回调的
func (self *Controller) CallBack() {
	//获取类数据
	rcvr := reflect.ValueOf(self.callClass)
	//获取函数反射
	method := rcvr.MethodByName(self.methodname)
	//回调
	method.Call([]reflect.Value{reflect.ValueOf(&self.Controller)})
}

/*注册一个类的所有函数为beego协议回调函数
所有函数的结构都为:func(*beego.Controller)
所有函数生成的协议格式都是:/类名/函数名(字符都会转换成小写)
例：type Test struct{} func TestMethod(*beego.Controller){}
生成的协议为:/test/testmethod
*/
func RegisterRouter(class interface{}) {
	sname, mnames := util.GetClassName(class)
	for _, mname := range mnames {
		con := &Controller{
			callClass:  class,
			methodname: mname,
		}
		callbackmsg := fmt.Sprintf("/%s/%s", strings.ToLower(sname), strings.ToLower(mname))
		beego.Router(callbackmsg, con, "*:CallBack")
	}
}
