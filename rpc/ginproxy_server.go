package rpc

import (
	"context"
	"fmt"
	"strings"
	"touchgocore/gin"
	"touchgocore/network/message"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
)

// isUpperHTTPMethod 判定方法段是否为合法的大写 HTTP 方法（与 config.validateGinPath 同口径）。
func isUpperHTTPMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		return true
	}
	return false
}

// ============================================================================
// 反向 gRPC 路由代理：网关服务端侧。
//
// 服务端与 gin 同进程。收到客户端的注册帧后，把路由写进 gin 分发表，条目持一个
// 指向本客户端会话流的 sessionProxy；网页请求经 gin 根劫持命中该条目时，gin 调
// sessionProxy.Do 把请求编码进流转发给客户端、按 request_id 等待响应、再回写网页端。
// ============================================================================

// handleGinProxyFrame 在服务端 Msg 循环内就地处理一帧代理内部消息（不进 handler 流水线）。
func (s *RpcServer) handleGinProxyFrame(cs *clientSession, clientNameKey string, reqID uint64, sub int32, msg *message.FSMessage) {
	switch sub {
	case ginSubRegister:
		reg := &message.GinRouteRegistration{}
		if err := proto.Unmarshal(msg.GetBody(), reg); err != nil {
			vars.Error("RPC服务端解析路由注册失败[%s]: %v", clientNameKey, err)
			s.sendGinRegisterAck(cs, clientNameKey, reqID, false, "invalid registration body")
			return
		}
		// A-F3 配套闸门：本会话已被同名新会话顶替时拒绝注册。旧连接上迟到的注册帧
		// 若放行，会把条目的 session 代际写回旧值，旧会话随后的双键回收又会误删，
		// 新会话路由静默 404。会话流按名解析的 sessionProxy 本身无此问题。
		if cur, ok := s.nametoclientstream.Load(clientNameKey); !ok || cur != cs {
			vars.Error("RPC服务端拒绝过期会话的路由注册[%s] session=%d（当前会话已切换）", clientNameKey, cs.id)
			s.sendGinRegisterAck(cs, clientNameKey, reqID, false, "stale session, registration rejected")
			return
		}
		proxy := &sessionProxy{server: s, clientNameKey: clientNameKey}
		n := 0
		for _, r := range reg.GetRoutes() {
			if r == nil {
				continue
			}
			// A-F2：注册前逐条复核格式，非法条目跳过并留痕，不影响同帧其余合法条目。
			// 客户端共享 token 鉴权，上报内容按不可信处理。
			path, method := r.GetUrlPath(), r.GetMethod()
			if !strings.HasPrefix(path, "/") {
				vars.Error("RPC服务端拒绝非法路由注册[%s] session=%d: path=%q 必须以 / 开头", clientNameKey, cs.id, path)
				continue
			}
			if method != "" && !isUpperHTTPMethod(method) {
				vars.Error("RPC服务端拒绝非法路由注册[%s] session=%d: %q method=%q 不是大写 HTTP 方法", clientNameKey, cs.id, path, method)
				continue
			}
			gin.RegisterGRPCRoute(clientNameKey, cs.id, path, method, proxy)
			n++
		}
		vars.Info("RPC服务端注册反向路由[%s] session=%d 共%d条", clientNameKey, cs.id, n)
		s.sendGinRegisterAck(cs, clientNameKey, reqID, true, fmt.Sprintf("registered %d routes", n))

	case ginSubProxyResp:
		rsp := &message.GinHTTPResponse{}
		if err := proto.Unmarshal(msg.GetBody(), rsp); err != nil {
			vars.Error("RPC服务端解析代理响应失败[%s] request_id=%d: %v", clientNameKey, reqID, err)
			return
		}
		v, ok := cs.proxyPending.Load(reqID)
		if !ok {
			// 等待方已超时退出/会话已切换的残留响应：丢弃并留痕，参照客户端 recvLoop 的 orphan 处理。
			vars.Error("RPC服务端收到无法关联的代理响应[%s] request_id=%d", clientNameKey, reqID)
			return
		}
		select {
		case v.(chan *message.GinHTTPResponse) <- rsp:
		default:
		}

	default:
		vars.Error("RPC服务端收到未知代理子码[%s] protocol2=%d request_id=%d", clientNameKey, sub, reqID)
	}
}

// sendGinRegisterAck 回一帧注册确认（复用 request_id 让客户端 pending 匹配）。
// 必须走 cs 自己的流而不是按名解析「当前会话」：注册帧可能来自已被顶替的旧会话
// （stale 拒绝路径），按名发送会把回执发到新会话的流上，旧客户端只能干等超时。
func (s *RpcServer) sendGinRegisterAck(cs *clientSession, clientNameKey string, reqID uint64, ok bool, m string) {
	body, err := proto.Marshal(&message.GinRegisterAck{Ok: ok, Msg: m})
	if err != nil {
		vars.Error("RPC服务端序列化注册回执失败[%s]: %v", clientNameKey, err)
		return
	}
	frame := newGinProxyFrame(ginSubRegisterAck, reqID, body)
	cs.sendMu.Lock()
	err = cs.stream.Send(frame)
	cs.sendMu.Unlock()
	if err != nil {
		vars.Error("RPC服务端回注册回执失败[%s]: %v", clientNameKey, err)
	}
}

// abandonProxyPending 作废本会话所有在途代理等待：向每个等待 chan 投 nil，
// 让 sessionProxy.Do 立即失败（gin 侧回 502），不让网页端干等到超时。
func (cs *clientSession) abandonProxyPending() {
	cs.proxyPending.Range(func(key, value any) bool {
		cs.proxyPending.Delete(key)
		if ch, ok := value.(chan *message.GinHTTPResponse); ok {
			select {
			case ch <- nil:
			default:
			}
		}
		return true
	})
}

// sessionProxy 实现 gin.GRPCProxyStream：把命中的 HTTP 请求转发给远端客户端会话。
// 只持服务端与客户端名，Do 时按名解析「当前」会话，天然应对同名重连后的会话替换。
type sessionProxy struct {
	server        *RpcServer
	clientNameKey string
}

// Do 转发请求并阻塞等待响应，受 ctx 超时/取消约束。
func (sp *sessionProxy) Do(ctx context.Context, req *gin.ProxyRequest) (*gin.ProxyResponse, error) {
	cs, ok := sp.server.nametoclientstream.Load(sp.clientNameKey)
	if !ok {
		return nil, fmt.Errorf("gRPC 代理: 客户端会话不存在[%s]", sp.clientNameKey)
	}

	reqID := cs.proxySeq.Add(1)
	ch := make(chan *message.GinHTTPResponse, 1)
	cs.proxyPending.Store(reqID, ch)
	defer cs.proxyPending.Delete(reqID)

	body, err := proto.Marshal(ginRequestToProxy(req))
	if err != nil {
		return nil, fmt.Errorf("gRPC 代理: 序列化代理请求失败: %w", err)
	}
	frame := newGinProxyFrame(ginSubProxyReq, reqID, body)

	cs.sendMu.Lock()
	err = cs.stream.Send(frame)
	cs.sendMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("gRPC 代理: 发送代理请求失败[%s]: %w", sp.clientNameKey, err)
	}

	select {
	case rsp := <-ch:
		if rsp == nil {
			return nil, fmt.Errorf("gRPC 代理: 会话已断开，请求作废[%s]", sp.clientNameKey)
		}
		return ginResponseToProxy(rsp), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
