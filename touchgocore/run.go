package touchgocore

import (
	"fmt"
	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/fileserver"
	"github.com/PunchDog/TouchGoCore/touchgocore/lua"
	"github.com/PunchDog/TouchGoCore/touchgocore/rpc"
	"github.com/PunchDog/TouchGoCore/touchgocore/time"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	impl "github.com/PunchDog/TouchGoCore/touchgocore/websocket_impl"
	"os"
	"os/signal"
	"syscall"
)

var ExitFunc_ func() = nil

//总体开关,此函数需要放在main的最后
func Run(serverName string, version string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Sprintf("捕获错误:", err)
		}
	}()

	if len(os.Args) == 1 {
		panic("启动参数不足")
	}

	config.ServerName_ = serverName
	config.Cfg_.Load(os.Args[1])

	vars.Info("*********************************************")
	vars.Info("           系统:[%s]版本:[%s]", serverName, version)
	vars.Info("*********************************************")
	//加载配置
	vars.Info("加载核心配置")

	//创建日志文件
	vars.Run(config.ServerName_, config.Cfg_.LogLevel)

	//检查redis
	if config.Cfg_.Redis == nil {
		panic("加载配置出错，没有redis配置")
	}
	if _, err := db.NewRedis(config.Cfg_.Redis); err != nil {
		panic("加载配置出错，没有redis配置:" + err.Error())
	}
	vars.Info("加载redis配置成功")

	//检查DB
	if config.Cfg_.Db != nil {
		vars.Info("开启DB功能")
		if _, err := db.NewDbMysql(config.Cfg_.Db); err != nil {
			panic("加载配置出错，没有db配置:" + err.Error())
		}
		db.Run()
		vars.Info("加载DB数据成功")
	}

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

	//go func() {
	chSig := make(chan os.Signal)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	vars.Info("Signal: ", <-chSig)
	rpc.Stop()            //关闭通道
	if ExitFunc_ != nil { //退出时清理工作
		ExitFunc_()
	}
	os.Exit(-1)
	//}()
}
