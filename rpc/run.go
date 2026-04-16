package rpc

import (
	"context"
	"fmt"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/vars"

	"google.golang.org/grpc"
)

const (
	MAX_MSG_SIZE     = 1024 * 1024 * 10
	MAX_CHANNEL_SIZE = 100000 // 减少通道容量以降低内存占用
)

func Run() {
	if config.Cfg_.RpcPort == nil {
		vars.Info("RPC配置为空，跳过RPC服务启动")
		return
	}
	rpcClient_ = syncmap.NewMap[string, *RpcClient]()
	service_ = syncmap.NewMap[string, *RpcServer]()
	cfg := config.Cfg_.RpcPort
	serverCount := len(cfg.Server)
	clientCount := len(cfg.Client)
	vars.Info("开始启动RPC服务: 服务器%d个, 客户端%d个", serverCount, clientCount)

	// 初始化服务发现（默认使用静态配置）
	InitDiscovery(NewStaticDiscovery(cfg.Server, cfg.Client))

	// 启动服务器监听（通过服务发现解析端点）
	for _, v := range cfg.Server {
		if v.Name == "" || v.Addr == "" || v.Port <= 0 {
			vars.Error("RPC服务器配置无效: Name=%s, Addr=%s, Port=%d", v.Name, v.Addr, v.Port)
			continue
		}
		useTLS := resolveTLSConfig(v.UseTLS)
		StartGrpcServer(v.Name, v.Addr, v.Port, useTLS)
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
}

// resolveTLSConfig 统一解析TLS配置
func resolveTLSConfig(defaultTLS bool) bool {
	if config.Cfg_.Rpc != nil && config.Cfg_.Rpc.TLS != nil && config.Cfg_.Rpc.TLS.Enable {
		if config.Cfg_.Rpc.TLS.SkipForIntranet {
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

func Stop() {
	// 关闭服务发现
	if dm := GetDiscovery(); dm != nil {
		dm.Close()
	}

	// 停止所有RPC服务器
	serverCount := 0
	service_.Range(func(key string, v1 *RpcServer) bool {
		v1.Stop()
		serverCount++
		return true
	})

	// 关闭所有RPC客户端连接
	clientCount := 0
	rpcClient_.Range(func(key string, v1 *RpcClient) bool {
		if connVal := v1.conn.Load(); connVal != nil {
			if conn, ok := connVal.(*grpc.ClientConn); ok && conn != nil {
				conn.Close()
				clientCount++
			}
		}
		return true
	})
	vars.Info("RPC服务停止: 服务器%d个, 客户端%d个", serverCount, clientCount)
}
