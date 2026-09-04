package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// RpcPort 历史字段，与 Rpc 二选一；Validate/Normalize 会将其归并到 Rpc。
	//
	// Deprecated: 请使用 json:"rpc"。
	RpcPort  *RpcConfig      `json:"rpc_port"`
	Rpc      *RpcConfig      `json:"rpc"`      // gRPC 配置
	Telegram *TelegramConfig `json:"telegram"` //telegram配置
	Server   *ServerConfig   `json:"server"`   // 服务器全局配置
	Metrics  *MetricsConfig  `json:"metrics"`  // Prometheus 监控配置
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
	flag.StringVar(&_configDirFlag, "c", "", "conf 目录路径（内含 config.ini 与各服 JSON），与 --config 相同")
	flag.StringVar(&_configDirFlag, "config", "", "conf 目录路径，同 -c")
	resolveConfigPaths()
}

// ApplyFlags 解析命令行并重新解析配置目录。Run / LoadWithError 会调用。
func ApplyFlags() {
	if !flag.Parsed() {
		flag.Parse()
	}
	resolveConfigPaths()
}

// resolveConfigPaths 解析 conf 目录。
// 优先级：-c/--config > 环境变量 CONFIG_PATH > 可执行文件旁/上级/CWD 自动查找。
func resolveConfigPaths() {
	if dir := strings.TrimSpace(_configDirFlag); dir != "" {
		applyConfigDir(dir)
		return
	}
	if p := strings.TrimSpace(os.Getenv("CONFIG_PATH")); p != "" {
		applyConfigDir(p)
		return
	}

	execDir := filepath.Dir(os.Args[0])
	candidates := []string{
		filepath.Join(execDir, ".."),
		filepath.Join(execDir, "..", ".."),
		".",
	}
	for _, base := range candidates {
		conf := filepath.Join(base, "conf")
		if PathExists(filepath.Join(conf, "config.ini")) {
			setConfDir(conf)
			return
		}
	}
}

// applyConfigDir 接受 conf 目录本身，或包含 conf/ 的基目录（兼容 CONFIG_PATH）。
func applyConfigDir(p string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)

	switch {
	case PathExists(filepath.Join(p, "config.ini")):
		setConfDir(p)
	case PathExists(filepath.Join(p, "conf", "config.ini")):
		setConfDir(filepath.Join(p, "conf"))
	case strings.EqualFold(filepath.Base(p), "conf"):
		setConfDir(p)
	default:
		setConfDir(filepath.Join(p, "conf"))
	}
}

func setConfDir(confDir string) {
	_confDir = confDir
	_defaultFile = filepath.Join(confDir, "config.ini")
	_basePath = filepath.Dir(confDir)
}

// Load 加载配置文件（兼容旧接口，内部调用LoadWithError）。
//
// Deprecated: 请使用 LoadWithError，由调用方决定错误处理，避免进程直接 panic。
func (this *Cfg) Load(cfgname string) {
	if err := this.LoadWithError(cfgname); err != nil {
		panic(err)
	}
}

// LoadWithError 加载配置文件（返回error而非panic）
// 推荐使用此方法替代Load，便于上层决定错误处理策略
func (this *Cfg) LoadWithError(cfgname string) error {
	ApplyFlags()
	p, err := ini.Load(_defaultFile)
	if err != nil {
		return fmt.Errorf("读取ini失败 [%s]: %w", _defaultFile, err)
	}
	iniName := strings.TrimSpace(p.GetString(cfgname, "ini", ""))
	if iniName == "" {
		return fmt.Errorf("配置文件路径为空, 服务器名: %s, ini: %s", cfgname, _defaultFile)
	}
	path1 := filepath.Join(_confDir, iniName)

	file, err := os.ReadFile(path1)
	if err != nil {
		return fmt.Errorf("读取启动配置出错: %w", err)
	}

	if err := json.Unmarshal(file, this); err != nil {
		return fmt.Errorf("解析配置出错[%s]: %w", path1, err)
	}

	return nil
}

// Normalize 将 rpc_port 别名归并到 Rpc，供启动与校验统一读取。
func (c *Cfg) Normalize() {
	if c == nil {
		return
	}
	if c.Rpc == nil && c.RpcPort != nil {
		c.Rpc = c.RpcPort
	}
}

// RpcOf 返回生效的 RPC 配置（优先 Rpc，其次 RpcPort）。
func (c *Cfg) RpcOf() *RpcConfig {
	if c == nil {
		return nil
	}
	if c.Rpc != nil {
		return c.Rpc
	}
	return c.RpcPort
}

// QueueCapacity 消息队列容量；未配置时用 defaultSize。
func (c *Cfg) QueueCapacity(defaultSize int) int {
	if defaultSize <= 0 {
		defaultSize = 4096
	}
	if c != nil && c.Server != nil && c.Server.ReadBuffer > 0 {
		return c.Server.ReadBuffer
	}
	return defaultSize
}

// WriteQueueCapacity 写队列容量。
func (c *Cfg) WriteQueueCapacity(defaultSize int) int {
	if defaultSize <= 0 {
		defaultSize = 4096
	}
	if c != nil && c.Server != nil && c.Server.WriteBuffer > 0 {
		return c.Server.WriteBuffer
	}
	return defaultSize
}

// DropOnFull 为 true 时队列满丢弃消息（背压）。
func (c *Cfg) DropOnFull() bool {
	return c != nil && c.Server != nil && c.Server.Backpressure
}

var (
	// Cfg_ 全局配置单例。
	//
	// Deprecated: 新代码应从 App.Cfg 或 corectx.CfgFrom(ctx) 读取。
	Cfg_           *Cfg = nil
	ServerName_    string
	_configDirFlag string
	_basePath      string
	_confDir       string
	_defaultFile   string
	_defServerId   = flag.String("s", "default", "server flag") //默认服务器ID
)

func GetBasePath() string {
	return _basePath
}

// GetConfDir 返回 conf 目录（config.ini 与各服 JSON 所在目录）。
func GetConfDir() string {
	return _confDir
}

func GetDefaultFie() string {
	return GetDefaultFile()
}

// GetDefaultFile 返回 config.ini 路径（GetDefaultFie 的正确拼写）。
func GetDefaultFile() string {
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
