package rpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"touchgocore/corectx"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

const (
	MAX_MSG_SIZE       = 1024 * 1024 * 10
	defaultChannelSize = 4096
	MAX_CHANNEL_SIZE   = defaultChannelSize
)

var channelSize = defaultChannelSize

// rpcRunCtx 本轮 Run 的生命周期上下文。原子指针：server/auth 协程每轮 select 都要
// 读它的 Done()，用普通变量会与下一轮 Run 的赋值构成数据竞争。
var rpcRunCtx atomic.Pointer[context.Context]

// runCtx 取当前生命周期上下文；未 Run 过时返回 Background。
func runCtx() context.Context {
	if p := rpcRunCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// setRunCtx 安装生命周期上下文（Run 与测试装配用）。
func setRunCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	rpcRunCtx.Store(&ctx)
}

// rpcWorkCtx 是 handler 的父上下文，只由本轮 Stop 在排空预算用尽后取消。
//
// 它刻意不继承 app.ctx：App 的关闭顺序是先 cancel 再逐个 Stop，若 handler 也挂在
// app.ctx 上，正在处理的请求会在 Shutdown 的第一毫秒全部收到 ctx.Done，
// GracefulStop 与 in-flight 排空就都成了空转（收到的请求必然回错误包）。
var (
	workMu        sync.Mutex
	rpcWorkCtx    context.Context = context.Background()
	rpcWorkCancel context.CancelFunc
)

// workParent 返回当前这一轮 handler 的父上下文。
func workParent() context.Context {
	workMu.Lock()
	defer workMu.Unlock()
	return rpcWorkCtx
}

// resetWorkCtx 为新一轮 Run 建立独立的 handler 上下文，并作废上一轮的残留。
func resetWorkCtx() {
	workMu.Lock()
	defer workMu.Unlock()
	if rpcWorkCancel != nil {
		rpcWorkCancel()
	}
	rpcWorkCtx, rpcWorkCancel = context.WithCancel(context.Background())
}

// cancelWorkCtx 在排空结束后取消 handler 上下文，防止卡死的 handler 永久持有资源。
// 取消后立刻把父上下文复位：StartGrpcServer 是公开 API，允许不经 Run 再起一轮，
// 留着已取消的那个会让之后每个 handler 一进来就 ctx.Done。
func cancelWorkCtx() {
	workMu.Lock()
	defer workMu.Unlock()
	if rpcWorkCancel != nil {
		rpcWorkCancel()
		rpcWorkCancel = nil
	}
	rpcWorkCtx = context.Background()
}

func Run(ctx context.Context) error {
	setRunCtx(ctx)
	resetWorkCtx()
	root := corectx.CfgFrom(ctx)
	if root == nil {
		vars.Info("RPC配置为空，跳过RPC服务启动")
		return nil
	}
	channelSize = root.QueueCapacity(defaultChannelSize)
	rpcCfg := root.Rpc
	if rpcCfg == nil {
		vars.Info("RPC配置为空，跳过RPC服务启动")
		return nil
	}
	if rpcClient_ == nil {
		rpcClient_ = syncmap.NewMap[string, *RpcClient]()
	}
	if service_ == nil {
		service_ = syncmap.NewMap[string, *RpcServer]()
	}
	cfg := rpcCfg
	serverCount := len(cfg.Server)
	clientCount := len(cfg.Client)
	vars.Info("开始启动RPC服务: 服务器%d个, 客户端%d个", serverCount, clientCount)

	// 初始化服务发现（默认使用静态配置）
	InitDiscovery(NewStaticDiscovery(cfg.Server, cfg.Client))

	var lastErr error
	started := 0
	for _, v := range cfg.Server {
		if v.Name == "" || v.Port <= 0 {
			vars.Error("RPC服务器配置无效: Name=%s, Addr=%s, Port=%d", v.Name, v.Addr, v.Port)
			continue
		}
		useTLS := resolveTLSConfig(v.UseTLS)
		if err := StartGrpcServer(v.Name, v.Port, useTLS); err != nil {
			lastErr = err
			continue
		}
		started++
	}

	// 启动客户端连接（通过服务发现解析端点）
	clientSuccess := 0
	for _, v := range cfg.Client {
		if v.Name == "" || v.Addr == "" || v.Port <= 0 {
			vars.Error("RPC客户端配置无效: Name=%s, Addr=%s, Port=%d", v.Name, v.Addr, v.Port)
			continue
		}
		if client := NewRpcClient(v.Name, v.Addr, v.Port); client != nil {
			clientSuccess++
		}
	}
	vars.Info("RPC服务启动完成: 服务器%d个, 客户端%d个 (成功连接%d个)", serverCount, clientCount, clientSuccess)
	if serverCount > 0 && started == 0 && lastErr != nil {
		return lastErr
	}
	return nil
}

// resolveTLSConfig 统一解析TLS配置
func resolveTLSConfig(defaultTLS bool) bool {
	rpc := activeRpcCfg()
	if rpc != nil && rpc.TLS != nil && rpc.TLS.Enable {
		if rpc.TLS.SkipForIntranet {
			vars.Warning("rpc.tls.skip_for_intranet=true：内网将按各端 use_tls 决定是否明文，生产环境请关闭")
			return defaultTLS
		}
		return true
	}
	return defaultTLS
}

// ResolveService 通过服务发现解析服务端点（供业务层使用）
func ResolveService(ctx context.Context, serviceName string) ([]*ServiceEndpoint, error) {
	dm := GetDiscovery()
	if dm == nil {
		return nil, fmt.Errorf("service discovery not initialized")
	}
	return dm.Resolve(ctx, serviceName)
}

func Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	// 关闭服务发现
	if dm := GetDiscovery(); dm != nil {
		dm.Close()
	}

	// 停止所有RPC服务器：以注册表为准，配置缺失或改动也要能关掉已起的server
	serverCount := 0
	if service_ != nil {
		service_.Range(func(key string, v1 *RpcServer) bool {
			v1.Stop(ctx)
			serverCount++
			return true
		})
		service_.Clear()
	}

	// 关闭所有RPC客户端连接
	clientCount := 0
	if rpcClient_ != nil {
		rpcClient_.Range(func(_ string, v1 *RpcClient) bool {
			if err := v1.Close(); err != nil {
				vars.Error("RPC客户端关闭失败[%s]: %v", v1.fullAddr, err)
			}
			clientCount++
			return true
		})
		rpcClient_.Clear()
	}
	vars.Info("RPC服务停止: 服务器%d个, 客户端%d个", serverCount, clientCount)
	// 排空窗口已过：还卡着的 handler 到此为止，随生命周期一起结束
	cancelWorkCtx()
}

// UseRegistry 将 RPC 服务端/客户端表绑定到调用方提供的 map（App 优先，全局 fallback）。
func UseRegistry(servers *syncmap.Map[string, *RpcServer], clients *syncmap.Map[string, *RpcClient]) {
	if servers != nil {
		service_ = servers
	}
	if clients != nil {
		rpcClient_ = clients
	}
}

// ==================== 全局访问方法（供外部使用）====================

// GetRpcClient 根据名称获取 RPC 客户端实例
func GetRpcClient(name string) *RpcClient {
	if rpcClient_ == nil {
		return nil
	}
	client, ok := rpcClient_.Load(name)
	if !ok {
		return nil
	}
	return client
}

// GetRpcServer 根据名称获取 RPC 服务端实例
func GetRpcServer(name string) *RpcServer {
	if service_ == nil {
		return nil
	}
	srv, ok := service_.Load(name)
	if !ok {
		return nil
	}
	return srv
}

// SetClientCallbacks 为指定客户端设置回调接口
func SetClientCallbacks(clientName string, callbacks *ClientCallbacks) bool {
	client := GetRpcClient(clientName)
	if client == nil {
		return false
	}
	client.SetCallbacks(callbacks)
	return true
}

// SetServerCallbacks 为指定服务端设置回调接口
func SetServerCallbacks(serverName string, callbacks *ServerCallbacks) bool {
	server := GetRpcServer(serverName)
	if server == nil {
		return false
	}
	server.SetCallbacks(callbacks)
	return true
}

// GetAllRpcClients 获取所有 RPC 客户端实例
func GetAllRpcClients() map[string]*RpcClient {
	result := make(map[string]*RpcClient)
	if rpcClient_ == nil {
		return result
	}
	rpcClient_.Range(func(key string, value *RpcClient) bool {
		result[key] = value
		return true
	})
	return result
}

// GetAllRpcServers 获取所有 RPC 服务端实例
func GetAllRpcServers() map[string]*RpcServer {
	result := make(map[string]*RpcServer)
	if service_ == nil {
		return result
	}
	service_.Range(func(key string, value *RpcServer) bool {
		result[key] = value
		return true
	})
	return result
}
