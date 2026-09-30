package rpc

import (
	"net/http"
	"strings"
	"touchgocore/gin"
	"touchgocore/network/message"

	"google.golang.org/protobuf/proto"
)

// ============================================================================
// 反向 gRPC 路由代理：rpc 侧的帧编解码与消息 ⇄ gin 结构互转。
//
// 编码口径：这些帧不走 util 的全局协议注册表（不 RegisterProtocolType），而是把
// proto 消息 proto.Marshal 后塞进 FSMessage.body，用保留协议号 ProtocolGinProxy +
// protocol2 子码标识帧类型，收发两侧自行 marshal/unmarshal。因此服务端 Msg 循环与
// 客户端 recvLoop 都在进入既有流水线/pending 匹配之前，先按 protocol1 拦截分发。
// ============================================================================

// ProtocolGinProxy 是反向路由代理帧的保留协议号（protocol1），选高位独立段避开业务协议号。
const ProtocolGinProxy int32 = 900001

// protocol2 子码。
const (
	ginSubRegister    int32 = 1 // 客户端 -> 服务端：路由注册请求
	ginSubRegisterAck int32 = 2 // 服务端 -> 客户端：注册确认
	ginSubProxyReq    int32 = 3 // 服务端 -> 客户端：代理请求（转发命中的 HTTP 请求）
	ginSubProxyResp   int32 = 4 // 客户端 -> 服务端：代理响应
)

// ginProxyCmd 是代理帧 Head.Cmd 的固定标记，仅作日志定位；分发不依赖它。
const ginProxyCmd = "ginproxy"

// newGinProxyFrame 组装一帧代理内部消息。body 必须非 nil：FSMessage 的 body 为 required，
// nil 会在 Send 时 marshaling 失败（与 sendErrorPacket 同一约束）。
func newGinProxyFrame(sub int32, reqID uint64, body []byte) *message.FSMessage {
	if body == nil {
		body = []byte{}
	}
	return &message.FSMessage{
		Head: &message.Head{
			Protocol1: proto.Int32(ProtocolGinProxy),
			Protocol2: proto.Int32(sub),
			RequestId: proto.Uint64(reqID),
			Cmd:       proto.String(ginProxyCmd),
		},
		Body: body,
	}
}

// ==================== 消息 ⇄ gin 结构互转（仅 rpc 侧，gin 不感知 proto）====================

// ginHeadersToHTTP 把 repeated GinHeader 还原为 http.Header（多值追加）。
func ginHeadersToHTTP(hs []*message.GinHeader) http.Header {
	out := make(http.Header, len(hs))
	for _, h := range hs {
		if h == nil {
			continue
		}
		for _, v := range h.GetValues() {
			out.Add(h.GetKey(), v)
		}
	}
	return out
}

// httpHeadersToGin 把 http.Header 摊平成 repeated GinHeader（每个键一条，值多值）。
func httpHeadersToGin(h http.Header) []*message.GinHeader {
	out := make([]*message.GinHeader, 0, len(h))
	for k, vs := range h {
		out = append(out, &message.GinHeader{Key: k, Values: vs})
	}
	return out
}

// ginRequestToProxy 把下行代理请求解码为 gin 中立结构（服务端 Do 侧使用）。
func ginRequestToProxy(req *gin.ProxyRequest) *message.GinHTTPRequest {
	params := make([]*message.GinParam, 0, len(req.Params))
	for k, v := range req.Params {
		params = append(params, &message.GinParam{Key: k, Value: v})
	}
	return &message.GinHTTPRequest{
		Method:         req.Method,
		UrlPath:        req.Path,
		RawQuery:       req.RawQuery,
		MatchedPattern: req.MatchedPattern,
		Params:         params,
		Headers:        httpHeadersToGin(req.Header),
		Body:           req.Body,
	}
}

// ginResponseToProxy 把上行代理响应解码为 gin 中立结构（服务端收到后回写网页端）。
func ginResponseToProxy(rsp *message.GinHTTPResponse) *gin.ProxyResponse {
	return &gin.ProxyResponse{
		Status: int(rsp.GetStatus()),
		Header: ginHeadersToHTTP(rsp.GetHeaders()),
		Body:   rsp.GetBody(),
	}
}

// ==================== ginpath 配置解析 ====================

// parseGinPath 把一条 "urlpath|METHOD" 拆成路径与方法（METHOD 省略即空=通配）。
func parseGinPath(item string) (path, method string) {
	path, method, _ = strings.Cut(item, "|")
	return path, method
}
