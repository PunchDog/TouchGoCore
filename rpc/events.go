package rpc

// ============================================================================
// 事件协议：让服务端能主动推"不是任何请求的响应"的帧
//
// 【为什么需要这个文件】
// 框架原本只有一条语义：服务端回的每一帧都是某个请求的响应，靠
// Head.RequestId 与客户端的 pending 表匹配（client.go recvLoop）。
// 服务端主动推（RpcServer.Send，requestID=0）在这套语义里没有位置：
//   - 没有在途请求时 → 帧被当作"无法关联的响应"丢弃；
//   - 有在途请求时   → 帧被错投成那个业务请求的响应，业务拿到一条
//                       语义完全无关的消息（比丢弃更危险）。
//
// 【修法】引入"事件协议"白名单：
//   显式声明过的 (p1,p2) 视为事件通道，其 requestID=0 的帧不进 pending
//   匹配，直接交给 ClientCallbacks.OnEvent。
//
// 【兼容性】未注册的协议号**行为完全不变**——仍走原来的"投给任意等待者"
//   路径（那是为兼容"旧服务端不回填 request_id"保留的，不能动）。
//   所以本修复对现有 1xx/2xx/3xx 资金链路零影响。
//
// 【使用约定】
//   服务端：RpcServer.Send(clientName, p1, p2, evt) 主动推；
//   客户端：rpc.RegisterEventProtocol(p1, p2) + ClientCallbacks.OnEvent 收。
//   两侧都必须注册同一组协议号，否则客户端会把事件当垃圾帧丢弃（会打日志）。
// ============================================================================

import (
	"fmt"
	"sync"
)

var eventProtocols sync.Map // "p1:p2" -> struct{}{}

func eventKey(p1, p2 int32) string {
	return fmt.Sprintf("%d:%d", p1, p2)
}

// RegisterEventProtocol 声明某个协议号是"服务端主动推的事件"。
//
// 幂等：重复注册无副作用。必须在客户端开始收包之前调用（一般放在启动的
// 协议注册阶段，与 util.RegisterProtocolType 相邻）。
func RegisterEventProtocol(p1, p2 int32) {
	eventProtocols.Store(eventKey(p1, p2), struct{}{})
}

// UnregisterEventProtocol 撤销声明（测试与热切换用）。
func UnregisterEventProtocol(p1, p2 int32) {
	eventProtocols.Delete(eventKey(p1, p2))
}

// IsEventProtocol 判定某协议号是否已声明为事件通道。
func IsEventProtocol(p1, p2 int32) bool {
	_, ok := eventProtocols.Load(eventKey(p1, p2))
	return ok
}
