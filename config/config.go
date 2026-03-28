package config

import (
	"encoding/json"
	"flag"
	"os"
	"path"

	"touchgocore/ini"
)

type Cfg struct {
	Redis    *RedisConfig     `json:"redis"`
	MySql    *MySqlDBConfig   `json:"mysql"`
	Mongo    *MongoDBConfig   `json:"mongo"`
	Ip       string           `json:"ip"`        //端口所在IP，如果没填，就获取本地内网IP
	Ws       *WebsocketConfig `json:"ws"`        //websocket启动模式:off不启动;:1234启动监听
	Websocket *WebsocketConfig `json:"websocket"` // WebSocket 详细配置
	Lua      string           `json:"lua"`       //off不启动，填写lua文件的相对路径启动lua
	LuaConfig *LuaConfig      `json:"lua_config"` // Lua 详细配置
	LogLevel string           `json:"log_level"` //日志等级，off为不开,其次为INFO,DEBUG,WARN,ERROR
	MapPath  string           `json:"map_path"`  //地图配置位置
	Web      *WebConfig       `json:"web"`       //web配置
	RpcPort  *RpcConfig       `json:"rpc_port"`  //rpc_port端口，没有则表示不开rpc服务
	Rpc      *RpcConfig       `json:"rpc"`       // gRPC 配置
	Telegram *TelegramConfig  `json:"telegram"`  //telegram配置
	Server   *ServerConfig    `json:"server"`    // 服务器全局配置
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
	var path1 string
	if p, err := ini.Load(_defaultFile); err == nil {
		path1 = path.Join(_basePath, "/conf/", p.GetString(cfgname, "ini", ""))
	}

	file, err := os.ReadFile(path1)
	if err != nil {
		panic("读取启动配置出错:" + err.Error())
	}
	// 注意：不打印配置内容，避免密码等敏感信息泄露到日志

	err = json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path1 + ":" + err.Error())
	}
}

var (
	Cfg_         *Cfg = nil
	ServerName_  string
	_basePath    = path.Join(path.Dir(os.Args[0]), "../")
	_defaultFile = path.Join(_basePath, "conf/config.ini")
	_defServerId = flag.String("s", "default", "server flag") //默认服务器ID
)

func GetBasePath() string {
	return _basePath
}

func GetDefaultFie() string {
	return _defaultFile
}

func GetServerID() string {
	return *_defServerId
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
