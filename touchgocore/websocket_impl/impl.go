package impl

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/util"
	"github.com/TouchGoCore/touchgocore/vars"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
)

type Connection struct {
	enterPort  int             //
	wsConnect  *websocket.Conn //
	remoteAddr string          //
	closeChan  chan byte       //
	isClosed   bool            // 防止closeChan被关闭多次
	Uid        int64           //全局用唯一ID
}

var maxUid int64 = 0

func InitConnection(port int, wsConn *websocket.Conn, remoteAddr string) (*Connection, error) {
	maxUid++
	conn := &Connection{
		enterPort:  port,
		wsConnect:  wsConn,
		closeChan:  make(chan byte, 1),
		isClosed:   false,
		remoteAddr: "",
		Uid:        maxUid,
	}
	if remoteAddr != "" {
		conn.remoteAddr = remoteAddr
	}
	if !callBack_.OnConnect(conn) {
		conn.Close("连接初始化失败")
		return nil, &util.Error{ErrMsg: "连接出错"}
	}

	vars.Info("%s创建连接成功！", remoteAddr)

	//执行
	go conn.readLoop()

	return conn, nil
}

func (this *Connection) EnterPort() int {
	return this.enterPort
}

func (this *Connection) RemoteAddr() string {
	if this.remoteAddr != "" {
		return this.remoteAddr
	}
	return this.wsConnect.RemoteAddr().String()
}
func (this *Connection) IsClose() bool {
	return this.isClosed
}

func (s *Connection) SendMsg(protocol1 int32, protocol2 int32, pb proto.Message) {
	if !s.IsClose() {
		data, err := proto.Marshal(pb)
		if err != nil {
			vars.Error(err.Error())
		}

		s.Write(protocol1, protocol2, data)
	} else {
		vars.Error("服务器连接还没创建上！！！")
	}
}

func (s *Connection) Write(protocol1 int32, protocol2 int32, buffer []byte) {
	if s.IsClose() {
		return
	}
	protocol := NewEchoPacket(protocol1, protocol2, buffer)
	select {
	case wsOnMessage_.writeChan <- &rwData{data: protocol.Serialize(), conn: s}:
	}
}

func (conn *Connection) Close(desc string) {
	// 线程安全，可多次调用
	conn.wsConnect.Close()

	if !conn.isClosed {
		conn.isClosed = true
		close(conn.closeChan)
		callBack_.OnClose(conn)
		if desc != "" {
			vars.Info(desc)
			conn.wsConnect.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(10000, desc))
		}
	}
}

func (conn *Connection) readLoop() {
	var (
		data []byte
		err  error
	)
	defer func() {
		recover()
		conn.Close("")
		runtime.Goexit()
	}()

	for {
		if conn.IsClose() {
			return
		}
		//读数据
		if _, data, err = conn.wsConnect.ReadMessage(); err != nil {
			return
		}
		wsOnMessage_.readChan <- &rwData{data, conn}
	}
}

//监听回调列表
var myserver_ map[int]*http.ServeMux = make(map[int]*http.ServeMux)

//添加监听函数
func AddListenFunc(port int, fnSrc string, fn func(w http.ResponseWriter, r *http.Request)) {
	if myserver_[port] == nil {
		myserver_[port] = http.NewServeMux()
	}
	myserver_[port].HandleFunc(fnSrc, fn)
}

//http监听
func HttpListenAndServe(port int) {
	if myserver_[port] != nil {
		go http.ListenAndServe(":"+strconv.Itoa(port), myserver_[port])
	}
}

//ws监听
func WsListenAndServe(port int) {
	AddListenFunc(port, "/ws", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				strerr := fmt.Sprintf("%s", err)
				vars.Error("异常捕获:", strerr)
			}
		}()
		var (
			wsConn   *websocket.Conn
			err      error
			upgrader = websocket.Upgrader{
				// 允许跨域
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			}
		)
		// 完成ws协议的握手操作
		// Upgrade:websocket
		if wsConn, err = upgrader.Upgrade(w, r, nil); err != nil {
			vars.Error(err.Error())
			return
		}

		proxy_add_x_forwarded_for := ""
		ips := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if len(ips) > 0 {
			proxy_add_x_forwarded_for = ips[0]
		}
		InitConnection(port, wsConn, proxy_add_x_forwarded_for)
	})
	AddListenFunc(port, "/", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				strerr := fmt.Sprintf("%s", err)
				vars.Error("异常捕获:", strerr)
			}
		}()
		var (
			upgrader = websocket.Upgrader{
				// 允许跨域
				CheckOrigin: func(r *http.Request) bool {
					return true
				},
			}
		)
		if _, err := upgrader.Upgrade(w, r, nil); err != nil {
			vars.Error(err.Error())
			return
		} else {
			vars.Info("链接/成功")
		}
	})

	HttpListenAndServe(port)
}

//客户端
type Client struct {
	conn      *Connection
	closed    bool
	connected bool
}

func (this *Client) GetConn() *Connection {
	return this.conn
}

func (s *Client) SendMsg(protocol1 int32, protocol2 int32, pb proto.Message) {
	s.conn.SendMsg(protocol1, protocol2, pb)
}

func (s *Client) Write(protocol1 int32, protocol2 int32, buffer []byte) {
	s.conn.Write(protocol1, protocol2, buffer)
}

//主动连接
func (this *Client) Connection1(ipstring string) error {
	this.closed = true
	// var addr = flag.String("addr", ipstring, "http service address")
	u := url.URL{Scheme: "ws", Host: ipstring, Path: "/ws"}
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err == nil {
		host := strings.Split(ipstring, ":")
		if len(host) != 2 {
			return &util.Error{ErrMsg: "获取连接端口出错"}
		}
		port, _ := strconv.Atoi(host[1])
		if this.conn, err = InitConnection(port, c, ""); err != nil {
			// this.conn.Close(err.Error())
			return err
		}
		this.closed = false
		this.connected = true
		vars.Info("connecting to %s", u.String())
	} else {
		vars.Error("dial:", err)
		return err
	}
	return nil

}

func (this *Client) Connection(ip string, port int) error {
	str := ip + ":" + strconv.Itoa(port)
	return this.Connection1(str)
}

func (this *Client) Connected() bool {
	return this.connected
}

func (this *Client) Close() {
	this.conn.Close("")
	this.connected = false
	this.closed = true
}

func (this *Client) IsClose() bool {
	return this.closed
}
