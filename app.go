package touchgocore

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/db"
	"touchgocore/db/dbmap"
	"touchgocore/localtimer"
	"touchgocore/mapmanager"
	"touchgocore/rpc"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"

	"touchgocore/ini"
	"touchgocore/syncmap"
)

// ==================== 依赖注入容器 ====================

// App 是框架的核心容器，聚合所有模块实例
// 通过依赖注入替代全局变量，提高可测试性和可维护性
type App struct {
	mu sync.RWMutex

	// 配置
	ServerName string
	Cfg        *config.Cfg

	// 服务实例（按需初始化）
	Redis    *db.Redis
	MySQL    *db.Client
	MongoDB  *db.DbOperate
	TimerMgr *localtimer.TimerManager
	CallFunc *util.CallFunction

	// 模块注册表（优先于此读取，全局变量作为 fallback）
	rpcServers *syncmap.Map[string, *rpc.RpcServer]
	rpcClients *syncmap.Map[string, *rpc.RpcClient]
	// wsClients 用分片表：每条消息派发都要按 uid 查一次客户端，几十个连接的读协程
	// 挤在一把 RWMutex 上时，锁排队直接算进消息延迟（S69 实测并行读快 2.4~2.8 倍）。
	wsClients *syncmap.ShardedMap[int64, *websocket.Client]
	databases *syncmap.MapAny

	// 上下文和取消
	ctx    context.Context
	cancel context.CancelFunc

	// 服务启停状态
	services []Service
	started  bool
}

// Service 定义服务生命周期接口
type Service interface {
	// Name 返回服务名称
	Name() string
	// Start 启动服务
	Start(ctx context.Context) error
	// Stop 停止服务
	Stop(ctx context.Context) error
}

// ==================== App 方法 ====================

// NewApp 创建新的App容器
func NewApp(serverName string) (*App, error) {
	app := &App{
		ServerName: serverName,
		rpcServers: syncmap.NewMap[string, *rpc.RpcServer](),
		rpcClients: syncmap.NewMap[string, *rpc.RpcClient](),
		wsClients:  syncmap.NewShardedMap[int64, *websocket.Client](0),
		databases:  syncmap.NewAny(),
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())
	app.CallFunc = util.DefaultCallFunc

	rpc.UseRegistry(app.rpcServers, app.rpcClients)
	websocket.UseShardedClientMap(app.wsClients)
	dbmap.UseAppRegistry(app.databases)

	// 加载配置
	if err := app.loadConfig(); err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	// 初始化日志
	app.initLogger()

	// 设置CPU核数
	app.setupMaxProcs()

	// 初始化数据库
	if err := app.initDatabase(); err != nil {
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}

	// 注册服务（按依赖顺序）
	app.registerServices()

	app.ctx = corectx.WithAppView(app.ctx, &corectx.AppView{
		ServerName: app.ServerName,
		Cfg:        app.Cfg,
	})
	globalApp = app
	return app, nil
}

// loadConfig 加载配置
func (app *App) loadConfig() error {
	// 使用改进后的Load方法（返回error而非panic）
	if err := config.Cfg_.LoadWithError(app.ServerName); err != nil {
		return err
	}
	config.ServerName_ = app.ServerName
	app.Cfg = config.Cfg_
	app.Cfg.Normalize()
	if err := app.Cfg.Validate(); err != nil {
		return fmt.Errorf("配置校验失败: %w", err)
	}

	//读取INI
	if p, err := ini.Load(config.GetDefaultFile()); err == nil {
		util.DEBUG = p.GetString("GLOBAL", "debug", "false") == "true"
		util.Fps, _ = strconv.Atoi(p.GetString("GLOBAL", "fps", "120"))
		util.Version = p.GetString(app.ServerName, "Version", "1.0")
		util.GameGroup = p.GetString("GLOBAL", "GameGroup", "default")
		//告诉具体代码加载
		util.DefaultCallFunc.Do(util.CallLoadIni, p)
	}

	return nil
}

// initLogger 初始化日志系统
func (app *App) initLogger() {
	logLevel := "info"
	if app.Cfg != nil && app.Cfg.LogLevel != "" {
		logLevel = app.Cfg.LogLevel
	}
	vars.Run(filepath.Join(config.GetBasePath(), "log"), app.ServerName, logLevel)

	centerstr := "*         Service:[" + config.ServerName_ + "] Version:[" + util.Version + "]         *"
	var sb strings.Builder
	sb.Grow(len(centerstr))
	for range len(centerstr) {
		sb.WriteByte('*')
	}
	showsr := sb.String()
	vars.Info("%s", showsr)
	vars.Info("%s", centerstr)
	vars.Info("%s", showsr)
}

// setupMaxProcs 按配置设置 CPU 核数；未配置则保留运行时默认值
func (app *App) setupMaxProcs() {
	if app.Cfg == nil || app.Cfg.Server == nil || app.Cfg.Server.MaxProcs <= 0 {
		vars.Info("加载核心配置, GOMAXPROCS 使用运行时默认值: %d", runtime.GOMAXPROCS(0))
		return
	}
	prev := runtime.GOMAXPROCS(app.Cfg.Server.MaxProcs)
	vars.Info("加载核心配置, GOMAXPROCS: %d -> %d", prev, app.Cfg.Server.MaxProcs)
}

// initDatabase 初始化数据库连接
func (app *App) initDatabase() error {
	// Redis（必选）
	if app.Cfg.Redis != nil {
		redis, err := db.NewRedis(app.Cfg.Redis)
		if err != nil {
			return fmt.Errorf("加载redis配置出错: %w", err)
		}
		app.Redis = redis
		vars.Info("加载redis配置成功")

		// 初始化虚拟时间模块
		util.InitVirtualTime(redis.Get())
	}
	// } else {
	// 	return fmt.Errorf("加载配置出错,没有redis配置")
	// }

	// MySQL（可选）
	if app.Cfg.MySql != nil {
		vars.Info("开启MySqlDB功能")
		// 组装连接选项：默认沿用框架既有行为（loc=Local）；仅当显式配置了 db_loc 时才追加 DSN 参数覆盖时区。
		mysqlOpts := []db.Option{db.WithMetricsHook(db.NewMetricsAdapter())}
		if loc := strings.TrimSpace(app.Cfg.MySql.Loc); loc != "" {
			mysqlOpts = append(mysqlOpts, db.WithDSNParam("loc", loc))
			vars.Info("MySql 连接时区(loc)设置为: %s", loc)
		}
		mysql, err := db.NewMySql(app.Cfg.MySql, mysqlOpts...)
		if err != nil {
			return fmt.Errorf("加载MySql配置出错: %w", err)
		}
		app.MySQL = mysql
		vars.Info("加载MySql数据成功")
	}

	// MongoDB（可选）
	if app.Cfg.Mongo != nil {
		vars.Info("开启Mongo功能")
		mongo, err := db.NewMongoDB(app.Cfg.Mongo)
		if err != nil {
			return fmt.Errorf("加载Mongo配置出错: %w", err)
		}
		app.MongoDB = mongo
		vars.Info("加载Mongo数据成功")
	}

	return nil
}

// registerServices 注册所有服务（按依赖顺序）
func (app *App) registerServices() {
	app.services = []Service{
		&timerService{},   // 定时器最先启动，其他服务依赖
		&metricsService{}, // Metrics 监控
		&websocketService{},
		// 地图早于 Lua：RunMap 负责把 Npc 类注册进 Lua 运行时并登记地图，
		// 脚本里 Npc() / SetMapId 都要求这两件事已经就绪
		&mapService{},
		&luaService{},
		&rpcService{},
		&telegramService{},
		&ginService{},
		&modelAPIService{},
	}
}

// Start 启动所有服务
func (app *App) Start() error {
	app.mu.Lock()
	defer app.mu.Unlock()

	if app.started {
		return fmt.Errorf("app already started")
	}

	// 按顺序启动服务
	startedCount := 0
	for _, svc := range app.services {
		if err := svc.Start(app.ctx); err != nil {
			// 回滚已启动的服务，同样给独立预算，避免卡死在退出路上
			for i := startedCount - 1; i >= 0; i-- {
				_ = stopService(app.services[i], minServiceStopBudget)
			}
			return fmt.Errorf("启动服务[%s]失败: %w", svc.Name(), err)
		}
		startedCount++
		vars.Info("服务[%s]启动成功", svc.Name())
	}

	// NPC 由 Lua 脚本创建并挂图，全部服务起来之后才有完整对象可校验；
	// 放在业务 CallStart 之前，让配置问题先于业务初始化暴露。只告警不阻断启动。
	if problems := mapmanager.ValidateNpcs(); len(problems) > 0 {
		vars.Warning("NPC 配置校验共发现 %d 处问题，请按上面的逐条告警修正", len(problems))
	}

	// 执行业务层初始化回调
	util.DefaultCallFunc.Do(util.CallStart)

	app.started = true
	vars.Info("touchgocore启动完成")
	return nil
}

// minServiceStopBudget 单个服务停止的最小时间预算，
// 防止服务多或总预算小导致靠后的服务只分到接近 0 的时间而被误判为超时。
const minServiceStopBudget = 2 * time.Second

// Shutdown 优雅关闭所有服务（反向顺序）
func (app *App) Shutdown(timeout time.Duration) error {
	app.mu.Lock()
	defer app.mu.Unlock()

	if !app.started {
		app.cancel()
		app.closeDatabase()
		return nil
	}

	deadline := time.Now().Add(timeout)

	// 先取消 app.ctx：各服务的 Run 循环都靠 ctx.Done 退出，
	// 若等到 Stop 之后再取消，Stop 等循环退出、循环等 Stop 取消，会互相卡住。
	app.cancel()

	var errs []error
	for i := len(app.services) - 1; i >= 0; i-- {
		if err := stopService(app.services[i], serviceStopBudget(deadline, i+1)); err != nil {
			errs = append(errs, err)
		}
	}

	// 执行业务层关闭回调
	util.DefaultCallFunc.Do(util.CallStop)

	app.closeDatabase()

	// 日志最后关闭，保证上面的关闭过程都能落盘
	vars.Shutdown()

	app.started = false

	if len(errs) > 0 {
		return fmt.Errorf("关闭过程中发生错误: %v", errs)
	}
	return nil
}

// serviceStopBudget 把总剩余时间按待停服务数均分，并保证最小预算
func serviceStopBudget(deadline time.Time, remainingServices int) time.Duration {
	if remainingServices < 1 {
		remainingServices = 1
	}
	budget := time.Until(deadline) / time.Duration(remainingServices)
	if budget < minServiceStopBudget {
		return minServiceStopBudget
	}
	return budget
}

// stopService 在独立预算内停止服务；超时不阻塞后续服务，
// done 带缓冲，确保迟到的返回值不会让协程永久阻塞。
func stopService(svc Service, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- svc.Stop(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			wrapped := fmt.Errorf("停止服务[%s]出错: %w", svc.Name(), err)
			vars.Error("%v", wrapped)
			return wrapped
		}
		vars.Info("服务[%s]已停止", svc.Name())
		return nil
	case <-ctx.Done():
		timedOut := fmt.Errorf("停止服务[%s]超时(%s)", svc.Name(), budget)
		vars.Error("%v", timedOut)
		return timedOut
	}
}

func (app *App) closeDatabase() {
	if app.MongoDB != nil {
		app.MongoDB.DBClose()
		app.MongoDB = nil
	}
	if app.MySQL != nil {
		if err := app.MySQL.Close(); err != nil {
			vars.Error("关闭MySQL失败: %v", err)
		}
		app.MySQL = nil
	}
	if app.Redis != nil {
		util.StopVirtualTime()
		app.Redis.Close()
		app.Redis = nil
	}
}

// GetApp 获取当前 App 实例（全局单例，向后兼容）。
//
// Deprecated: 新代码应通过 NewApp 返回值或 corectx.AppViewFrom(ctx) 获取依赖。
var globalApp *App

func GetApp() *App {
	return globalApp
}

func (app *App) Context() context.Context {
	return app.ctx
}

// GetRpcClient 从 App registry 取客户端，未命中则 fallback 全局。
func (app *App) GetRpcClient(name string) *rpc.RpcClient {
	if app != nil && app.rpcClients != nil {
		if c, ok := app.rpcClients.Load(name); ok {
			return c
		}
	}
	return rpc.GetRpcClient(name)
}

// GetRpcServer 从 App registry 取服务端，未命中则 fallback 全局。
func (app *App) GetRpcServer(name string) *rpc.RpcServer {
	if app != nil && app.rpcServers != nil {
		if s, ok := app.rpcServers.Load(name); ok {
			return s
		}
	}
	return rpc.GetRpcServer(name)
}

// GetWSClient 按 UID 取 WebSocket 客户端，未命中则 fallback 全局。
func (app *App) GetWSClient(uid int64) *websocket.Client {
	if app != nil && app.wsClients != nil {
		if c, ok := app.wsClients.Load(uid); ok {
			return c
		}
	}
	return websocket.GetClient(uid)
}
