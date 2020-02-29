package impl

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TouchGoCore/touchgocore/vars"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
)

type ConnCallback interface {
	OnConnect(*Connection) bool
	OnMessage(*Connection, interface{}) bool
	OnClose(*Connection)
}

type connCallBack struct {
	fn           func(c *Connection, body []byte)
	callbackChan chan byte //
}

type Connection struct {
	enterPort     int                     //
	wsConnect     *websocket.Conn         //
	remoteAddr    string                  //
	outChan       chan []byte             //
	closeChan     chan byte               //
	isClosed      bool                    // 防止closeChan被关闭多次
	connCallback  ConnCallback            //通常回调函数
	connCallback2 map[int64]*connCallBack //特定执行的回调函数
	Uid           int64                   //全局用唯一ID
}

type dataerr struct {
	err string
}

func (this *dataerr) Error() string {
	return this.err
}

var maxUid int64 = 0

func InitConnection(port int, wsConn *websocket.Conn, remoteAddr string, callback ConnCallback) (*Connection, error) {
	maxUid++
	conn := &Connection{
		enterPort:     port,
		wsConnect:     wsConn,
		outChan:       make(chan []byte, 1000),
		closeChan:     make(chan byte, 1),
		connCallback:  callback,
		connCallback2: make(map[int64]*connCallBack),
		isClosed:      false,
		remoteAddr:    "",
		Uid:           maxUid,
	}
	if remoteAddr != "" {
		conn.remoteAddr = remoteAddr
	}
	if !conn.connCallback.OnConnect(conn) {
		conn.Close("连接初始化失败")
		return nil, &dataerr{"连接出错"}
	}

	vars.Info("%s创建连接成功！", remoteAddr)

	//执行
	go conn.handleLoop()
	go conn.writeLoop()

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

func (s *Connection) SendMsg(pb proto.Message, msgId int32, cbid int64) {
	if !s.IsClose() {
		data, err := proto.Marshal(pb)
		if err != nil {
			vars.Error(err.Error())
		}

		s.Write(data, msgId, cbid)
	} else {
		vars.Error("服务器连接还没创建上！！！")
	}
}

func (s *Connection) SendMsgByMust(pb proto.Message, msgId int32, cbid int64) {
	if !s.IsClose() {
		data, err := proto.Marshal(pb)
		if err != nil {
			vars.Error(err.Error())
		}

		protocol := NewEchoPacket(data, msgId, cbid)
		s.wsConnect.WriteMessage(websocket.BinaryMessage, protocol.Serialize())
	}
}

func (s *Connection) Write(buffer []byte, msgId int32, cbid int64) {
	if s.IsClose() {
		return
	}
	protocol := NewEchoPacket(buffer, msgId, cbid)
	s.WriteMessage(protocol.Serialize())
}

func (conn *Connection) WriteMessage(data []byte) (err error) {

	select {
	case conn.outChan <- data:
	case <-conn.closeChan:
		err = errors.New("connection is closeed")
	}
	return
}

func (conn *Connection) Close(desc string) {
	// 线程安全，可多次调用
	conn.wsConnect.Close()

	if !conn.isClosed {
		conn.isClosed = true
		close(conn.closeChan)
		close(conn.outChan)
		conn.connCallback.OnClose(conn)
		if desc != "" {
			vars.Info(desc)
			conn.wsConnect.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(10000, desc))
		}
	}
}

func (conn *Connection) handleLoop() {
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
		//解析操作
		data := &EchoPacket{buff: data}
		cbid := int64(data.GetCbid())
		if conn.connCallback2[cbid] == nil {
			if !conn.connCallback.OnMessage(conn, data) {
				return
			}
		} else {
			conn.connCallback2[cbid].fn(conn, data.GetBody())
			conn.connCallback2[cbid].fn = nil
			conn.connCallback2[cbid].callbackChan <- 1
		}
	}
}

func (conn *Connection) writeLoop() {
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
		//写数据
		select {
		case data = <-conn.outChan:
			if conn.IsClose() {
				return
			}

			if err = conn.wsConnect.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		case <-conn.closeChan:
			return
		}
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
func WsListenAndServe(port int, call ConnCallback) {
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
		InitConnection(port, wsConn, proxy_add_x_forwarded_for, call)
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

func (s *Client) SendMsg(pb proto.Message, msgId int32, cbid int64, Fn func(c *Connection, body []byte)) {
	cbid1 := cbid
	if Fn != nil {
		maxUid++
		cbid1 = maxUid
		s.conn.connCallback2[cbid1] = &connCallBack{
			fn:           Fn,
			callbackChan: make(chan byte, 1),
		}
	}
	s.conn.SendMsg(pb, msgId, cbid1)
	//等待数据回复
	if Fn != nil {
		for {
			select {
			case <-s.conn.connCallback2[cbid1].callbackChan:
				delete(s.conn.connCallback2, cbid)
				return
			case <-time.After(time.Second * 5):
				return
			}
		}
	}
}

func (s *Client) Write(buffer []byte, msgId int32, cbid int64) {
	s.conn.Write(buffer, msgId, cbid)
}

//主动连接
func (this *Client) Connection1(ipstring string, call ConnCallback) error {
	this.closed = true
	// var addr = flag.String("addr", ipstring, "http service address")
	u := url.URL{Scheme: "ws", Host: ipstring, Path: "/ws"}
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err == nil {
		host := strings.Split(ipstring, ":")
		if len(host) != 2 {
			return &dataerr{"获取连接端口出错"}
		}
		port, _ := strconv.Atoi(host[1])
		if this.conn, err = InitConnection(port, c, "", call); err != nil {
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

func (this *Client) Connection(ip string, port int, call ConnCallback) error {
	str := ip + ":" + strconv.Itoa(port)
	return this.Connection1(str, call)
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
