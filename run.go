package touchgocore

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"touchgocore/beegoweb"
	"touchgocore/config"
	"touchgocore/db"
	lua "touchgocore/gopherlua"
	"touchgocore/ini"
	"touchgocore/mapmanager"
	"touchgocore/rpc"
	"touchgocore/timelocal"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"
)

var chExit chan bool
var ChClose chan bool

var DEBUG bool
var fps int
var version string

func init() {
	chExit = make(chan bool)
	ChClose = make(chan bool)
}

// 总体开关,此函数需要放在main的最后
func Run(serverName string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
		}
	}()

	config.ServerName_ = serverName
	config.Cfg_.Load(serverName)

	//读取INI
	if p, err := ini.Load(config.GetDefaultFie()); err == nil {
		DEBUG = p.GetString("GLOBAL", "debug", "false") == "true"
		fps, _ = strconv.Atoi(p.GetString("GLOBAL", "fps", "120"))
		version = p.GetString(serverName, "Version", "1.0")
	}

	//创建日志文件
	vars.Run(config.GetBasePath()+"/log/", config.ServerName_, config.Cfg_.LogLevel)

	centerstr := fmt.Sprintf("*         Service:[%s] Version:[%s]         *", serverName, version)
	l := len(centerstr)
	var showsr string
	for i := 0; i < l; i++ {
		showsr = showsr + "*"
	}
	vars.Info(showsr)
	vars.Info(centerstr)
	vars.Info(showsr)

	//设置核数
	runtime.GOMAXPROCS(runtime.NumCPU())

	//加载配置
	vars.Info("加载核心配置")

	//检查redis
	if config.Cfg_.Redis != nil {
		if _, err := db.NewRedis(config.Cfg_.Redis); err != nil {
			vars.Error("加载配置出错,没有redis配置:" + err.Error())
			<-time.After(time.Millisecond * 10)
			panic("加载配置出错,没有redis配置:" + err.Error())
		}
		vars.Info("加载redis配置成功")
	} else {
		vars.Error("加载配置出错,没有redis配置")
		<-time.After(time.Millisecond * 10)
		panic("加载配置出错,没有redis配置")
	}

	//检查DB
	if config.Cfg_.MySql != nil {
		vars.Info("开启MySqlDB功能")
		if _, err := db.NewDbMysql(config.Cfg_.MySql); err != nil {
			vars.Error("加载MySql配置出错:" + err.Error())
			<-time.After(time.Millisecond * 10)
			panic("加载MySql配置出错:" + err.Error())
		}
		db.MySqlRun()
		vars.Info("加载MySql数据成功")
	}

	//检查DB
	if config.Cfg_.Mongo != nil {
		vars.Info("开启Mongo功能")
		if _, err := db.NewMongoDB(config.Cfg_.Mongo); err != nil {
			vars.Error("加载Mongo配置出错:" + err.Error())
			<-time.After(time.Millisecond * 10)
			panic("加载Mongo配置出错:" + err.Error())
		}
		vars.Info("加载Mongo数据成功")
	}

	//启动timer定时器
	timelocal.Run()

	//启动rpc相关
	rpc.Run()

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
	aftertime := int64(time.Second) / int64(fps) //按照帧率停顿时间
	for {
		be := time.Now().UnixNano()
		if err := loop(); err != nil {
			vars.Error(err.Error())
			break
		}
		af := time.Now().UnixNano()
		condition := af - be
		if condition < aftertime {
			<-time.After(time.Duration(aftertime - condition))
		}
	}
	<-time.After(time.Second * 2)
}

func loop() (err error) {
	defer func() {
		if r := recover(); err != nil {
			// 类型断言转换
			switch v := r.(type) {
			case string:
				err = errors.New(v) // 字符串转为 error
			case error:
				err = v // 直接使用 error 类型
			default:
				err = fmt.Errorf("unexpected panic: %v", v) // 其他类型包装为 error
			}
		}
	}()
	err = nil
	select {
	case <-chExit:
		err = errors.New("退出服务器")
	case <-ChClose:
		go closeServer()
	case <-timelocal.Tick():
	case <-websocket.Handle():
	case <-rpc.OnTick():
	default:
	}
	return
}

func closeServer() {
	lua.Stop()       //关闭lua定时器
	timelocal.Stop() //关闭定时器
	websocket.Stop() //关闭websock
	rpc.Stop()       //关闭通道

	//退出时清理工作
	util.DefaultCallFunc.Do(util.CallStop)
	chExit <- true
	vars.Info("关闭完成,退出服务器")
}

func signalProcHandler() {
	//开阻塞
	chSig := make(chan os.Signal)
	signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	vars.Info("Signal: ", <-chSig)
	closeServer()
}
