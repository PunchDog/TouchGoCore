package touchgocore

import (
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/fileserver"
	"github.com/TouchGoCore/touchgocore/lua"
	"github.com/TouchGoCore/touchgocore/rpc"
	"github.com/TouchGoCore/touchgocore/time"
	"github.com/TouchGoCore/touchgocore/vars"
	impl "github.com/TouchGoCore/touchgocore/websocket_impl"
	"os"
	"os/signal"
	"syscall"
)

var ExitFunc_ func() = nil

//总体开关
func Run(serverName string, version string) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("捕获错误:", err)
		}
	}()

	if len(os.Args) == 1 {
		panic("启动参数不足")
	}

	vars.Info("*********************************************")
	vars.Info("           系统:[%s]版本:[%s]", serverName, version)
	vars.Info("*********************************************")
	//加载配置
	vars.Info("加载核心配置")
	config.ServerName_ = serverName
	config.Cfg_.Load(os.Args[1])

	//创建日志文件
	vars.Run(config.ServerName_, config.Cfg_.LogLevel)

	//启动rpc相关
	rpc.Run()

	//启动timer定时器
	vars.Info("启动计时器")
	go time.TimerManager_.Tick()

	//启动ws
	impl.Run()

	//启动lua脚本
	lua.Run()

	//启动文件服务
	fileserver.Run()

	//启动完成
	vars.Info("touchgocore启动完成")

	go func() {
		chSig := make(chan os.Signal)
		signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		vars.Info("Signal: ", <-chSig)
		rpc.Stop()            //关闭通道
		if ExitFunc_ != nil { //退出时清理工作
			ExitFunc_()
		}
		os.Exit(-1)
	}()
}
