package config

import (
	"io/ioutil"

	"github.com/PunchDog/TouchGoCore/touchgocore/jsonthr"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
)

type Cfg struct {
	Redis      *RedisConfig `json:"redis"`
	Db         *DBConfig    `json:"db"`
	BusId      string       `json:"busid"`      //通道ID
	Ip         string       `json:"ip"`         //端口所在IP
	ListenPort int          `json:"init_port"`  //监听端口
	ServerType string       `json:"servertype"` //服务器注册类型：exec|dll，两种注册类型
	Ws         string       `json:"ws"`         //websocket启动模式:off不启动;:1234启动监听；http://127.0.0.1:1234启动连接，监听和连接可同时存在，用|分割,连接模式必须用http开头
	Http       string       `json:"http"`       //http启动模式，off或者端口号,多个端口以|隔开
	Lua        string       `json:"lua"`        //off不启动，填写lua文件的相对路径启动lua
	File       string       `json:"file"`       //文件服务，off为不开，端口号就是开启
	LogLevel   string       `json:"loglevel"`   //日志等级，off为不开,其次为info,debug,error
	TeamId     string       `json:"teamid"`     //服务器集群ID
	DllList    []string     `json:"dlllist"`    //自动拉起程序列表(仅对servertype为exec的有效)
	MapPath    string       `json:"mappath"`    //地图配置路径
}

func (this *Cfg) Load(path string) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		panic("读取启动配置出错:" + err.Error())
	}
	vars.Info(string(file))
	err = jsonthr.Json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path + ":" + err.Error())
	}
}

var Cfg_ *Cfg = &Cfg{
	Ws:       "off",
	Http:     "off",
	Lua:      "off",
	File:     "off",
	LogLevel: "info",
	MapPath:  "off",
}
var ServerName_ string
