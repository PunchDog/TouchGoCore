package config

import (
	"io/ioutil"

	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/jsonthr"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
)

type Cfg struct {
	Redis      *db.RedisConfig `json:"redis"`
	Db         *db.DBConfig    `json:"db"`
	BusId      string          `json:"busid"`      //通道ID
	Ip         string          `json:"ip"`         //端口所在IP
	ListenPort int             `json:"init_port"`  //监听端口
	ServerType string          `json:"servertype"` //服务器注册类型：exec|dll，两种注册类型
	Ws         string          `json:"ws"`         //websocket启动模式:off不启动;:1234启动监听；http://127.0.0.1:1234启动连接，监听和连接可同时存在，用|分割,连接模式必须用http开头
	Http       string          `json:"http"`       //http启动模式，off或者端口号,多个端口以|隔开
	Lua        string          `json:"lua"`        //off不启动，否则就是启动路径lua文件根目录必须有有个init.lua文件，里面提供一个GetFileName函数加载其他lua文件,GetFileName会传入index,返回end结束加载
	File       string          `json:"file"`       //文件服务，off为不开，端口号就是开启
	LogLevel   string          `json:"loglevel"`   //日志等级，off为不开,其次为info,debug,error
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
	File: "off",
}
var ServerName_ string
