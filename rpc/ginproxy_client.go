package rpc

import (
	"net/http"
	"sync"
	"time"
	"touchgocore/network/message"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
)

// ============================================================================
// 反向 gRPC 路由代理：后端客户端侧。
//
// 客户端连上网关、流建立后，按配置的 ginpath 向网关注册本进程可代理的 HTTP 路由；
// 收到网关下行的代理请求时，按服务端传来的 matched_pattern + method 直接查本地回调表
// （不做二次段匹配），执行回调并把响应回传给网关。业务通过 RegisterGinProxyHandler 提供处理逻辑。
// ============================================================================

// GinProxyRequest 是回调入参：字段对齐 gin.ProxyRequest，由本文件与 proto 互转，业务不感知 proto。
type GinProxyRequest struct {
	Method         string
	Path           string
	RawQuery       string
	MatchedPattern string // 服务端命中的注册模板（含 ":name" 原样）
	Params         map[string]string
	Header         http.Header
	Body           []byte
}

// GinProxyResponse 是回调返回：字段对齐 gin.ProxyResponse。
type GinProxyResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// GinProxyHandler 处理一条代理请求并返回响应。
type GinProxyHandler func(req *GinProxyRequest) *GinProxyResponse

// ginProxyHandlers 包级回调表：canonicalKey -> handler。
// canonicalKey = "urlpath|METHOD"，METHOD 省略（通配）时以 "urlpath|" 形式存储。
var ginProxyHandlers sync.Map // map[string]GinProxyHandler

// normalizeProxyPattern 把注册模式 "urlpath|METHOD" 或 "urlpath" 归一化为 "urlpath|METHOD"（METHOD 可为空）。
func normalizeProxyPattern(pattern string) string {
	path, method := parseGinPath(pattern)
	return path + "|" + method
}

// RegisterGinProxyHandler 注册一条本地代理回调。
// pattern 形如 "urlpath|METHOD" 或 "urlpath"（省略方法=通配所有方法），urlpath 可含 ":name"，
// 但此处 urlpath 与服务端注册的模板字面必须一致：分发直接按模板字面查表，客户端不重复段匹配。
func RegisterGinProxyHandler(pattern string, h GinProxyHandler) {
	if h == nil {
		vars.Error("RegisterGinProxyHandler: pattern=%q 传入 nil handler，忽略", pattern)
		return
	}
	ginProxyHandlers.Store(normalizeProxyPattern(pattern), h)
}

// lookupGinProxyHandler 精确 "matched|METHOD" 优先，回退 "matched|"（通配）。
func lookupGinProxyHandler(matched, method string) (GinProxyHandler, bool) {
	if h, ok := ginProxyHandlers.Load(matched + "|" + method); ok {
		return h.(GinProxyHandler), true
	}
	if h, ok := ginProxyHandlers.Load(matched + "|"); ok {
		return h.(GinProxyHandler), true
	}
	return nil, false
}

// ginPathsForServer 从当前生效配置里取本客户端（按 serverName 匹配）声明的 ginpath 路由清单。
func ginPathsForServer(serverName string) []*message.GinRoute {
	rpc := activeRpcCfg()
	if rpc == nil {
		return nil
	}
	for _, cl := range rpc.Client {
		if cl == nil || cl.Name != serverName {
			continue
		}
		routes := make([]*message.GinRoute, 0, len(cl.GinPath))
		for _, item := range cl.GinPath {
			path, method := parseGinPath(item)
			if path == "" {
				continue
			}
			routes = append(routes, &message.GinRoute{UrlPath: path, Method: method})
		}
		return routes
	}
	return nil
}

// sendGinRegistration 流建立后向网关注册 ginpath 路由，并等待注册回执。
// 无 ginpath 配置时直接返回，全链路零影响。
func (c *RpcClient) sendGinRegistration() {
	if c.closed.Load() {
		return
	}
	routes := ginPathsForServer(c.serverName)
	if len(routes) == 0 {
		return
	}
	conn := c.conn.Load()
	if conn == nil {
		return
	}
	stream, err := c.ensureStream(conn)
	if err != nil {
		vars.Error("RPC客户端注册反向路由: 建流失败[%s]: %v", c.fullAddr, err)
		return
	}

	body, err := proto.Marshal(&message.GinRouteRegistration{Routes: routes})
	if err != nil {
		vars.Error("RPC客户端注册反向路由: 序列化失败[%s]: %v", c.fullAddr, err)
		return
	}

	reqID := c.nextReqID.Add(1)
	waitCh := make(chan *message.FSMessage, 1)
	c.pending.Store(reqID, waitCh)
	defer c.pending.Delete(reqID)

	frame := newGinProxyFrame(ginSubRegister, reqID, body)
	c.sendMu.Lock()
	err = stream.Send(frame)
	c.sendMu.Unlock()
	if err != nil {
		vars.Error("RPC客户端注册反向路由: 发送失败[%s]: %v", c.fullAddr, err)
		return
	}

	select {
	case recv := <-waitCh:
		if recv == nil {
			vars.Error("RPC客户端注册反向路由: 连接断开，无回执[%s]", c.fullAddr)
			return
		}
		ack := &message.GinRegisterAck{}
		if err := proto.Unmarshal(recv.GetBody(), ack); err != nil {
			vars.Error("RPC客户端注册反向路由: 解析回执失败[%s]: %v", c.fullAddr, err)
			return
		}
		if ack.GetOk() {
			vars.Info("RPC客户端注册反向路由成功[%s]: %s", c.fullAddr, ack.GetMsg())
		} else {
			vars.Error("RPC客户端注册反向路由被拒[%s]: %s", c.fullAddr, ack.GetMsg())
		}
	case <-time.After(c.timeout):
		vars.Error("RPC客户端注册反向路由: 等待回执超时[%s]", c.fullAddr)
	}
}

// handleProxyRequest 处理一条下行代理请求：查回调 → 执行 → 回传响应（request_id 原样回填）。
func (c *RpcClient) handleProxyRequest(recv *message.FSMessage) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("RPC客户端处理代理请求panic[%s]: %v", c.fullAddr, r)
		}
	}()
	req := &message.GinHTTPRequest{}
	if err := proto.Unmarshal(recv.GetBody(), req); err != nil {
		vars.Error("RPC客户端解析代理请求失败[%s]: %v", c.fullAddr, err)
		return
	}
	rid := recv.GetHead().GetRequestId()

	rsp := c.dispatchProxyHandler(req)
	body, err := proto.Marshal(rsp)
	if err != nil {
		vars.Error("RPC客户端序列化代理响应失败[%s]: %v", c.fullAddr, err)
		return
	}
	c.sendRaw(newGinProxyFrame(ginSubProxyResp, rid, body))
}

// dispatchProxyHandler 按 matched_pattern + method 查本地回调并执行；无回调回 501，panic 回 500。
func (c *RpcClient) dispatchProxyHandler(req *message.GinHTTPRequest) *message.GinHTTPResponse {
	h, ok := lookupGinProxyHandler(req.GetMatchedPattern(), req.GetMethod())
	if !ok {
		return &message.GinHTTPResponse{Status: http.StatusNotImplemented}
	}
	preq := &GinProxyRequest{
		Method:         req.GetMethod(),
		Path:           req.GetUrlPath(),
		RawQuery:       req.GetRawQuery(),
		MatchedPattern: req.GetMatchedPattern(),
		Params:         ginParamsToMap(req.GetParams()),
		Header:         ginHeadersToHTTP(req.GetHeaders()),
		Body:           req.GetBody(),
	}

	var presp *GinProxyResponse
	callErr := func() (err any) {
		defer func() { err = recover() }()
		presp = h(preq)
		return nil
	}()
	if callErr != nil {
		vars.Error("RPC客户端代理回调panic[%s] %s %s: %v", c.fullAddr, req.GetMethod(), req.GetUrlPath(), callErr)
		return &message.GinHTTPResponse{Status: http.StatusInternalServerError}
	}
	if presp == nil {
		return &message.GinHTTPResponse{Status: http.StatusInternalServerError}
	}
	status := int32(presp.Status)
	if status == 0 {
		status = http.StatusOK
	}
	return &message.GinHTTPResponse{
		Status:  status,
		Headers: httpHeadersToGin(presp.Header),
		Body:    presp.Body,
	}
}

// ginParamsToMap 把 repeated GinParam 转为 map。
func ginParamsToMap(ps []*message.GinParam) map[string]string {
	if len(ps) == 0 {
		return nil
	}
	m := make(map[string]string, len(ps))
	for _, p := range ps {
		if p == nil {
			continue
		}
		m[p.GetKey()] = p.GetValue()
	}
	return m
}

// sendRaw 在当前流上直接发送一帧（不等待响应），持 sendMu 与业务发送串行。
func (c *RpcClient) sendRaw(frame *message.FSMessage) {
	stream := c.currentStream()
	if stream == nil {
		vars.Error("RPC客户端回传代理响应: 无可用流[%s]", c.fullAddr)
		return
	}
	c.sendMu.Lock()
	err := stream.Send(frame)
	c.sendMu.Unlock()
	if err != nil {
		vars.Error("RPC客户端回传代理响应: 发送失败[%s]: %v", c.fullAddr, err)
	}
}
