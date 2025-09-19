package websocket

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

const (
	UPGRADER_READ_BUFFER_SIZE  = 1024 * 1024 * 10
	UPGRADER_WRITE_BUFFER_SIZE = 1024 * 1024 * 10
)

var (
	serverList []*http.Server = make([]*http.Server, 0)
	upgrader                  = websocket.Upgrader{
		ReadBufferSize:  UPGRADER_READ_BUFFER_SIZE,
		WriteBufferSize: UPGRADER_WRITE_BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

type ICall interface {
	// 创建连接时的处理
	OnConnect(client *Client) bool
	//收到消息时的处理
	OnMessage(client *Client, message proto.Message)
	// 关闭连接时的处理
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
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		vars.Error("获取客户端IP失败", err)
		ip = "127.0.0.1"
	}
	return ip
}

// 监听端口
func ListenAndServe(port int) error {
	r := gin.Default()
	r.GET("/ws", func(c *gin.Context) {
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
		if wsConn, err = upgrader.Upgrade(c.Writer, c.Request, nil); err != nil {
			vars.Error("路径/ws链接错误", err)
			http.NotFound(c.Writer, c.Request)
			return
		}

		_, err = NewClient(wsConn, getClientIP(c.Request))
		if err != nil {
			vars.Error("", err)
			return
		}
	})

	//websocket实现ipv6
	server := &http.Server{
		Addr:    "[::]:" + strconv.Itoa(port),
		Handler: r,
	}

	go func() { //异步启动
		server.ListenAndServe()
	}()
	serverList = append(serverList, server)
	//将服务器名字注册到redis中
	return nil
}
