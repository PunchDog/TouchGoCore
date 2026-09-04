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
	"touchgocore/gin"
	lua "touchgocore/golua"
	"touchgocore/localtimer"
	"touchgocore/mapmanager"
	"touchgocore/rpc"
	"touchgocore/telegram"
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
	MySQL    *db.DbMysql
	MongoDB  *db.DbOperate
	TimerMgr *localtimer.TimerManager
	CallFunc *util.CallFunction

	// 模块注册表（优先于此读取，全局变量作为 fallback）
	rpcServers *syncmap.Map[string, *rpc.RpcServer]
	rpcClients *syncmap.Map[string, *rpc.RpcClient]
	wsClients  *syncmap.Map[int64, *websocket.Client]
	databases  *syncmap.MapAny

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

// ==================== 服务适配器 ====================

// timerService 定时器服务适配器
type timerService struct{}

func (s *timerService) Name() string { return "timer" }
func (s *timerService) Start(ctx context.Context) error {
	localtimer.Run(ctx)
	return nil
}
func (s *timerService) Stop(ctx context.Context) error {
	localtimer.TimeStop(ctx)
	return nil
}

// websocketService WebSocket服务适配器
type websocketService struct{}

func (s *websocketService) Name() string { return "websocket" }
func (s *websocketService) Start(ctx context.Context) error {
	return websocket.Run(ctx)
}
func (s *websocketService) Stop(ctx context.Context) error {
	websocket.Stop(ctx)
	return nil
}

// luaService Lua脚本服务适配器
type luaService struct{}

func (s *luaService) Name() string { return "lua" }
func (s *luaService) Start(ctx context.Context) error {
	return lua.Run(ctx)
}
func (s *luaService) Stop(ctx context.Context) error {
	lua.Stop(ctx)
	return nil
}

// rpcService gRPC服务适配器
type rpcService struct{}

func (s *rpcService) Name() string { return "rpc" }
func (s *rpcService) Start(ctx context.Context) error {
	return rpc.Run(ctx)
}
func (s *rpcService) Stop(ctx context.Context) error {
	rpc.Stop(ctx)
	return nil
}

// telegramService Telegram Bot服务适配器
type telegramService struct{}

func (s *telegramService) Name() string { return "telegram" }
func (s *telegramService) Start(ctx context.Context) error {
	telegram.TelegramStart(ctx)
	return nil
}
func (s *telegramService) Stop(ctx context.Context) error {
	telegram.TelegramStop(ctx)
	return nil
}

// mapService 地图管理服务适配器
type mapService struct{}

func (s *mapService) Name() string { return "map" }
func (s *mapService) Start(ctx context.Context) error {
	return mapmanager.RunMap(ctx)
}
func (s *mapService) Stop(ctx context.Context) error {
	mapmanager.StopMap(ctx)
	return nil
}

// ginService Gin HTTP服务适配器
type ginService struct{}

func (s *ginService) Name() string { return "gin" }
func (s *ginService) Start(ctx context.Context) error {
	return gin.Run(ctx)
}
func (s *ginService) Stop(ctx context.Context) error {
	return gin.Stop(ctx)
}

// ==================== App 方法 ====================

// NewApp 创建新的App容器
func NewApp(serverName string) (*App, error) {
	app := &App{
		ServerName: serverName,
		rpcServers: syncmap.NewMap[string, *rpc.RpcServer](),
		rpcClients: syncmap.NewMap[string, *rpc.RpcClient](),
		wsClients:  syncmap.NewMap[int64, *websocket.Client](),
		databases:  syncmap.NewAny(),
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())
	app.CallFunc = util.DefaultCallFunc

	rpc.UseRegistry(app.rpcServers, app.rpcClients)
	websocket.UseClientMap(app.wsClients)
	db.UseRegistry(app.databases)

	// 加载配置
	if err := app.loadConfig(); err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	// 初始化日志
	app.initLogger()

	// 设置CPU核数
	runtime.GOMAXPROCS(0)
	vars.Info("加载核心配置")

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
		mysql, err := db.NewDbMysql(app.Cfg.MySql)
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
		&luaService{},
		&rpcService{},
		&telegramService{},
		&mapService{},
		&ginService{},
	}
}

// metricsService Prometheus监控服务适配器
type metricsService struct{}

func (s *metricsService) Name() string { return "metrics" }
func (s *metricsService) Start(ctx context.Context) error {
	cfg := corectx.CfgFrom(ctx)
	if cfg != nil {
		StartMetrics(cfg.Metrics)
	}
	return nil
}
func (s *metricsService) Stop(ctx context.Context) error {
	ShutdownMetrics(ctx)
	return nil
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
			for i := startedCount - 1; i >= 0; i-- {
				_ = app.services[i].Stop(app.ctx)
			}
			return fmt.Errorf("启动服务[%s]失败: %w", svc.Name(), err)
		}
		startedCount++
		vars.Info("服务[%s]启动成功", svc.Name())
	}

	// 执行业务层初始化回调
	util.DefaultCallFunc.Do(util.CallStart)

	app.started = true
	vars.Info("touchgocore启动完成")
	return nil
}

// Shutdown 优雅关闭所有服务（反向顺序）
func (app *App) Shutdown(timeout time.Duration) error {
	app.mu.Lock()
	defer app.mu.Unlock()

	if !app.started {
		app.closeDatabase()
		return nil
	}

	// 创建带超时的关闭上下文
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 反向关闭服务
	var errs []error
	for i := len(app.services) - 1; i >= 0; i-- {
		svc := app.services[i]
		done := make(chan error, 1)
		go func() {
			done <- svc.Stop(ctx)
		}()

		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("停止服务[%s]出错: %w", svc.Name(), err))
			}
			vars.Info("服务[%s]已停止", svc.Name())
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("停止服务[%s]超时", svc.Name()))
		}
	}

	// 执行业务层关闭回调
	util.DefaultCallFunc.Do(util.CallStop)

	app.closeDatabase()

	vars.Shutdown()

	app.started = false
	app.cancel()

	if len(errs) > 0 {
		return fmt.Errorf("关闭过程中发生错误: %v", errs)
	}
	return nil
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
