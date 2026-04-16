package touchgocore

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime"
	"sync"
	"syscall"
	"time"

	"touchgocore/config"
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
func (s *timerService) Start(_ context.Context) error {
	localtimer.Run()
	return nil
}
func (s *timerService) Stop(_ context.Context) error {
	localtimer.TimeStop()
	return nil
}

// websocketService WebSocket服务适配器
type websocketService struct{}

func (s *websocketService) Name() string { return "websocket" }
func (s *websocketService) Start(_ context.Context) error {
	websocket.Run()
	return nil
}
func (s *websocketService) Stop(_ context.Context) error {
	websocket.Stop()
	return nil
}

// luaService Lua脚本服务适配器
type luaService struct{}

func (s *luaService) Name() string { return "lua" }
func (s *luaService) Start(_ context.Context) error {
	lua.Run()
	return nil
}
func (s *luaService) Stop(_ context.Context) error {
	lua.Stop()
	return nil
}

// rpcService gRPC服务适配器
type rpcService struct{}

func (s *rpcService) Name() string { return "rpc" }
func (s *rpcService) Start(_ context.Context) error {
	rpc.Run()
	return nil
}
func (s *rpcService) Stop(_ context.Context) error {
	rpc.Stop()
	return nil
}

// telegramService Telegram Bot服务适配器
type telegramService struct{}

func (s *telegramService) Name() string { return "telegram" }
func (s *telegramService) Start(_ context.Context) error {
	telegram.TelegramStart()
	return nil
}
func (s *telegramService) Stop(_ context.Context) error {
	telegram.TelegramStop()
	return nil
}

// mapService 地图管理服务适配器
type mapService struct{}

func (s *mapService) Name() string { return "map" }
func (s *mapService) Start(_ context.Context) error {
	mapmanager.RunMap()
	return nil
}
func (s *mapService) Stop(_ context.Context) error {
	return nil
}

// ginService Gin HTTP服务适配器
type ginService struct{}

func (s *ginService) Name() string { return "gin" }
func (s *ginService) Start(_ context.Context) error {
	gin.Run()
	return nil
}
func (s *ginService) Stop(_ context.Context) error {
	return nil
}

// ==================== App 方法 ====================

// NewApp 创建新的App容器
func NewApp(serverName string) (*App, error) {
	app := &App{
		ServerName: serverName,
	}
	app.ctx, app.cancel = context.WithCancel(context.Background())

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

	return app, nil
}

// loadConfig 加载配置
func (app *App) loadConfig() error {
	return loadConfigOnly(app.ServerName)
}

// initLogger 初始化日志系统
func (app *App) initLogger() {
	vars.Run(path.Join(config.GetBasePath(), "/log"), config.ServerName_, config.Cfg_.LogLevel)

	centerstr := "*         Service:[" + config.ServerName_ + "] Version:[" + util.Version + "]         *"
	var showsr string
	for range len(centerstr) {
		showsr = showsr + "*"
	}
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
	} else {
		return fmt.Errorf("加载配置出错,没有redis配置")
	}

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
func (s *metricsService) Start(_ context.Context) error {
	StartMetrics(config.Cfg_.Metrics)
	return nil
}
func (s *metricsService) Stop(_ context.Context) error {
	ShutdownMetrics(context.Background())
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
	for _, svc := range app.services {
		if err := svc.Start(app.ctx); err != nil {
			return fmt.Errorf("启动服务[%s]失败: %w", svc.Name(), err)
		}
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

	// 关闭日志系统（最后关闭）
	vars.Shutdown()

	app.started = false
	app.cancel()

	if len(errs) > 0 {
		return fmt.Errorf("关闭过程中发生错误: %v", errs)
	}
	return nil
}

// RunWithApp 使用App容器运行（替代全局Run函数）
// 提供更可控的生命周期管理和依赖注入能力
func RunWithApp(serverName string) {
	app, err := NewApp(serverName)
	if err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	if err := app.Start(); err != nil {
		vars.Error("启动失败: %v", err)
		_ = app.Shutdown(10 * time.Second)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	// 信号处理
	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	sig := <-chSig
	vars.Info("Signal: %v", sig)

	// 优雅关闭（30秒超时）
	if err := app.Shutdown(30 * time.Second); err != nil {
		vars.Error("关闭出错: %v", err)
	}

	vars.Info("关闭完成,退出服务器")
}

// GetApp 获取当前App实例（全局单例，向后兼容）
// 注意：这仅用于过渡期，新代码应通过依赖注入获取App实例
var globalApp *App

func GetApp() *App {
	return globalApp
}

func SetApp(app *App) {
	globalApp = app
}
