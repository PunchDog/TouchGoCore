package touchgocore

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"touchgocore/config"
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

// initLogger 初始化日志系统（旧版，保留向后兼容）
// Deprecated: 请使用 App 容器自动初始化
func initLogger() {
	vars.Run(path.Join(config.GetBasePath(), "/log"), config.ServerName_, config.Cfg_.LogLevel)

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
