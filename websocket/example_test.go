package websocket_test

import (
	"sync"

	"google.golang.org/protobuf/proto"

	"touchgocore/vars"
	"touchgocore/websocket"
)

// roomService 是跨连接共享的业务状态。它必须自带锁：只要把
// config.ws.worker_pool_size 配成大于 0，OnMessage 就会由 N 个 Worker 协程并发调用，
// 而 OnConnect / OnClose 无论是否启用 Worker 池都与 OnMessage 不在同一个协程上。
type roomService struct {
	mu   sync.RWMutex
	room map[int64]int64 // uid -> 房间号
}

var rooms = &roomService{room: make(map[int64]int64)}

func (s *roomService) join(uid, room int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.room[uid] = room
}

func (s *roomService) leave(uid int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.room, uid)
}

func (s *roomService) where(uid int64) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.room[uid]
	return r, ok
}

// roomCall 是每连接一个的回调实例，且实例本身来自对象池。
//
// 三点必须记住：
//   - RegisterCall 只取传入值的类型，池里给出的是 reflect.New 的零值实例，
//     注册时那个原型上的字段不会带进连接 —— 共享状态因此只能走包级变量。
//   - 实例在连接关闭后归还，下一位主人认领到的是带着上一份字段的对象，
//     所以连接级字段要在 OnConnect 显式复位、OnClose 归还前清掉引用。
//   - 下面 OnMessage 里读写 c.joined 不加锁，前提是「同一连接的回调不并发」：
//     串行模式天然成立，并行模式要 worker_pool_size>0 且 shard_by_key=true。
//     轮询派发（shard_by_key=false）时同一连接的消息可以同时在两个 Worker 上跑，
//     连连接级字段都要锁，或改用 channel 交给单协程收敛。
type roomCall struct {
	joined bool // 仅属于当前连接
}

func (c *roomCall) OnConnect(client *websocket.Client) bool {
	c.joined = false
	return true
}

func (c *roomCall) OnMessage(client *websocket.Client, _ proto.Message) {
	// 写入：连接首次收到消息时登记
	if !c.joined {
		c.joined = true
		rooms.join(client.UID, 1)
	}
	// 读取：只读取用读锁即可，别把整张表锁住再处理消息
	if room, ok := rooms.where(client.UID); ok {
		vars.Info("uid=%d 当前房间=%d", client.UID, room)
	}
}

func (c *roomCall) OnClose(client *websocket.Client) {
	// 取值要在放引用之前：*Client 同样池化，本方法返回后不得再持有它
	uid := client.UID
	rooms.leave(uid)
	c.joined = false
}

// ExampleRegisterCall 演示业务回调如何持有跨连接状态：包级的 rooms 自带锁，
// 连接级的字段随连接复位。注册本身要在 websocket.Run 之前完成。
func ExampleRegisterCall() {
	websocket.RegisterCall("RoomCall", &roomCall{})
}
