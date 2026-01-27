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
	"touchgocore/db"
	"touchgocore/gin"
	lua "touchgocore/golua"
	"touchgocore/ini"
	"touchgocore/localtimer"
	"touchgocore/rpc"
	"touchgocore/telegram"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"
)

// 总体开关,此函数需要放在main的最后
func Run(serverName string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("程序发生panic错误: %v", err)
		}
	}()

	//解析命令行参数
	flag.Parse()

	// 加载配置
	if err := loadConfig(serverName); err != nil {
		fmt.Printf("加载配置失败: %v", err)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	// 初始化日志系统
	initLogger()

	// 设置核数
	setCPUNum()

	// 初始化数据库连接
	if err := initDatabase(); err != nil {
		vars.Error("初始化数据库失败: %v", err)
		<-time.After(time.Millisecond * 100)
		os.Exit(1)
	}

	// 启动服务
	startServices()

	// 核心加载完了后自己想执行的东西
	_, _ = util.DefaultCallFunc.Do(util.CallStart)

	// 启动完成
	vars.Info("touchgocore启动完成")

	// 进程监控
	signalProcHandler()
}

func closeServer() {
	lua.Stop()            //关闭lua定时器
	localtimer.TimeStop() //关闭定时器
	websocket.Stop()      //关闭websock
	rpc.Stop()            //关闭gRPC
	telegram.TelegramStop()

	//退出时清理工作
	_, _ = util.DefaultCallFunc.Do(util.CallStop)

	//关闭日志系统
	vars.Shutdown()

	vars.Info("关闭完成,退出服务器")
}

func signalProcHandler() {
	//开阻塞
	chSig := make(chan os.Signal, 1)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	sig := <-chSig
	vars.Info("Signal: %v", sig)
	closeServer()
}

// loadConfig 加载配置文件
func loadConfig(serverName string) error {
	config.ServerName_ = serverName
	config.Cfg_.Load(serverName)

	//读取INI
	if p, err := ini.Load(config.GetDefaultFie()); err == nil {
		util.DEBUG = p.GetString("GLOBAL", "debug", "false") == "true"
		util.Fps, _ = strconv.Atoi(p.GetString("GLOBAL", "fps", "120"))
		util.Version = p.GetString(serverName, "Version", "1.0")
	}

	return nil
}

// initLogger 初始化日志系统
func initLogger() {
	//创建日志文件
	vars.Run(path.Join(config.GetBasePath(), "/log"), config.ServerName_, config.Cfg_.LogLevel)

	centerstr := "*         Service:[" + config.ServerName_ + "] Version:[" + util.Version + "]         *"
	l := len(centerstr)
	var showsr string
	for i := 0; i < l; i++ {
		showsr = showsr + "*"
	}
	vars.Info(showsr)
	vars.Info(centerstr)
	vars.Info(showsr)
}

// setCPUNum 设置CPU核数
func setCPUNum() {
	//设置核数
	// runtime.GOMAXPROCS(runtime.NumCPU())
	runtime.GOMAXPROCS(0)
	vars.Info("加载核心配置")
}

// initDatabase 初始化数据库连接
func initDatabase() error {
	//检查redis
	if config.Cfg_.Redis != nil {
		if _, err := db.NewRedis(config.Cfg_.Redis); err != nil {
			return fmt.Errorf("加载redis配置出错: %w", err)
		}
		vars.Info("加载redis配置成功")
	} else {
		return fmt.Errorf("加载配置出错,没有redis配置")
	}

	//检查DB
	if config.Cfg_.MySql != nil {
		vars.Info("开启MySqlDB功能")
		if _, err := db.NewDbMysql(config.Cfg_.MySql); err != nil {
			return fmt.Errorf("加载MySql配置出错: %w", err)
		}
		// db.MySqlRun()
		vars.Info("加载MySql数据成功")
	}

	//检查DB
	if config.Cfg_.Mongo != nil {
		vars.Info("开启Mongo功能")
		if _, err := db.NewMongoDB(config.Cfg_.Mongo); err != nil {
			return fmt.Errorf("加载Mongo配置出错: %w", err)
		}
		vars.Info("加载Mongo数据成功")
	}

	return nil
}

// startServices 启动所有服务
func startServices() {
	//启动timer定时器
	localtimer.Run()

	//启动ws
	websocket.Run()

	//启动lua脚本
	lua.Run()

	//启动gin
	gin.Run()

	// 启动gRPC服务
	rpc.Run()

	//启动telegram
	telegram.TelegramStart()
}
