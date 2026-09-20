package rpc

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// defaultAuthMode 未配置 rpc.auth 或未填 mode 时按 token 处理（fail-closed）。
// 需要匿名互通必须显式写 mode=none——开放 RPC 端口只校验 client-name 存在与否，等于无鉴权。
const defaultAuthMode = "token"

func authMode() string {
	return authModeOf(rpcAuthCfg())
}

// authModeOf 对一份配置快照求鉴权模式。
// 调用方必须先取快照再判模式：分两次读 rpcAuthCfg() 时，两次之间配置换掉
// 就会拿到「mode=allowlist 但 cfg 为 nil」的组合，遍历 cfg.AllowList 直接空指针。
func authModeOf(cfg *config.RpcAuthConfig) string {
	if cfg == nil {
		return defaultAuthMode
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		return defaultAuthMode
	}
	return mode
}

func activeCfg() *config.Cfg {
	return corectx.CfgFrom(runCtx())
}

func activeRpcCfg() *config.RpcConfig {
	cfg := activeCfg()
	if cfg == nil {
		return nil
	}
	return cfg.RpcOf()
}

func rpcAuthCfg() *config.RpcAuthConfig {
	rpc := activeRpcCfg()
	if rpc == nil {
		return nil
	}
	return rpc.Auth
}

func authenticate(ctx context.Context) error {
	// 单次快照：mode 与名单/口令必须来自同一份配置，否则鉴权判定会用到撕裂状态
	cfg := rpcAuthCfg()
	mode := authModeOf(cfg)
	md, _ := metadata.FromIncomingContext(ctx)
	clientName := firstMD(md, "client-name")

	switch mode {
	case "none":
		if clientName == "" {
			return status.Error(codes.Unauthenticated, "missing client-name")
		}
		return nil
	case "allowlist":
		if clientName == "" {
			return status.Error(codes.Unauthenticated, "missing client-name")
		}
		if cfg == nil || len(cfg.AllowList) == 0 {
			return status.Error(codes.Unauthenticated, "rpc.auth.allowlist is not configured")
		}
		for _, name := range cfg.AllowList {
			if name == clientName {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "client-name not in allowlist")
	case "token":
		if cfg == nil || strings.TrimSpace(cfg.Token) == "" {
			// 默认即 token：漏配 token 时宁可拒绝所有连接，也不能放行空口令
			return status.Error(codes.Unauthenticated, "rpc.auth.token is not configured")
		}
		token := bearerToken(md)
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cfg.Token)) != 1 {
			return status.Error(codes.Unauthenticated, "invalid token")
		}
		if clientName == "" {
			return status.Error(codes.Unauthenticated, "missing client-name")
		}
		return nil
	case "mtls":
		pr, ok := peer.FromContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing peer certificate")
		}
		tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
		if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
			return status.Error(codes.Unauthenticated, "mTLS client certificate required")
		}
		if clientName == "" {
			return status.Error(codes.Unauthenticated, "missing client-name")
		}
		return nil
	default:
		return status.Errorf(codes.Unauthenticated, "unsupported auth mode %s", mode)
	}
}

func firstMD(md metadata.MD, key string) string {
	if md == nil {
		return ""
	}
	vs := md.Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

func bearerToken(md metadata.MD) string {
	raw := firstMD(md, "authorization")
	if raw == "" {
		return ""
	}
	const prefix = "Bearer "
	// RFC 7235 的 scheme 大小写不敏感：只认 "Bearer " 会把 "bearer xxx" 的合法客户端拒掉
	if len(raw) >= len(prefix) && strings.EqualFold(raw[:len(prefix)], prefix) {
		return strings.TrimSpace(raw[len(prefix):])
	}
	return raw
}

func authStreamInterceptor(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := authenticate(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func authUnaryInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if err := authenticate(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func clientAuthMetadata(serverName string) metadata.MD {
	pairs := []string{"client-name", serverName}
	if cfg := rpcAuthCfg(); cfg != nil && strings.TrimSpace(cfg.Token) != "" {
		pairs = append(pairs, "authorization", "Bearer "+cfg.Token)
	}
	return metadata.Pairs(pairs...)
}

func mtlsServerTLS(base *tls.Config) (*tls.Config, error) {
	cfg := rpcAuthCfg()
	if cfg == nil || strings.TrimSpace(cfg.CAFile) == "" {
		return nil, fmt.Errorf("mTLS requires rpc.auth.ca_file")
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("invalid mTLS CA pem")
	}
	cloned := base.Clone()
	cloned.ClientCAs = pool
	cloned.ClientAuth = tls.RequireAndVerifyClientCert
	return cloned, nil
}

func mtlsClientTLS() (*tls.Config, error) {
	cfg := rpcAuthCfg()
	if cfg == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.ClientCertFile != "" && cfg.ClientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load mTLS client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read mTLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			tlsCfg.RootCAs = pool
		}
	}
	return tlsCfg, nil
}

func warnInsecureRPC(name string, useTLS bool) {
	if !useTLS {
		vars.Warning("gRPC[%s] 未启用 TLS；生产环境应开启 tls.enable，且不要使用 skip_for_intranet", name)
	}
	if rpc := activeRpcCfg(); rpc != nil && rpc.TLS != nil && rpc.TLS.Enable && rpc.TLS.SkipForIntranet {
		vars.Warning("gRPC[%s] skip_for_intranet=true，内网明文可被伪造身份；生产请设为 false", name)
	}
	cfg := rpcAuthCfg()
	if cfg == nil || strings.TrimSpace(cfg.Mode) == "" {
		vars.Error("gRPC[%s] 未配置 rpc.auth.mode，已按 %s 处理；需要匿名互通请显式设置 mode=none", name, defaultAuthMode)
	}
	switch authModeOf(cfg) {
	case "none":
		vars.Error("gRPC[%s] auth.mode=none，仅校验 client-name 是否存在——能连上端口的进程即可冒充任意服务名，生产环境必须改用 allowlist/token/mtls", name)
	case "token":
		if cfg == nil || strings.TrimSpace(cfg.Token) == "" {
			vars.Error("gRPC[%s] auth.mode=token 但未配置 rpc.auth.token，所有客户端连接都会被拒绝", name)
		}
	case "allowlist":
		if cfg == nil || len(cfg.AllowList) == 0 {
			vars.Error("gRPC[%s] auth.mode=allowlist 但 rpc.auth.allowlist 为空，所有客户端连接都会被拒绝", name)
		}
	}
}
