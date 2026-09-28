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
	Cache     *CacheConfig     `json:"cache"`      // 两级缓存（Redis+DB 回源）；nil=不启用
	Ws        *WebsocketConfig `json:"ws"`         //websocket启动模式:off不启动;:1234启动监听详细配置
	LuaConfig *LuaConfig       `json:"lua_config"` // Lua 详细配置,如果没有就不启动lua
	LogLevel  string           `json:"log_level"`  //日志等级，off为不开,其次为INFO,DEBUG,WARN,ERROR
	MapPath   string           `json:"map_path"`   //地图配置位置
	Web       *WebConfig       `json:"web"`        //web配置
	Rpc       *RpcConfig       `json:"rpc"`        // gRPC 配置
	Telegram  *TelegramConfig  `json:"telegram"`   //telegram配置
	Whatsapp  *WhatsappConfig  `json:"whatsapp"`   //WhatsApp 通道（登录/充值/提现）配置
	Usdt      *UsdtConfig      `json:"usdt"`       //USDT(TRC20) 通道（充值/提现）配置
	Tron      *TronConfig      `json:"tron"`       //TRON 原生币(TRX) 通道（充值/提现）配置
	Bsc       *BscConfig       `json:"bsc"`        //BSC 原生币(BNB) 通道（充值/提现）配置
	Sol       *SolConfig       `json:"sol"`        //Solana 原生币(SOL) 通道（充值/提现）配置
	// PaySDks 是资金 SDK 集中登记表，键是 SDK 段名；whatsapp/usdt/telegram.ton/tron/bsc/sol
	// 各段用 {sdk, account} 引用这里的一段。凭证只在这里出现一次。
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
		Other:    nil,
		Telegram: nil,
	}
	flag.StringVar(&_configDirFlag, "c", "", "conf 目录路径（内含 config.ini 与各服 JSON），与 --config 相同")
	flag.StringVar(&_configDirFlag, "config", "", "conf 目录路径，同 -c")
	resolveConfigPaths()
}

// ApplyFlags 解析命令行并重新解析配置目录。Run / LoadWithError 会调用。
func ApplyFlags() {
	// flag.Parse 会顺着注册时交出去的指针直接写 _configDirFlag 与 *_defServerId，
	// 这两处写入不经过本包任何代码，所以解析动作本身也要落在写锁里，
	// 否则它与 configDirFlag()/GetServerID() 的读就是并发读写。
	_stateMu.Lock()
	if !flag.Parsed() {
		flag.Parse()
	}
	_stateMu.Unlock()
	resolveConfigPaths()
}

// resolveConfigPaths 解析 conf 目录。
// 优先级：-c/--config > 环境变量 CONFIG_PATH > 可执行文件旁/上级/CWD 自动查找。
func resolveConfigPaths() {
	if dir := strings.TrimSpace(configDirFlag()); dir != "" {
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

// setConfDir 整组更新 conf 目录派生出的三个路径。三者必须同时可见：
// 只更新 _confDir 而 _defaultFile 还是旧值时，LoadWithError 会拿旧 ini 配新目录。
func setConfDir(confDir string) {
	_stateMu.Lock()
	_confDir = confDir
	_defaultFile = filepath.Join(confDir, "config.ini")
	_basePath = filepath.Dir(confDir)
	_stateMu.Unlock()
}

// configDirFlag 读取 -c/--config 的当前值（写方是 flag.Parse，见 ApplyFlags）。
func configDirFlag() string {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _configDirFlag
}

// pathSnapshot 成组取出 conf 目录与 config.ini 路径，供一次加载全程复用，
// 避免加载途中另一协程改了路径导致 ini 与主 JSON 来自不同目录。
func pathSnapshot() (confDir, defaultFile string) {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _confDir, _defaultFile
}

// setConfDirField 记录 INI 里 conf_dir 字段的原始值。
func setConfDirField(v string) {
	_stateMu.Lock()
	_confDirField = v
	_stateMu.Unlock()
}

// featureDirSnapshot 成组取出功能配置目录及其「是否已解析」标记。
// 两者必须一起读：只读 _featureDir 会把「目录已置位但值为空」与「尚未解析」混为一谈。
func featureDirSnapshot() (dir string, isSet bool) {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _featureDir, _featureDirSet
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
// 推荐使用此方法替代Load，便于上层决定错误处理策略。
//
// 并发安全：整个加载过程持 _loadMu 串行化——ini、主 JSON、功能 JSON 是三次独立读盘，
// 必须看到同一份路径快照；调用方还常共享同一个 *Cfg（如 config.Cfg_），
// 并发 json.Unmarshal 到同一目标本身就是数据竞争。路径与功能目录状态另由 _stateMu 保护。
func (this *Cfg) LoadWithError(cfgname string) error {
	_loadMu.Lock()
	defer _loadMu.Unlock()

	ApplyFlags()
	confDir, defaultFile := pathSnapshot()
	p, err := ini.Load(defaultFile)
	if err != nil {
		return fmt.Errorf("读取ini失败 [%s]: %w", defaultFile, err)
	}
	iniName := strings.TrimSpace(p.GetString(cfgname, "ini", ""))
	if iniName == "" {
		return fmt.Errorf("配置文件路径为空, 服务器名: %s, ini: %s", cfgname, defaultFile)
	}
	path1 := filepath.Join(confDir, iniName)

	file, err := os.ReadFile(path1)
	if err != nil {
		return fmt.Errorf("读取启动配置出错: %w", err)
	}

	if err := json.Unmarshal(file, this); err != nil {
		return fmt.Errorf("解析配置出错[%s]: %w", path1, err)
	}

	// 解析功能配置文件夹
	confDirField := strings.TrimSpace(p.GetString(cfgname, "conf_dir", ""))
	setConfDirField(confDirField)
	if confDirField != "" {
		resolveFeatureDir()
		// 整轮固定一份功能目录：既避开逐个配置重复加锁读全局，也保证同一批配置
		// 一定来自同一个目录（即使将来有人在加载途中改动了状态）。
		featureDir, _ := featureDirSnapshot()
		// 先收集所有已注册的 key，避免在迭代中做 I/O
		var registered []string
		_featureReg.Range(func(key, value any) bool {
			registered = append(registered, key.(string))
			return true
		})
		// 逐个加载
		for _, name := range registered {
			target, _ := _featureReg.Load(name)
			if loadErr := loadFeatureConfigFrom(featureDir, name, target); loadErr != nil {
				return loadErr
			}
		}
	}

	return nil
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
	_confDirField  string   // INI 中 conf_dir 字段值（读写均持 _stateMu）
	_featureDir    string   // 功能配置文件夹绝对路径（读写均持 _stateMu）
	_featureDirSet bool     // 功能配置文件夹是否已解析（读写均持 _stateMu）
	_featureReg    sync.Map // key=jsonname, value=注册的目标 struct 指针或 *map[string]any
	_featureData   sync.Map // key=jsonname, value=已加载的数据（struct 指针或 map[string]any）
	_featureLoaded sync.Map // key=jsonname, value=bool

	// _stateMu 保护上面所有包级可变状态：_configDirFlag、_basePath、_confDir、
	// _defaultFile、*_defServerId，以及 _confDirField / _featureDir / _featureDirSet。
	//
	// 为什么是 RWMutex 而不是 atomic：读点（GetConfDir / GetDefaultFile / GetBasePath /
	// GetFeatureDir / GetServerID）分布在启动装配与运行期日志、监控、路由注册路径上，
	// 属于多协程高频只读；写点只在 init / ApplyFlags / LoadWithError 的路径解析里，低频。
	// 又因为这些字段是「成组」的（_confDir / _defaultFile / _basePath 由同一次解析得出，
	// _featureDir 与 _featureDirSet 必须同时可见），单变量 atomic 无法保证组内一致性，
	// 故用一把锁盖住整组。
	//
	// _loadMu 串行化整次配置加载与功能配置注册（见 LoadWithError / RegisterFunc 注释）。
	// 锁序固定为 _loadMu → _stateMu：只有这两个入口先持 _loadMu 再取 _stateMu，
	// 反向持锁不存在，因此不会成环；持锁期间不回调业务代码，无自锁风险。
	_stateMu sync.RWMutex
	_loadMu  sync.Mutex
)

func GetBasePath() string {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _basePath
}

// GetConfDir 返回 conf 目录（config.ini 与各服 JSON 所在目录）。
func GetConfDir() string {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _confDir
}

// GetDefaultFie 返回 config.ini 路径。
//
// Deprecated: 拼写错误（Fie 应为 File），计划于下一个大版本移除；请改用 GetDefaultFile。
// 本函数仅为兼容既有外部调用保留，行为与 GetDefaultFile 完全一致。
func GetDefaultFie() string {
	return GetDefaultFile()
}

// GetDefaultFile 返回 config.ini 路径（GetDefaultFie 的正确拼写）。
func GetDefaultFile() string {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _defaultFile
}

func GetServerID() string {
	// _defServerId 指向的值由 flag.Parse 写入，读侧同样走 _stateMu（写侧见 ApplyFlags）。
	_stateMu.RLock()
	defer _stateMu.RUnlock()
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
	// 读（_featureDirSet / _confDirField / _confDir）与写（_featureDir / _featureDirSet）
	// 必须在同一临界区，否则两个协程会基于不同的中间态各自拼出一份目录。
	_stateMu.Lock()
	defer _stateMu.Unlock()
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
//
// 并发安全：与 LoadWithError 共用 _loadMu，注册与加载互斥。缺了这把锁，同一个 target
// 可能被两条加载路径同时命中：注册方先 LoadOrStore 再读 _featureDirSet，读到已置位就立即
// 加载；而加载方的 _featureReg.Range 可能已经收走了同一个名字，于是两个协程并发
// json.Unmarshal 写同一个用户结构体——这是用户内存上的数据竞争（-race 会报，本机跑不了）。
// 持锁后注册要么整段在加载之前（只由加载方解析），要么整段在之后（只由注册方解析）。
func RegisterFunc(jsonname string, target any) error {
	name := normalizeJSONName(jsonname)

	_loadMu.Lock()
	defer _loadMu.Unlock()

	// 检查是否已注册
	if _, loaded := _featureReg.LoadOrStore(name, target); loaded {
		return fmt.Errorf("功能配置已注册: %s", name)
	}

	// 如果功能配置文件夹已解析，立即加载
	if dir, isSet := featureDirSnapshot(); isSet {
		return loadFeatureConfigFrom(dir, name, target)
	}

	return nil
}

// loadFeatureConfigFrom 用调用方给定的功能目录加载单个功能配置文件。
// 目录由调用方一次快照传入，读盘期间不再取全局，因此不会读到加载途中被另一协程
// 改写的 _featureDir（两个调用方：LoadWithError 的整轮快照、RegisterFunc 的立即加载）。
func loadFeatureConfigFrom(featureDir, jsonname string, target any) error {
	name := normalizeJSONName(jsonname)
	if featureDir == "" {
		return fmt.Errorf("功能配置文件夹未配置")
	}

	path := filepath.Join(featureDir, name)
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
	_stateMu.RLock()
	defer _stateMu.RUnlock()
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
