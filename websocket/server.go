package websocket

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"touchgocore/vars"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

var (
	serverList []*http.Server = make([]*http.Server, 0)
	upgrader                  = websocket.Upgrader{
		ReadBufferSize:  0,
		WriteBufferSize: 0,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

type ICall interface {
	OnConnect(client *Client) bool
	OnMessage(client *Client, message proto.Message)
	OnClose(client *Client)
}

func getClientIP(r *http.Request) string {
	// 优先从X-Forwarded-For解析第一个IP
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	// 次选X-Real-IP
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}
	// 最后从TCP连接获取（可能为代理IP）
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// 监听端口
func ListenAndServe(port int) error {
	var myserver = http.NewServeMux()
	myserver.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("", err.(error))
			}
		}()
		var (
			wsConn *websocket.Conn
			err    error
		)
		// 完成ws协议的握手操作
		// Upgrade:websocket
		if wsConn, err = upgrader.Upgrade(w, r, nil); err != nil {
			vars.Error("路径/ws链接错误", err)
			http.NotFound(w, r)
			return
		}

		_, err = NewClient(wsConn, getClientIP(r))
		if err != nil {
			vars.Error("", err)
			return
		}
	})
	// myserver.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	// 	defer func() {
	// 		if err := recover(); err != nil {
	// 			vars.Error("异常捕获:", err)

	// 		}
	// 	}()
	// 	if _, err := upgrader.Upgrade(w, r, nil); err != nil {
	// 		vars.Error("链接/失败:", err)
	// 		return
	// 	} else {
	// 		vars.Info("链接/成功:")
	// 	}
	// })

	//websocket实现ipv6
	server := &http.Server{
		Addr:    "[::]:" + strconv.Itoa(port),
		Handler: myserver,
	}

	err := make(chan error, 1)
	go func() {
		err <- server.ListenAndServe()
	}()
	serverList = append(serverList, server)
	//将服务器名字注册到redis中
	return <-err
}
