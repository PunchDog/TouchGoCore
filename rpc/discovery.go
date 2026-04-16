package rpc

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/vars"
)

// ==================== 服务发现抽象层 ====================

// ServiceEndpoint 代表一个服务端点
type ServiceEndpoint struct {
	Name    string // 服务名称
	Addr    string // IP地址
	Port    int    // 端口
	UseTLS  bool   // 是否使用TLS
	Meta    map[string]string // 元数据（可用于权重、区域等）
}

// ServiceDiscovery 服务发现接口
// 抽象服务发现机制，支持静态配置、DNS、etcd等实现
type ServiceDiscovery interface {
	// Resolve 解析服务名到端点列表
	Resolve(ctx context.Context, serviceName string) ([]*ServiceEndpoint, error)
	// Watch 监听服务变化（可选实现，返回nil表示不支持）
	Watch(ctx context.Context, serviceName string) (<-chan []*ServiceEndpoint, error)
	// Close 关闭服务发现
	Close() error
}

// ==================== 静态配置解析器 ====================

// StaticDiscovery 基于配置文件的静态服务发现
// 适用于服务地址固定的场景（当前默认实现）
type StaticDiscovery struct {
	endpoints map[string][]*ServiceEndpoint
	mu        sync.RWMutex
}

// NewStaticDiscovery 从RpcAddr配置创建静态服务发现
func NewStaticDiscovery(servers, clients []*config.RpcAddr) *StaticDiscovery {
	sd := &StaticDiscovery{
		endpoints: make(map[string][]*ServiceEndpoint),
	}

	for _, s := range servers {
		ep := &ServiceEndpoint{
			Name:   s.Name,
			Addr:   s.Addr,
			Port:   s.Port,
			UseTLS: s.UseTLS,
		}
		sd.endpoints[s.Name] = append(sd.endpoints[s.Name], ep)
	}

	return sd
}

// Resolve 解析服务名到端点列表
func (sd *StaticDiscovery) Resolve(_ context.Context, serviceName string) ([]*ServiceEndpoint, error) {
	sd.mu.RLock()
	defer sd.mu.RUnlock()

	endpoints, ok := sd.endpoints[serviceName]
	if !ok {
		return nil, fmt.Errorf("service '%s' not found in static config", serviceName)
	}

	// 返回副本以避免外部修改
	result := make([]*ServiceEndpoint, len(endpoints))
	copy(result, endpoints)
	return result, nil
}

// Watch 静态配置不支持Watch，返回nil
func (sd *StaticDiscovery) Watch(_ context.Context, _ string) (<-chan []*ServiceEndpoint, error) {
	return nil, nil
}

// Close 关闭静态服务发现
func (sd *StaticDiscovery) Close() error {
	return nil
}

// ==================== DNS解析器 ====================

// DNSDiscovery 基于DNS的服务发现
// 适用于使用DNS做服务发现的场景（如Kubernetes Headless Service）
type DNSDiscovery struct {
	defaultPort int
	defaultTLS  bool
}

// NewDNSDiscovery 创建DNS服务发现
func NewDNSDiscovery(defaultPort int, defaultTLS bool) *DNSDiscovery {
	return &DNSDiscovery{
		defaultPort: defaultPort,
		defaultTLS:  defaultTLS,
	}
}

// Resolve 通过DNS解析服务地址
func (dd *DNSDiscovery) Resolve(ctx context.Context, serviceName string) ([]*ServiceEndpoint, error) {
	// 使用net.LookupHost进行DNS解析
	host := serviceName
	port := dd.defaultPort

	// 如果 serviceName 格式为 host:port，则解析
	if h, p, err := net.SplitHostPort(serviceName); err == nil {
		host = h
		if p != "" {
			if parsedPort, err := strconv.Atoi(p); err == nil {
				port = parsedPort
			}
		}
	}

	// 添加DNS解析超时
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var addrs []string
	var err error

	done := make(chan struct{})
	go func() {
		addrs, err = net.LookupHost(host)
		close(done)
	}()

	select {
	case <-resolveCtx.Done():
		return nil, fmt.Errorf("DNS resolution timeout for %s", host)
	case <-done:
		if err != nil {
			return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
		}
	}

	endpoints := make([]*ServiceEndpoint, 0, len(addrs))
	for _, addr := range addrs {
		endpoints = append(endpoints, &ServiceEndpoint{
			Name:   serviceName,
			Addr:   addr,
			Port:   port,
			UseTLS: dd.defaultTLS,
		})
	}

	return endpoints, nil
}

// Watch DNS不支持Watch，返回nil
func (dd *DNSDiscovery) Watch(_ context.Context, _ string) (<-chan []*ServiceEndpoint, error) {
	return nil, nil
}

// Close 关闭DNS服务发现
func (dd *DNSDiscovery) Close() error {
	return nil
}

// ==================== 服务发现管理器 ====================

// DiscoveryManager 服务发现管理器
// 管理服务发现的实例，支持运行时切换发现策略
type DiscoveryManager struct {
	discovery ServiceDiscovery
	mu        sync.RWMutex
}

var globalDiscoveryManager *DiscoveryManager

// InitDiscovery 初始化全局服务发现管理器
func InitDiscovery(sd ServiceDiscovery) {
	globalDiscoveryManager = &DiscoveryManager{
		discovery: sd,
	}
	vars.Info("服务发现管理器初始化完成")
}

// GetDiscovery 获取全局服务发现管理器
func GetDiscovery() *DiscoveryManager {
	return globalDiscoveryManager
}

// Resolve 解析服务端点
func (dm *DiscoveryManager) Resolve(ctx context.Context, serviceName string) ([]*ServiceEndpoint, error) {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	if dm.discovery == nil {
		return nil, fmt.Errorf("service discovery not initialized")
	}

	return dm.discovery.Resolve(ctx, serviceName)
}

// SetDiscovery 切换服务发现实现（支持运行时热切换）
func (dm *DiscoveryManager) SetDiscovery(sd ServiceDiscovery) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if dm.discovery != nil {
		dm.discovery.Close()
	}
	dm.discovery = sd
	vars.Info("服务发现实现已切换")
}

// Close 关闭服务发现管理器
func (dm *DiscoveryManager) Close() error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if dm.discovery != nil {
		return dm.discovery.Close()
	}
	return nil
}

// ==================== 预留：etcd 服务发现 ====================

// EtcdDiscoveryConfig etcd服务发现配置
// 当项目需要etcd支持时，取消注释并实现
type EtcdDiscoveryConfig struct {
	Endpoints   []string      // etcd集群地址
	Prefix      string        // 服务注册前缀，如 "/services/"
	DialTimeout time.Duration // 连接超时
	TTL         time.Duration // 租约TTL
}

// EtcdDiscovery 基于etcd的服务发现（预留）
// 实现步骤:
// 1. go get go.etcd.io/etcd/client/v3
// 2. 实现 ServiceDiscovery 接口
// 3. 使用 InitDiscovery(NewEtcdDiscovery(cfg)) 切换
//
// type EtcdDiscovery struct {
//     client *clientv3.Client
//     prefix string
// }
//
// func NewEtcdDiscovery(cfg EtcdDiscoveryConfig) (*EtcdDiscovery, error) { ... }
// func (ed *EtcdDiscovery) Resolve(ctx context.Context, serviceName string) ([]*ServiceEndpoint, error) { ... }
// func (ed *EtcdDiscovery) Watch(ctx context.Context, serviceName string) (<-chan []*ServiceEndpoint, error) { ... }
// func (ed *EtcdDiscovery) Close() error { ... }
