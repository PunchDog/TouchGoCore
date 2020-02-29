package touchgocore

import (
	"github.com/TouchGoCore/touchgocore/rpc"
	"github.com/TouchGoCore/touchgocore/time"
	"github.com/TouchGoCore/touchgocore/vars"
	"os"
)

//总体开关
func Run(serverName string) error {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("捕获错误:", err)
		}
	}()

	//启动默认数据
	configaddr := "./bus.json"
	loglevel := "info"

	//获取附加数据
	if len(os.Args) < 3 {
		panic("启动参数不足")
	}

	configaddr = os.Args[1]
	loglevel = os.Args[2]

	//创建日志文件
	vars.Run(serverName, loglevel)

	//启动rpc相关
	rpc.Run(serverName, configaddr)

	//启动timer定时器
	go time.TimerManager_.Tick()
	return nil
}
