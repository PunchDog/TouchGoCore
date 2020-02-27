package rpc

//服务器链接注册
var httpserver_ *HttpServer = &HttpServer{}

type HttpServer struct {
	msgClassMap_ map[type]type
}

func (this *HttpServer)Run()  {
	
}

//注册服务器监听函数
func AddServerListen(class interface{}) {

	classMap_.LoadOrStore("httpserver", httpserver_)
}
