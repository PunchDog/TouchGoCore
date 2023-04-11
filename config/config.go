package config

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"

	"touchgocore/jsonthr"
)

type Cfg struct {
	Redis    *RedisConfig    `json:"redis"`
	MySql    *MySqlDBConfig  `json:"mysql"`
	Mongo    *MongoDBConfig  `json:"mongo"`
	Ip       string          `json:"ip"`       //端口所在IP，如果没填，就获取本地内网IP
	Ws       string          `json:"ws"`       //websocket启动模式:off不启动;:1234启动监听；http://127.0.0.1:1234启动连接，监听和连接可同时存在，用|分割,连接模式必须用http开头
	Lua      string          `json:"lua"`      //off不启动，填写lua文件的相对路径启动lua
	LogLevel string          `json:"loglevel"` //日志等级，off为不开,其次为info,debug,error
	MapPath  string          `json:"map_path"` //地图配置位置
	BeegoWeb *BeegoWebConfig `json:"beegoweb"` //beegoweb配置
}

func init() {
	Cfg_ = &Cfg{
		Ws:       "off",
		Lua:      "off",
		LogLevel: "info",
		MapPath:  "off",
		Ip:       "",
	}

	if PathExists(_defaultFile) == false {
		_basePath = path.Join(path.Dir(os.Args[0]), "../../")
	}
}

func (this *Cfg) Load(path string) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		panic("读取启动配置出错:" + err.Error())
	}
	fmt.Println(string(file))
	err = jsonthr.Json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path + ":" + err.Error())
	}

	//如果没有填IP，则是获取本地IP
	if this.Ip == "" {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			fmt.Println(err)
			return
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					this.Ip = ipnet.IP.String()
					fmt.Println(this.Ip)
				}
			}
		}
	}
}

var Cfg_ *Cfg = nil
var ServerName_ string
var _basePath = path.Join(path.Dir(os.Args[0]), "../")
var _defaultFile = path.Join(_basePath, "configs/config.ini")

func GetBasePath() string {
	return _basePath
}

func PathExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	return false
}
