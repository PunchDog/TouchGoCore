package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"

	"touchgocore/ini"
)

type Cfg struct {
	Redis    *RedisConfig     `json:"redis"`
	MySql    *MySqlDBConfig   `json:"mysql"`
	Mongo    *MongoDBConfig   `json:"mongo"`
	Ip       string           `json:"ip"`        //端口所在IP，如果没填，就获取本地内网IP
	Ws       *WebsocketConfig `json:"ws"`        //websocket启动模式:off不启动;:1234启动监听；http://127.0.0.1:1234启动连接，监听和连接可同时存在，用|分割,连接模式必须用http开头
	Lua      string           `json:"lua"`       //off不启动，填写lua文件的相对路径启动lua
	LogLevel string           `json:"log_level"` //日志等级，off为不开,其次为INFO,DEBUG,WARN,ERROR
	MapPath  string           `json:"map_path"`  //地图配置位置
	BeegoWeb *BeegoWebConfig  `json:"beegoweb"`  //beegoweb配置
	RpcPort  *RpcConfig       `json:"rpc_port"`  //rpc_port端口，没有则表示不开rpc服务
	Telegram *TelegramConfig  `json:"telegram"`  //telegram配置
	//其他配置
	Other interface{} `json:"other_data"` //其他配置,需要自行传入想要的数据模型
}

func init() {
	Cfg_ = &Cfg{
		Ws:       nil,
		Lua:      "off",
		LogLevel: "info",
		MapPath:  "off",
		Ip:       "",
		RpcPort:  nil,
		Other:    nil,
		Telegram: nil,
	}

	if PathExists(_defaultFile) == false {
		_basePath = path.Join(path.Dir(os.Args[0]), "../../")
		_defaultFile = path.Join(_basePath, "conf/config.ini")
	}
}

func (this *Cfg) Load(cfgname string) {
	var path string
	if p, err := ini.Load(_defaultFile); err == nil {
		path = _basePath + "/conf/" + p.GetString(cfgname, "ini", "")
	}

	file, err := ioutil.ReadFile(path)
	if err != nil {
		panic("读取启动配置出错:" + err.Error())
	}
	fmt.Println(string(file))

	err = json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path + ":" + err.Error())
	}
}

var (
	Cfg_         *Cfg = nil
	ServerName_  string
	_basePath    = path.Join(path.Dir(os.Args[0]), "../")
	_defaultFile = path.Join(_basePath, "conf/config.ini")
	_defServerId = 1 //TODO 0
	_serverFlag  = flag.Int("s", _defServerId, "server flag")
)

func GetBasePath() string {
	return _basePath
}

func GetDefaultFie() string {
	return _defaultFile
}

func GetServerID() int {
	return _defServerId
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
