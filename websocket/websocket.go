package websocket

import (
	"fmt"
	"strconv"
	"strings"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"
)

var (
	closeCh  chan bool          = nil
	msgQueue chan *msgQueueType = nil
	call     ICall              = nil
)

type msgQueueType struct {
	uid  int64
	data []byte
}

func RegisterCall(call1 ICall) {
	call = call1
}

func Run() {
	if config.Cfg_.Ws == nil || call == nil {
		return
	}
	closeCh = make(chan bool)
	msgQueue = make(chan *msgQueueType, 102400)
	clientpool = util.NewPoolMgr()

	//启动监听
	list := strings.Split(config.Cfg_.Ws.Port, "|")
	for _, szp := range list {
		port, _ := strconv.Atoi(szp)
		if port == 0 {
			continue
		}
		err := ListenAndServe(port)
		if err != nil {
			vars.Error(fmt.Sprintf("websocket服务启动端口%d监听失败:%s", port, err.Error()))
			continue
		}
	}
	vars.Info("websocket服务启动")
}

func Stop() {
	close(closeCh)
}

func Tick() chan bool {
	select {
	case <-closeCh:
		//关闭所有服务器
		for _, server := range serverList {
			server.Close()
		}
		//关闭所有客户端
		clientmap.Range(func(key, value interface{}) bool {
			client := value.(*Client)
			client.Close("")
			return true
		})

		//关闭消息队列
		close(msgQueue)
		return nil
	case read_msg := <-msgQueue:
		// 	处理消息队列
		if c, h := clientmap.Load(read_msg.uid); h {
			msg1 := util.PasreFSMessage(read_msg.data)
			if msg1 != nil {
				call.OnMessage(c.(*Client), msg1)
			}
		}
	}
	return nil
}
