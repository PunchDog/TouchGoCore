package touchgocore

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"touchgocore/config"
	"touchgocore/ini"
	"touchgocore/util"
	"touchgocore/vars"
)

// 默认关闭超时时间
const defaultShutdownTimeout = 30 * time.Second

// 总体开关,此函数需要放在main的最后
// 内部委托给 App 容器执行，保持向后兼容
func Run(serverName string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("程序发生panic错误: %v\n", err)
		}
	}()

	//解析命令行参数
	flag.Parse()

	// 创建 App 容器（内部完成配置加载、日志初始化、数据库连接）
	app, err := NewApp(serverName)
	if err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	// 保存全局单例（向后兼容）
	SetApp(app)

	// 启动所有服务
	if err := app.Start(); err != nil {
		vars.Error("启动失败: %v", err)
		_ = app.Shutdown(10 * time.Second)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	// 进程监控（阻塞等待信号）
	signalProcHandler()
}

// signalProcHandler 信号处理
func signalProcHandler() {
	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	sig := <-chSig
	vars.Info("Signal: %v", sig)

	// 带超时的优雅关闭
	if app := GetApp(); app != nil {
		if err := app.Shutdown(defaultShutdownTimeout); err != nil {
			vars.Error("关闭出错: %v", err)
		}
	} else {
		closeServer()
	}
}

// closeServer 兼容旧的无App容器的关闭逻辑（不应再被主动调用）
func closeServer() {
	vars.Info("关闭完成,退出服务器")
}

// ==================== 配置加载辅助函数 ====================
// 以下函数由 App.loadConfig 调用，保留为独立函数以便复用

// loadConfigOnly 仅加载配置（不初始化日志和数据库，由App容器统一管理）
func loadConfigOnly(serverName string) error {
	config.ServerName_ = serverName

	// 使用改进后的Load方法（返回error而非panic）
	if err := config.Cfg_.LoadWithError(serverName); err != nil {
		return err
	}

	//读取INI
	if p, err := ini.Load(config.GetDefaultFie()); err == nil {
		util.DEBUG = p.GetString("GLOBAL", "debug", "false") == "true"
		util.Fps, _ = strconv.Atoi(p.GetString("GLOBAL", "fps", "120"))
		util.Version = p.GetString(serverName, "Version", "1.0")
	}

	return nil
}

// ==================== 旧版独立函数（向后兼容，新代码请使用 App 容器） ====================

// loadConfig 加载配置文件（旧版，保留向后兼容）
// Deprecated: 请使用 App 容器的 NewApp() 代替
func loadConfig(serverName string) error {
	return loadConfigOnly(serverName)
}

// initLogger 初始化日志系统（旧版，保留向后兼容）
// Deprecated: 请使用 App 容器自动初始化
func initLogger() {
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

// setCPUNum 设置CPU核数（旧版，保留向后兼容）
// Deprecated: 请使用 App 容器自动初始化
func setCPUNum() {
	runtime.GOMAXPROCS(0)
	vars.Info("加载核心配置")
}

// startServices 启动所有服务（旧版，保留向后兼容）
// Deprecated: 请使用 App 容器的 App.Start() 代替
func startServices() {
	if app := GetApp(); app != nil {
		// 如果App容器已初始化，由App统一管理服务启动
		return
	}
	// 以下为旧版逻辑，仅在没有App容器时使用
	vars.Error("startServices: App容器未初始化，旧版启动流程已废弃")
}
