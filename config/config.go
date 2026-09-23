package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	Whatsapp *WhatsappConfig `json:"whatsapp"` //WhatsApp 通道（登录/充值/提现）配置
	Usdt     *UsdtConfig     `json:"usdt"`     //USDT(TRC20) 通道（充值/提现）配置
	// PaySDks 是资金 SDK 集中登记表，键是 SDK 段名；whatsapp/usdt/telegram.ton
	// 三段各自用 {sdk, account} 引用这里的一段。凭证只在这里出现一次。
	PaySDks  map[string]*PaySDKConfig `json:"pay_sdks"`
	Server   *ServerConfig            `json:"server"`    // 服务器全局配置
	Metrics  *MetricsConfig           `json:"metrics"`   // Prometheus 监控配置
	ModelAPI *ModelAPIConfig          `json:"model_api"` // 模型API接入配置（OpenAI 兼容接口）
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

	// 解析功能配置文件夹
	_confDirField = strings.TrimSpace(p.GetString(cfgname, "conf_dir", ""))
	if _confDirField != "" {
		resolveFeatureDir()
		// 先收集所有已注册的 key，避免在迭代中做 I/O
		var registered []string
		_featureReg.Range(func(key, value any) bool {
			registered = append(registered, key.(string))
			return true
		})
		// 逐个加载
		for _, name := range registered {
			target, _ := _featureReg.Load(name)
			if loadErr := loadFeatureConfig(name, target); loadErr != nil {
				return loadErr
			}
		}
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

	// 功能配置注册系统
	_confDirField   string    // INI 中 conf_dir 字段值
	_featureDir     string    // 功能配置文件夹绝对路径
	_featureDirSet  bool      // 功能配置文件夹是否已解析
	_featureReg     sync.Map  // key=jsonname, value=注册的目标 struct 指针或 *map[string]any
	_featureData    sync.Map  // key=jsonname, value=已加载的数据（struct 指针或 map[string]any）
	_featureLoaded  sync.Map  // key=jsonname, value=bool
)

func GetBasePath() string {
	return _basePath
}

// GetConfDir 返回 conf 目录（config.ini 与各服 JSON 所在目录）。
func GetConfDir() string {
	return _confDir
}

// Deprecated: 拼写错误（Fie 应为 File），请使用 GetDefaultFile。
// 正确拼写的版本已存在且被使用，本函数仅为兼容既有外部调用保留。
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

// resolveFeatureDir 解析功能配置文件夹路径
// 相对路径基于 _confDir，绝对路径直接使用
func resolveFeatureDir() {
	if _featureDirSet {
		return
	}
	if _confDirField == "" {
		return
	}
	if filepath.IsAbs(_confDirField) {
		_featureDir = _confDirField
	} else {
		_featureDir = filepath.Join(_confDir, _confDirField)
	}
	_featureDirSet = true
}

// normalizeJSONName 统一 JSON 文件名格式，确保带 .json 后缀
func normalizeJSONName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	return name
}

// RegisterFunc 注册功能配置。
// jsonname: JSON 文件名（不含路径，如 "game_rules.json" 或 "game_rules"）
// target: 目标 struct 指针；传 nil 时按 map[string]any 读取
//
// 调用时机：LoadWithError 之前或之后均可。
//   - 之前注册：LoadWithError 加载主配置后自动加载所有已注册的 JSON
//   - 之后注册：立即加载对应 JSON 文件
func RegisterFunc(jsonname string, target any) error {
	name := normalizeJSONName(jsonname)

	// 检查是否已注册
	if _, loaded := _featureReg.LoadOrStore(name, target); loaded {
		return fmt.Errorf("功能配置已注册: %s", name)
	}

	// 如果功能配置文件夹已解析，立即加载
	if _featureDirSet {
		return loadFeatureConfig(name, target)
	}

	return nil
}

// loadFeatureConfig 加载单个功能配置文件
func loadFeatureConfig(jsonname string, target any) error {
	name := normalizeJSONName(jsonname)
	if _featureDir == "" {
		return fmt.Errorf("功能配置文件夹未配置")
	}

	path := filepath.Join(_featureDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取功能配置出错 [%s]: %w", path, err)
	}

	if target == nil {
		// 按 map[string]any 读取
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("解析功能配置出错 [%s]: %w", path, err)
		}
		_featureData.Store(name, m)
	} else {
		// 按 struct 读取
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("解析功能配置出错 [%s]: %w", path, err)
		}
		_featureData.Store(name, target)
	}

	_featureLoaded.Store(name, true)
	return nil
}

// GetFeatureConfig 获取已加载的功能配置
// jsonname 与 RegisterFunc 中一致
func GetFeatureConfig(jsonname string) (any, bool) {
	name := normalizeJSONName(jsonname)
	return _featureData.Load(name)
}

// GetFeatureConfigMap 获取未注册 struct 的通用 map 配置
func GetFeatureConfigMap(jsonname string) (map[string]any, bool) {
	name := normalizeJSONName(jsonname)
	v, ok := _featureData.Load(name)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// GetFeatureDir 返回功能配置文件夹路径
func GetFeatureDir() string {
	return _featureDir
}

// IsFeatureLoaded 检查功能配置是否已加载
func IsFeatureLoaded(jsonname string) bool {
	name := normalizeJSONName(jsonname)
	v, ok := _featureLoaded.Load(name)
	if !ok {
		return false
	}
	return v.(bool)
}
