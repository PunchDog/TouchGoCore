package touchgocore

import (
	"github.com/TouchGoCore/touchgocore/rpc"
	"github.com/TouchGoCore/touchgocore/time"
	"github.com/TouchGoCore/touchgocore/vars"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

//总体开关
func Run(serverName string, version string) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("捕获错误:", err)
		}
	}()

	//启动默认数据
	conf := map[string]interface{}{
		"-b": "./bus.json",
		"-l": "info",
	}

	//获取附加数据
	idx := 0
	for idx < len(os.Args) {
		if n := strings.Index(os.Args[idx], "-"); n > -1 {
			conf[os.Args[idx]] = os.Args[idx+1]
			idx += 2
		} else {
			idx++
		}
	}

	vars.Info("*********************************************")
	vars.Info("           系统:[%s]版本:[%s]", serverName, version)
	vars.Info("*********************************************")

	//创建日志文件
	vars.Run(serverName, conf["-l"].(string))

	//启动rpc相关
	rpc.Run(serverName, conf["-b"].(string))

	//启动timer定时器
	vars.Info("启动计时器")
	go time.TimerManager_.Tick()

	//启动完成
	vars.Info("启动附加配置：", conf)
	vars.Info("touchgocore启动完成")

	go func() {
		chSig := make(chan os.Signal)
		signal.Notify(chSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		vars.Info("Signal: ", <-chSig)
		rpc.Stop() //关闭通道
		os.Exit(-1)
	}()
}
