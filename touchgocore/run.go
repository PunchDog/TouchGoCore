package touchgocore

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/fileserver"
	"github.com/PunchDog/TouchGoCore/touchgocore/lua"
	"github.com/PunchDog/TouchGoCore/touchgocore/mapmanager"
	"github.com/PunchDog/TouchGoCore/touchgocore/rpc"
	"github.com/PunchDog/TouchGoCore/touchgocore/time"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	impl "github.com/PunchDog/TouchGoCore/touchgocore/websocket_impl"
)

var ExitFunc_ func() = nil
var StartFunc_ func() = nil

//总体开关,此函数需要放在main的最后
func Run(serverName string, version string) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("捕获错误:", err)
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

	//设置核数
	runtime.GOMAXPROCS(runtime.NumCPU())

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

	//启动timer定时器
	time.Run()

	//启动rpc相关
	rpc.Run()

	//启动ws
	impl.Run()

	//读取地图
	mapmanager.Run()

	//启动lua脚本
	lua.Run()

	//启动文件服务
	fileserver.Run()

	//核心加载完了后自己想执行的东西
	if StartFunc_ != nil {
		StartFunc_()
	}

	//启动完成
	vars.Info("touchgocore启动完成")

	//启动其他进程
	if config.Cfg_.ServerType == "exec" {
		for _, dllpath := range config.Cfg_.DllList {
			//根据不同的操作系统来启动程序
			path := dllpath
			switch runtime.GOOS {
			case "windows":
				path += ".exe"
			case "macos":
				path = fmt.Sprintf("env GOTRACEBACK=crash nohup %s &", dllpath)
			case "linux":
				path = fmt.Sprintf("env GOTRACEBACK=crash nohup %s &", dllpath)
			}

			cmd := exec.Command(path, os.Args...)
			_, err := cmd.CombinedOutput()
			if err != nil {
				vars.Error("启动附加进程%s失败:%s", dllpath, err)
				continue
			}
			vars.Info("成功启动进程:%s", dllpath)
		}
	}

	//开阻塞
	chSig := make(chan os.Signal)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	vars.Info("Signal: ", <-chSig)
	rpc.Stop()            //关闭通道
	lua.Stop()            //关闭lua定时器
	if ExitFunc_ != nil { //退出时清理工作
		ExitFunc_()
	}
	time.Stop() //关闭定时器
	os.Exit(-1)
}
