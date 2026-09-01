package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"

	"touchgocore/ini"
)

type Cfg struct {
	Redis     *RedisConfig     `json:"redis"`
	MySql     *MySqlDBConfig   `json:"mysql"`
	Mongo     *MongoDBConfig   `json:"mongo"`
	Ws        *WebsocketConfig `json:"ws"`         //websocket启动模式:off不启动;:1234启动监听详细配置
	LuaConfig *LuaConfig       `json:"lua_config"` // Lua 详细配置,如果没有就不启动lua
	LogLevel  string           `json:"log_level"`  //日志等级，off为不开,其次为INFO,DEBUG,WARN,ERROR
	MapPath   string           `json:"map_path"`   //地图配置位置
	Web       *WebConfig       `json:"web"`        //web配置
	RpcPort   *RpcConfig       `json:"rpc_port"`   //rpc_port端口，没有则表示不开rpc服务
	Rpc       *RpcConfig       `json:"rpc"`        // gRPC 配置
	Telegram  *TelegramConfig  `json:"telegram"`   //telegram配置
	Server    *ServerConfig    `json:"server"`     // 服务器全局配置
	Metrics   *MetricsConfig   `json:"metrics"`    // Prometheus 监控配置
	//其他配置
	Other interface{} `json:"other_data"` //其他配置,需要自行传入想要的数据模型
}

func init() {
	Cfg_ = &Cfg{
		Ws:       nil,
		LogLevel: "info",
		MapPath:  "off",
		RpcPort:  nil,
		Other:    nil,
		Telegram: nil,
	}

	if PathExists(_defaultFile) == false {
		_basePath = path.Join(path.Dir(os.Args[0]), "../../")
		_defaultFile = path.Join(_basePath, "conf/config.ini")
	}
}

// Load 加载配置文件（兼容旧接口，内部调用LoadWithError）
func (this *Cfg) Load(cfgname string) {
	if err := this.LoadWithError(cfgname); err != nil {
		panic(err)
	}
}

// LoadWithError 加载配置文件（返回error而非panic）
// 推荐使用此方法替代Load，便于上层决定错误处理策略
func (this *Cfg) LoadWithError(cfgname string) error {
	var path1 string
	if p, err := ini.Load(_defaultFile); err == nil {
		path1 = path.Join(_basePath, "/conf/", p.GetString(cfgname, "ini", ""))
	}

	if path1 == "" {
		return fmt.Errorf("配置文件路径为空, 服务器名: %s", cfgname)
	}

	file, err := os.ReadFile(path1)
	if err != nil {
		return fmt.Errorf("读取启动配置出错: %w", err)
	}
	fmt.Println(string(file))

	if err := json.Unmarshal(file, this); err != nil {
		return fmt.Errorf("解析配置出错[%s]: %w", path1, err)
	}

	return nil
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
