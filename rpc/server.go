package rpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/metrics"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	service_ *syncmap.Map[string, *RpcServer]
)

const (
	defaultHandlerTimeout     = 5 * time.Second
	defaultGracefulStopBudget = 10 * time.Second
	defaultDrainBudget        = 5 * time.Second
	defaultHandlerConcurrency = 256
)

var (
	handlerTimeout_     atomic.Int64
	gracefulStopBudget_ atomic.Int64
	drainBudget_        atomic.Int64
)

func init() {
	handlerTimeout_.Store(int64(defaultHandlerTimeout))
	gracefulStopBudget_.Store(int64(defaultGracefulStopBudget))
	drainBudget_.Store(int64(defaultDrainBudget))
}

// handlerTimeout 当前 handler 执行超时。
func handlerTimeout() time.Duration {
	return time.Duration(handlerTimeout_.Load())
}

// SetHandlerTimeout 设置单条消息 handler 的执行超时，d<=0 恢复默认 5 秒。
func SetHandlerTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultHandlerTimeout
	}
	handlerTimeout_.Store(int64(d))
}

// SetShutdownBudget 设置停机预算：GracefulStop 的等待上限与 in-flight handler 的排空上限。
// 传 0 或负值恢复默认（10s / 5s）。
func SetShutdownBudget(gracefulStop, drainInFlight time.Duration) {
	if gracefulStop <= 0 {
		gracefulStop = defaultGracefulStopBudget
	}
	if drainInFlight <= 0 {
		drainInFlight = defaultDrainBudget
	}
	gracefulStopBudget_.Store(int64(gracefulStop))
	drainBudget_.Store(int64(drainInFlight))
}

// ErrorCmd 是 Head.Cmd 的保留取值：该帧表示服务端处理失败，Body 为空。
const ErrorCmd = "rpc.error"

// MessageInfo 是 RPC handler 的入参，handler 签名为
// func(context.Context, *MessageInfo) proto.Message。
type MessageInfo struct {
	// Req 已解析的业务请求体
	Req proto.Message
	// ClientNameKey 发起请求的客户端名（连接标识）
	ClientNameKey string
	// Protol1 协议号一级
	Protol1 int32
	// Protol2 协议号二级
	Protol2 int32
	// RequestID 客户端请求流水号，0 表示旧客户端的串行兼容模式
	RequestID uint64
}

// clientSession 一个客户端连接对应的流。gRPC 要求同一条流同一时刻只有一个 Send，
// 因此所有发送都持 sendMu；id 仅用于日志定位。
type clientSession struct {
	id     uint64
	stream message.Grpc_MsgServer
	sendMu sync.Mutex
}

var sessionSeq atomic.Uint64

type RpcServer struct {
	message.UnimplementedGrpcServer
	nametoclientstream *syncmap.Map[string, *clientSession]
	sessionMu          sync.Mutex
	name               string
	service            *grpc.Server
	readchannel        chan *MessageInfo
	handlechannel      chan *MessageInfo
	done               chan struct{}
	stopped            atomic.Bool
	closeOnce          sync.Once
	handlerSem         chan struct{}
	// handler 回调表：业务通过 util.DefaultCallFunc 按「CallRpcMsg:协议号:协议号」注册，
	// 因此所有 RpcServer 实例共享同一张表（协议号全局唯一是既有约定）
	callFunc *util.CallFunction
	// 回调接口（原子指针：SetCallbacks 可能与 recv/Send 并发）
	callbacks atomic.Pointer[ServerCallbacks]
	// 正在执行的 handler 数量。超时只丢弃结果；handler 须自行尊重 ctx，运行时不会强杀 goroutine。
	inFlight atomic.Int64
	// drainDeadline：本轮停机排空截止时间（UnixNano，0 表示未设置）
	drainDeadline atomic.Int64
	// readClose 只停「接收+解析」这一段；关闭后 readChannel 转交完存量才退出。
	// 停机必须分两段：先停上游、等它把存量交给下游，再停 handleChannel。
	// 两段一起停的话，readchannel 里的存量会因为下游先退场而永久失联。
	readClose chan struct{}
	readOnce  sync.Once
	// readGone/handleGone 在两个常驻协程退出时关闭：停机要等「接收段把存量转交完」，
	// 用通道而不是 WaitGroup——Add 与 Wait 并发会撞上 WaitGroup 的使用限制。
	readGone   chan struct{}
	handleGone chan struct{}
}

// closeRead 发出「停止接收」信号，幂等。
// readClose 为 nil 表示这个实例是外部直接构造的（未走 StartGrpcServer），无接收段可停。
func (s *RpcServer) closeRead() {
	if s.readClose == nil {
		return
	}
	s.readOnce.Do(func() { close(s.readClose) })
}

// startLoops 启动接收段与处理段两个常驻协程，并登记各自的退出信号。
func (s *RpcServer) startLoops() {
	go func() {
		defer close(s.readGone)
		s.readChannel()
	}()
	go func() {
		defer close(s.handleGone)
		s.handleChannel()
	}()
}

// waitGone 等待某个常驻协程退出，最多等 budget。
func waitGone(ch chan struct{}, budget time.Duration) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(budget):
	}
}

// handlerResult 单次 handler 执行结果。bret=false 表示失败（未注册、panic、ctx 已结束）。
type handlerResult struct {
	bret bool
	res  []reflect.Value
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		vars.Error("gRPC连接错误,没有元数据")
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	clientName := md.Get("client-name")
	if len(clientName) == 0 {
		vars.Error("gRPC连接错误,没有客户端名称")
		return status.Error(codes.Unauthenticated, "missing client-name")
	}
	if clientName[0] == "" {
		vars.Error("gRPC连接错误,没有客户端名称")
		return status.Error(codes.Unauthenticated, "empty client-name")
	}
	// 客户端名称作为key
	clientNameKey := clientName[0]
	// 存储客户端stream（同名重连会替换，退出时按会话身份 compare-delete）
	cs := s.openSession(clientNameKey, stream)
	defer s.closeSession(clientNameKey, cs)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			vars.Info("gRPC连接关闭,客户端主动断开连接")
			return nil
		}
		if err != nil {
			vars.Error("接收gRPC消息错误: %v", err)
			return err
		}

		// 避免在服务器停止后继续处理消息
		select {
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
		default:
		}

		p1 := msg.GetHead().GetProtocol1()
		p2 := msg.GetHead().GetProtocol2()
		var reqID uint64
		if msg.GetHead() != nil {
			reqID = msg.GetHead().GetRequestId()
		}
		item := &MessageInfo{
			Req:           msg,
			ClientNameKey: clientNameKey,
			Protol1:       p1,
			Protol2:       p2,
			RequestID:     reqID,
		}
		// 收到拒绝就不再入队，否则回调的放行判定形同虚设；
		// 但必须回一个错误包，不然客户端只能空等满自己的调用超时才知道被拒。
		if !s.triggerOnMessageReceived(clientNameKey, p1, p2, msg) {
			vars.Info("RPC服务器[%s]回调拒绝消息,协议号:%d:%d, 客户端:%s", s.name, p1, p2, clientNameKey)
			s.sendErrorPacket(item)
			continue
		}

		select {
		case s.readchannel <- item:
			// 发送成功
		case <-s.readClose:
			vars.Info("RPC服务器[%s]停止接收，退回消息 协议号:%d:%d, 客户端:%s", s.name, p1, p2, clientNameKey)
			s.sendErrorPacket(item)
			// 接收段已经在交还存量，继续 Recv 只会把更多请求送进无人消费的队列
			return nil
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
		}
	}
}

// openSession 登记一条客户端流并触发连接回调。
func (s *RpcServer) openSession(clientNameKey string, stream message.Grpc_MsgServer) *clientSession {
	cs := &clientSession{id: sessionSeq.Add(1), stream: stream}
	s.sessionMu.Lock()
	s.nametoclientstream.Store(clientNameKey, cs)
	s.sessionMu.Unlock()
	s.triggerOnClientConnected(clientNameKey)
	return cs
}

// closeSession 只在登记的仍是本会话时移除并触发断开回调。
// 同名新会话已经接管时不再回调，避免连接/断开事件成对错乱。
func (s *RpcServer) closeSession(clientNameKey string, cs *clientSession) {
	s.sessionMu.Lock()
	own := false
	if cur, ok := s.nametoclientstream.Load(clientNameKey); ok && cur == cs {
		s.nametoclientstream.Delete(clientNameKey)
		own = true
	}
	s.sessionMu.Unlock()
	if own {
		s.triggerOnClientDisconnected(clientNameKey)
	}
}

// 发送消息
func (s *RpcServer) Send(name string, pb1, pb2 int32, pb proto.Message) error {
	return s.SendWithRequestID(name, pb1, pb2, 0, pb)
}

// SendWithRequestID 发送响应并回填 request_id（0 表示旧客户端兼容串行模式）
func (s *RpcServer) SendWithRequestID(name string, pb1, pb2 int32, requestID uint64, pb proto.Message) error {
	if !s.triggerOnSendResponse(name, pb1, pb2, pb) {
		return fmt.Errorf("gRPC Send: 回调阻止了发送 [name=%s]", name)
	}

	rsp := util.NewFSMessageWithID(pb1, pb2, requestID, pb)
	if rsp == nil {
		return fmt.Errorf("gRPC Send: 序列化失败 [name=%s 协议号:%d:%d]", name, pb1, pb2)
	}
	return s.sendToSession(name, rsp)
}

// sendToSession 按客户端会话串行发送一帧（gRPC 流禁止并发 Send）
func (s *RpcServer) sendToSession(name string, rsp *message.FSMessage) error {
	cs, ok := s.nametoclientstream.Load(name)
	if !ok {
		return fmt.Errorf("gRPC Send: 未找到客户端流[name=%s]", name)
	}
	cs.sendMu.Lock()
	defer cs.sendMu.Unlock()
	if err := cs.stream.Send(rsp); err != nil {
		return fmt.Errorf("发送gRPC响应错误: %v", err)
	}
	return nil
}

// sendErrorPacket 告知客户端本次请求失败，避免它空等满调用超时。
// 旧客户端（request_id=0）按“下一条响应即本次响应”匹配，无法区分错误包，故跳过。
func (s *RpcServer) sendErrorPacket(msg *MessageInfo) {
	if msg.RequestID == 0 {
		return
	}
	// Body 必须是空切片而不是 nil：FSMessage 是 proto2，body 标了 required，
	// 缺 presence 的帧在 Send 时直接 marshaling 失败，错误包一个都发不出去。
	rsp := &message.FSMessage{
		Head: &message.Head{
			Protocol1: proto.Int32(msg.Protol1),
			Protocol2: proto.Int32(msg.Protol2),
			RequestId: proto.Uint64(msg.RequestID),
			Cmd:       proto.String(ErrorCmd),
		},
		Body: []byte{},
	}
	if err := s.sendToSession(msg.ClientNameKey, rsp); err != nil {
		vars.Error("RPC服务端回错误包失败[%s] 协议号:%d:%d request_id=%d: %v",
			s.name, msg.Protol1, msg.Protol2, msg.RequestID, err)
	}
}

// 解析数据
func (s *RpcServer) readChannel() {
	for {
		select {
		case <-s.readClose:
			s.drainRead()
			return
		// 异常路径（Serve 失败、直接构造的实例）只关了 done：这里也必须收工，
		// 但仍要把已接收的存量交出去，别让客户端干等超时。
		case <-s.done:
			s.drainRead()
			return
		case msg := <-s.readchannel:
			s.forward(msg)
		}
	}
}

// forward 解析请求并转交下游处理协程。
func (s *RpcServer) forward(msg *MessageInfo) {
	if msg == nil {
		return
	}
	req := util.ParseFSMessage(msg.Req)
	if req == nil {
		// 协议未注册/Body 解码失败：客户端在等这一条的回包，明确拒绝比静默丢弃友好
		vars.Error("RPC服务端解析请求失败[%s] 协议号:%d:%d, 客户端:%s",
			s.name, msg.Protol1, msg.Protol2, msg.ClientNameKey)
		s.sendErrorPacket(msg)
		return
	}
	item := &MessageInfo{
		Req:           req,
		ClientNameKey: msg.ClientNameKey,
		Protol1:       msg.Protol1,
		Protol2:       msg.Protol2,
		RequestID:     msg.RequestID,
	}
	// 不带 done 分支的话，停机时 handlechannel 一满这个 goroutine 就永久卡死
	select {
	case s.handlechannel <- item:
	case <-s.done:
		s.runSerial(item)
	}
}

// drainRead 停机信号到达后把 readchannel 的存量转交下游。
//
// 这些请求已经从客户端流里读走了，客户端在等回包；只清 handlechannel
// 而放过 readchannel，等于把这一段队列里的请求无声吞掉。
func (s *RpcServer) drainRead() {
	for {
		select {
		case msg := <-s.readchannel:
			s.forward(msg)
		default:
			return
		}
	}
}

// runSerial 停机兜底：下游协程已收工时，由当前协程串行把存量跑完。
//
// 只有 Stop 排过截止时间才允许兜底：没有截止时间说明这不是一次有预算的停机
// （Serve 启动失败、异常构造的实例），此时明确回错误包比无预算地跑 handler 安全。
func (s *RpcServer) runSerial(msg *MessageInfo) {
	remain := s.drainRemaining()
	if remain <= 0 {
		s.rejectQueued(msg)
		return
	}
	select {
	case s.handlerSem <- struct{}{}:
	case <-time.After(remain):
		s.rejectQueued(msg)
		return
	}
	s.handleOne(msg)
}

// drainRemaining 距排空截止时间的剩余量：>0 还剩多久，<=0 已超时或本轮无排空预算。
func (s *RpcServer) drainRemaining() time.Duration {
	deadline := s.drainDeadline.Load()
	if deadline <= 0 {
		return 0
	}
	return time.Until(time.Unix(0, deadline))
}

// rejectQueued 排空预算内再也放不下时，明确给对端一个错误包并留痕，
// 而不是让客户端干等到自己的超时。
func (s *RpcServer) rejectQueued(msg *MessageInfo) {
	vars.Error("RPC停机排空溢出，丢弃请求[%s] 协议号:%d:%d, 客户端:%s",
		s.name, msg.Protol1, msg.Protol2, msg.ClientNameKey)
	s.sendErrorPacket(msg)
}

// 操作数据：每条消息交给独立 goroutine 处理，串行 await 会让一个慢 handler
// 拖住整条流水线；令牌仍由 handler goroutine 归还，保证并发上限是真实在跑的 handler 数。
//
// 退出只认 s.done：停机由 Stop 按「先停接收、再排空处理」两段驱动，
// 跟着 app.ctx 一起退会让还在转交的存量找不到下游。
func (s *RpcServer) handleChannel() {
	for {
		select {
		case <-s.done:
			s.drainBacklog()
			return
		case msg := <-s.handlechannel:
			s.startHandle(msg)
		}
	}
}

// startHandle 占用一个并发令牌后异步处理一条消息。
func (s *RpcServer) startHandle(msg *MessageInfo) {
	select {
	case s.handlerSem <- struct{}{}:
		go s.handleOne(msg)
	case <-s.done:
		s.runSerial(msg)
	}
}

// drainBacklog 停机信号到达后消化 handlechannel 里的存量请求。
//
// 这些请求已经收进服务端队列（对端在等回包），退出时直接丢弃等于让它们全部等到超时；
// 存量以串行方式处理，避免停机瞬间又拉起一批并发 handler。
func (s *RpcServer) drainBacklog() {
	for {
		select {
		case msg := <-s.handlechannel:
			s.runSerial(msg)
		default:
			return
		}
	}
}

// handleOne 处理单条消息：等待 handler 结果或超时，然后回包。
//
// 调用方已占用一个 handlerSem 令牌，令牌由这里的 handler 协程归还。
// 「取到令牌 → 协程真正跑起来」之间任何一步崩掉都必须把令牌还回去，
// 否则并发令牌只减不增，服务会在若干次异常后彻底停止处理请求。
func (s *RpcServer) handleOne(msg *MessageInfo) {
	// handlerStarted 之前令牌仍归本函数负责。
	// 归还必须第一个登记：下面的 context 构造、metrics、反射取值任何一步崩掉，
	// 都发生在 handler 协程起来之前，晚登记一步就是一次永久令牌泄漏。
	handlerStarted := false
	defer func() {
		if !handlerStarted {
			<-s.handlerSem
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			vars.Error("处理gRPC请求panic,协议号:%d:%d, 客户端:%s: %v",
				msg.Protol1, msg.Protol2, msg.ClientNameKey, r)
		}
	}()

	// handler 挂在工作上下文上，而不是 app.ctx：见 workParent 的说明，
	// 否则「先 cancel 再 Stop」的关闭顺序会在第一毫秒就掐死全部在途请求。
	parent := workParent()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, handlerTimeout())
	defer cancel()

	method := fmt.Sprintf("%d:%d", msg.Protol1, msg.Protol2)
	metrics.RPC.IncRequests(s.name, method)
	started := time.Now()

	resultCh := make(chan handlerResult, 1)
	s.inFlight.Add(1)
	handlerStarted = true
	go func() {
		defer func() { <-s.handlerSem }()
		defer s.inFlight.Add(-1)
		defer func() {
			if r := recover(); r != nil {
				vars.Error("RPC handler发生panic[%s] 协议号:%d:%d, 客户端:%s: %v",
					s.name, msg.Protol1, msg.Protol2, msg.ClientNameKey, r)
				select {
				case resultCh <- handlerResult{bret: false}:
				default:
				}
			}
		}()
		// 排队期间已超时就不再启动 handler，直接交给等待侧收敛
		select {
		case <-ctx.Done():
			return
		default:
		}
		key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.Protol1, msg.Protol2)
		results, ok := s.callFunc.DoWithRetCtx(ctx, key, msg)
		if ok && len(results) > 0 {
			resultCh <- handlerResult{bret: true, res: results}
			return
		}
		resultCh <- handlerResult{bret: false}
	}()

	select {
	case <-ctx.Done():
		metrics.RPC.IncErrors(s.name, method, "timeout")
		metrics.RPC.ObserveLatency(s.name, method, time.Since(started))
		vars.Error("处理gRPC请求超时,协议号:%d:%d, 客户端:%s, in-flight=%d（回调须接受 context.Context）",
			msg.Protol1, msg.Protol2, msg.ClientNameKey, s.inFlight.Load())
		s.sendErrorPacket(msg)
		s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, nil, false)

	case result := <-resultCh:
		metrics.RPC.ObserveLatency(s.name, method, time.Since(started))
		rsp, kind := classifyResult(result)
		switch kind {
		case resultOK:
			if err := s.SendWithRequestID(msg.ClientNameKey, msg.Protol1, msg.Protol2, msg.RequestID, rsp); err != nil {
				vars.Error("RPC服务端回包失败[%s] 协议号:%d:%d, 客户端:%s: %v",
					s.name, msg.Protol1, msg.Protol2, msg.ClientNameKey, err)
				s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, rsp, false)
				return
			}
			s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, rsp, true)

		case resultEmpty:
			metrics.RPC.IncErrors(s.name, method, "empty")
			vars.Error("处理gRPC请求错误,没有返回值,协议号:%d:%d, 客户端:%s",
				msg.Protol1, msg.Protol2, msg.ClientNameKey)
			s.sendErrorPacket(msg)
			s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, nil, false)

		case resultBadType:
			metrics.RPC.IncErrors(s.name, method, "bad_type")
			vars.Error("处理gRPC请求返回类型错误,协议号:%d:%d, 客户端:%s, 实际类型:%s",
				msg.Protol1, msg.Protol2, msg.ClientNameKey, result.res[0].Type())
			s.sendErrorPacket(msg)
			s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, nil, false)

		default:
			metrics.RPC.IncErrors(s.name, method, "handler")
			vars.Error("处理gRPC请求错误,协议号:%d:%d, 客户端:%s",
				msg.Protol1, msg.Protol2, msg.ClientNameKey)
			s.sendErrorPacket(msg)
			s.triggerOnMessageProcessed(msg.ClientNameKey, msg.Protol1, msg.Protol2, nil, false)
		}
	}
}

type resultKind int

const (
	resultOK resultKind = iota
	resultEmpty
	resultBadType
	resultFailed
)

// classifyResult 判定 handler 返回值：断言必须带 ok，否则类型不符会直接 panic 掉服务 goroutine。
func classifyResult(result handlerResult) (proto.Message, resultKind) {
	if !result.bret || len(result.res) == 0 {
		return nil, resultFailed
	}
	v, ok := result.res[0].Interface().(proto.Message)
	if !ok {
		return nil, resultBadType
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return nil, resultEmpty
	}
	return v, resultOK
}

// closeDone 发出「处理段也停止」的信号，幂等。
// 一并关闭 readClose：Serve 失败等异常路径只调本函数，不能留下永不自家的接收协程。
func (s *RpcServer) closeDone() {
	s.closeOnce.Do(func() {
		s.closeRead()
		close(s.done)
	})
}

func (s *RpcServer) Stop(ctx context.Context) {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	grace := time.Duration(gracefulStopBudget_.Load())
	drain := time.Duration(drainBudget_.Load())
	if ctx == nil {
		ctx = context.Background()
	}
	// App 的关闭顺序是「先 cancel app.ctx，再逐个 Stop」，传进来的 ctx 到这里必然已经 Done。
	// 若把它当成「调用方等不及」，GracefulStop 会在第一毫秒被换成 Stop() 强杀：
	// 在途 handler 的回包全部丢失，后面的两段排空也只剩空转。
	// 调用方要真正提前中止，应该用 SetShutdownBudget 收紧预算。
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	if s.service != nil {
		done := make(chan struct{})
		go func() {
			s.service.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			s.service.Stop()
		case <-time.After(grace):
			s.service.Stop()
		}
	}

	// 排空截止时间从这里才立起来：GracefulStop 可能已经吃掉整个 grace，
	// 提前立的话这里剩余量恒为负，每条存量都会被「预算已尽」直接拒掉。
	s.drainDeadline.Store(time.Now().Add(drain).UnixNano())

	// 两段式停机：先停接收段并等它把 readchannel 的存量转交出去，
	// 再停处理段。同时关闭两个信号的话，存量会因为下游已退场而永久失联。
	// 每段只吃上一段的剩余量，整轮排空合计不超过 drain。
	s.closeRead()
	waitGone(s.readGone, s.drainRemaining())
	s.closeDone()

	// 排空：等处理协程消化完队列存量
	waitGone(s.handleGone, s.drainRemaining())
	// 两条常驻协程收工后，仍滞留在队列里的请求不会再有人碰：
	// 接收段退出与 drainRead 之间存在竞态（select 随机选中入队分支），
	// 这里兜底清干净，让它们拿到错误包而不是干等到客户端超时。
	s.drainLeftover()
	// 已经交给独立协程的 handler 可能仍在跑（含超时后仍不返回的），再等一轮收敛
	deadline := time.Now().Add(s.drainRemaining())
	for s.inFlight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.inFlight.Load(); n > 0 {
		vars.Warning("RPC服务器[%s] 关闭时仍有 %d 个 in-flight handler", s.name, n)
	}

	s.triggerOnServerStopped()

	vars.Info("RPC服务器停止[%s]", s.name)
}

// drainLeftover 清掉两条常驻协程退出后仍留在队列里的请求，逐个回错误包。
func (s *RpcServer) drainLeftover() {
	for {
		select {
		case msg := <-s.readchannel:
			s.rejectQueued(msg)
		case msg := <-s.handlechannel:
			s.rejectQueued(msg)
		default:
			return
		}
	}
}

func StartGrpcServer(name string, port int, useTLS bool) error {
	addr := "[::]:" + strconv.Itoa(port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("gRPC监听失败[%s]: %v", addr, err)
		return err
	}
	closeLis := func() {
		if cerr := lis.Close(); cerr != nil {
			vars.Error("关闭gRPC监听失败[%s]: %v", addr, cerr)
		}
	}

	// 检查 TLS 配置
	var serverOptions []grpc.ServerOption

	if useTLS {
		rpc := activeRpcCfg()
		if rpc != nil && rpc.TLS != nil {
			cert, err := tls.LoadX509KeyPair(rpc.TLS.CertFile, rpc.TLS.KeyFile)
			if err != nil {
				vars.Error("gRPC加载TLS证书失败[%s]: %v", addr, err)
				closeLis()
				return err
			}
			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
			if authMode() == "mtls" {
				tlsConfig, err = mtlsServerTLS(tlsConfig)
				if err != nil {
					vars.Error("gRPC mTLS 配置失败[%s]: %v", addr, err)
					closeLis()
					return err
				}
			}
			serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
		}
	}

	warnInsecureRPC(name, useTLS)

	serverOptions = append(serverOptions,
		grpc.ChainUnaryInterceptor(authUnaryInterceptor),
		grpc.ChainStreamInterceptor(authStreamInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// MaxConnectionIdle 和 MaxConnectionAge 设为 0 表示无限制，永不主动断开
			MaxConnectionIdle:     0,                // 不因空闲断开
			MaxConnectionAge:      0,                // 不因存活时间断开
			MaxConnectionAgeGrace: 30 * time.Second, // 优雅关闭宽限期
			Time:                  2 * time.Hour,    // 服务端 ping 间隔（基本不主动 ping）
			Timeout:               20 * time.Second, // ping 超时
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // 允许客户端最小 5 秒 ping 一次（宽松策略）
			PermitWithoutStream: true,            // 允许无 stream 时 ping
		}),
		grpc.MaxRecvMsgSize(MAX_MSG_SIZE),
		grpc.MaxSendMsgSize(MAX_MSG_SIZE),
	)

	vars.Info("gRPC监听已启动[%s]，服务器名称:%s, TLS: %v", addr, name, useTLS)

	s := grpc.NewServer(serverOptions...)
	service := &RpcServer{
		name:               name,
		service:            s,
		readchannel:        make(chan *MessageInfo, channelSize),
		handlechannel:      make(chan *MessageInfo, channelSize),
		done:               make(chan struct{}),
		readClose:          make(chan struct{}),
		readGone:           make(chan struct{}),
		handleGone:         make(chan struct{}),
		callFunc:           util.DefaultCallFunc,
		nametoclientstream: syncmap.NewMap[string, *clientSession](),
		handlerSem:         make(chan struct{}, defaultHandlerConcurrency),
	}
	service.callbacks.Store(NewServerCallbacks())

	message.RegisterGrpcServer(service.service, service)

	go func(s *RpcServer) {
		//启动监听
		if err := s.service.Serve(lis); err != nil {
			vars.Error("gRPC服务启动失败[%s]: %v", s.name, err)
			service_.Delete(s.name)
			s.closeDone()
			return
		}
	}(service)

	service.startLoops()

	service_.Store(name, service)
	vars.Info("gRPC服务启动成功,端口:%d", port)

	// 触发服务启动回调
	service.triggerOnServerStarted()
	return nil
}

// ==================== 回调触发方法（内部使用）====================

// callCallback 执行业务回调并隔离 panic：回调跑在服务端 goroutine 里，
// 一次 panic 会带走整个进程。
func callCallback(desc string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("RPC回调panic[%s]: %v", desc, r)
		}
	}()
	fn()
}

// callGate 执行“是否放行”型回调；回调 panic 按拒绝处理（宁可丢一条消息，也不放行未知数据）。
func callGate(desc string, fn func() bool) bool {
	pass := false
	defer func() {
		if r := recover(); r != nil {
			vars.Error("RPC回调panic[%s]: %v", desc, r)
			pass = false
		}
	}()
	pass = fn()
	return pass
}

// triggerOnServerStarted 触发服务启动回调
func (s *RpcServer) triggerOnServerStarted() {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnServerStarted != nil {
		callCallback(fmt.Sprintf("server[%s].OnServerStarted", s.name), func() {
			cbs.OnServerStarted(s.name)
		})
	}
}

// triggerOnServerStopped 触发服务停止回调
func (s *RpcServer) triggerOnServerStopped() {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnServerStopped != nil {
		callCallback(fmt.Sprintf("server[%s].OnServerStopped", s.name), func() {
			cbs.OnServerStopped(s.name)
		})
	}
}

// triggerOnClientConnected 触发客户端连接回调
func (s *RpcServer) triggerOnClientConnected(clientName string) {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnClientConnected != nil {
		callCallback(fmt.Sprintf("server[%s].OnClientConnected", s.name), func() {
			cbs.OnClientConnected(s.name, clientName)
		})
	}
}

// triggerOnClientDisconnected 触发客户端断开回调
func (s *RpcServer) triggerOnClientDisconnected(clientName string) {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnClientDisconnected != nil {
		callCallback(fmt.Sprintf("server[%s].OnClientDisconnected", s.name), func() {
			cbs.OnClientDisconnected(s.name, clientName)
		})
	}
}

// triggerOnMessageReceived 触发消息接收回调（返回是否继续处理）
func (s *RpcServer) triggerOnMessageReceived(clientName string, protocol1, protocol2 int32, msg proto.Message) bool {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnMessageReceived != nil {
		return callGate(fmt.Sprintf("server[%s].OnMessageReceived", s.name), func() bool {
			return cbs.OnMessageReceived(s.name, clientName, protocol1, protocol2, msg)
		})
	}
	return true // 默认继续处理
}

// triggerOnMessageProcessed 触发消息处理完成回调
func (s *RpcServer) triggerOnMessageProcessed(clientName string, protocol1, protocol2 int32, result proto.Message, success bool) {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnMessageProcessed != nil {
		callCallback(fmt.Sprintf("server[%s].OnMessageProcessed", s.name), func() {
			cbs.OnMessageProcessed(s.name, clientName, protocol1, protocol2, result, success)
		})
	}
}

// triggerOnSendResponse 触发发送响应前回调（返回是否继续发送）
func (s *RpcServer) triggerOnSendResponse(clientName string, protocol1, protocol2 int32, resp proto.Message) bool {
	cbs := s.callbacks.Load()
	if cbs != nil && cbs.OnSendResponse != nil {
		return callGate(fmt.Sprintf("server[%s].OnSendResponse", s.name), func() bool {
			return cbs.OnSendResponse(s.name, clientName, protocol1, protocol2, resp)
		})
	}
	return true // 默认继续发送
}

// ==================== 公共方法：回调接口管理 ====================

// SetCallbacks 设置服务端回调接口
func (s *RpcServer) SetCallbacks(callbacks *ServerCallbacks) {
	if callbacks == nil {
		callbacks = NewServerCallbacks()
	}
	s.callbacks.Store(callbacks)
}

// GetCallbacks 获取服务端回调接口
func (s *RpcServer) GetCallbacks() *ServerCallbacks {
	return s.callbacks.Load()
}
