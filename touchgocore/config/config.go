package config

import (
	"io/ioutil"

	"github.com/TouchGoCore/touchgocore/db"
	"github.com/TouchGoCore/touchgocore/jsonthr"
	"github.com/TouchGoCore/touchgocore/vars"
)

type Cfg struct {
	db.RedisConfig
	Protocol1  int    `json:"protocol1"`  //协议1级协议号
	BusId      int    `json:"busid"`      //通道ID
	Ip         string `json:"ip"`         //端口所在IP
	ListenPort int    `json:"port"`       //监听端口
	ServerType string `json:"servertype"` //服务器注册类型：exec|dll，两种注册类型
	Ws         string `json:"ws"`         //websocket启动模式:off不启动;:1234启动监听；http://127.0.0.1:1234启动连接，监听和连接可同时存在，用|分割,连接模式必须用http开头
	Http       string `json:"http"`       //http启动模式，off或者端口号,多个端口以|隔开
	Lua        string `json:"lua"`        //off不启动，否则就是启动路径lua文件根目录必须有有个init.lua文件，里面提供一个GetFileName函数加载其他lua文件,GetFileName会传入index,返回end结束加载
}

func (this *Cfg) Load(path string) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		panic("读取Bus文件出错:" + err.Error())
	}
	vars.Info(string(file))
	err = jsonthr.Json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path + ":" + err.Error())
	}
}

var Cfg_ *Cfg = &Cfg{
	Ws:   "off",
	Http: "off",
	Lua:  "off",
}
var ServerName_ string