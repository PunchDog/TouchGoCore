package touchgocore

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"touchgocore/beegoweb"
	"touchgocore/config"
	"touchgocore/db"
	lua "touchgocore/gopherlua"
	"touchgocore/mapmanager"
	time1 "touchgocore/time"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"
)

var chExit chan bool
var chExitOk chan int

func init() {
	chExit = make(chan bool)
	chExitOk = make(chan int)
}

// 总体开关,此函数需要放在main的最后
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

	//创建日志文件
	vars.Run(config.ServerName_, config.Cfg_.LogLevel)

	vars.Info("*********************************************")
	vars.Info("           系统:[%s]版本:[%s]", serverName, version)
	vars.Info("*********************************************")

	//设置核数
	runtime.GOMAXPROCS(runtime.NumCPU())

	//加载配置
	vars.Info("加载核心配置")

	//检查redis
	if config.Cfg_.Redis == nil {
		panic("加载配置出错，没有redis配置")
	}
	if _, err := db.NewRedis(config.Cfg_.Redis); err != nil {
		panic("加载配置出错，没有redis配置:" + err.Error())
	}
	vars.Info("加载redis配置成功")

	//检查DB
	if config.Cfg_.MySql != nil {
		vars.Info("开启MySqlDB功能")
		if _, err := db.NewDbMysql(config.Cfg_.MySql); err != nil {
			panic("加载配置出错:" + err.Error())
		}
		db.MySqlRun()
		vars.Info("加载MySql数据成功")
	}

	//检查DB
	if config.Cfg_.Mongo != nil {
		vars.Info("开启Mongo功能")
		if _, err := db.NewMongoDB(config.Cfg_.Mongo); err != nil {
			panic("加载配置出错:" + err.Error())
		}
		vars.Info("加载Mongo数据成功")
	}

	//启动timer定时器
	time1.Run()

	//启动rpc相关
	// rpc.Run()

	//启动ws
	websocket.Run()

	//读取地图
	mapmanager.Run()

	//启动lua脚本
	lua.Run()

	//启动beego
	beegoweb.Run()

	//核心加载完了后自己想执行的东西
	util.DefaultCallFunc.Do(util.CallStart)

	// //启动其他进程
	// if config.Cfg_.ServerType == "exec" {
	// 	for _, dllpath := range config.Cfg_.DllList {
	// 		//根据不同的操作系统来启动程序
	// 		path := dllpath
	// 		switch runtime.GOOS {
	// 		case "windows":
	// 			path += ".exe"
	// 		case "macos":
	// 			path = fmt.Sprintf("env GOTRACEBACK=crash nohup %s &", dllpath)
	// 		case "linux":
	// 			path = fmt.Sprintf("env GOTRACEBACK=crash nohup %s &", dllpath)
	// 		}

	// 		cmd := exec.Command(path, os.Args...)
	// 		_, err := cmd.CombinedOutput()
	// 		if err != nil {
	// 			vars.Error("启动附加进程%s失败:%s", dllpath, err)
	// 			continue
	// 		}
	// 		vars.Info("成功启动进程:%s", dllpath)
	// 	}
	// }

	//启动完成
	vars.Info("touchgocore启动完成")

	//进程监控
	go signalProcHandler()

	//主循环
	for {
		if err := loop(); err != nil {
			break
		}
		<-time.After(time.Nanosecond * 10)
	}
}

func loop() (err interface{}) {
	defer func() {
		if err = recover(); err != nil {
			vars.Error(err)
			chExitOk <- (-1)
		}
	}()
	err = nil
	select {
	case <-time1.Tick():
	case <-lua.TimeTick():
	case <-websocket.Handle():
	case <-chExit:
		err = &util.Error{ErrMsg: "退出服务器"}
		chExitOk <- (0)
	default:
	}
	return
}

func signalProcHandler() {
	//开阻塞
	chSig := make(chan os.Signal)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	vars.Info("Signal: ", chSig)
	chExit <- true

	// rpc.Stop()       //关闭通道
	lua.Stop()       //关闭lua定时器
	time1.Stop()     //关闭定时器
	websocket.Stop() //关闭websock

	//退出时清理工作
	util.DefaultCallFunc.Do(util.CallStop)

	exitNum := <-chExitOk

	os.Exit(exitNum)
}
