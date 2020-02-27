package rpc

//服务器链接注册
var httpserver_ *HttpServer = &HttpServer{}

type HttpServer struct {
	msgClassMap_ map[string]IRpcCallFunctionClass
}

func (this *HttpServer)Run()  {
	for _, var := range httpserhttpserver_.mapclassMap_ {
	}
}

//注册服务器监听函数
func AddServerListen(class IRpcCallFunctionClass) {
	httphttpserver_.msgclassMap_[class.ClassName()] = class
	classMap_.LoadOrStore("httpserver", httpserver_)
}
