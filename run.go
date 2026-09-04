package touchgocore

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"touchgocore/config"
	"touchgocore/vars"
)

// 默认关闭超时时间
const defaultShutdownTimeout = 30 * time.Second

// Run 总体开关,此函数需要放在main的最后
// 内部委托给 App 容器执行
func Run(serverName string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("程序发生panic错误: %v\n", err)
			if app := GetApp(); app != nil {
				_ = app.Shutdown(10 * time.Second)
			}
			os.Exit(2)
		}
	}()

	flag.Parse()
	config.ApplyFlags()

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

	waitSignals(app)
}

// RunWithApp 与 Run 相同，保留向后兼容
func RunWithApp(serverName string) {
	Run(serverName)
}

// waitSignals 等待退出信号。忽略 SIGHUP 以兼容 nohup。
// 第一次信号触发优雅关闭；关闭期间再次收到信号则强制退出。
func waitSignals(app *App) {
	signal.Ignore(syscall.SIGHUP)

	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(chSig)

	sig := <-chSig
	vars.Info("Signal: %v", sig)

	go func() {
		sig2 := <-chSig
		vars.Error("再次收到信号 %v，强制退出", sig2)
		os.Exit(1)
	}()

	if app != nil {
		if err := app.Shutdown(defaultShutdownTimeout); err != nil {
			vars.Error("关闭出错: %v", err)
		}
	}
	vars.Info("关闭完成,退出服务器")
}
