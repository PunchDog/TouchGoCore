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
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func authMode() string {
	cfg := rpcAuthCfg()
	if cfg == nil {
		return "none"
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		return "none"
	}
	return mode
}

func rpcAuthCfg() *config.RpcAuthConfig {
	if config.Cfg_ == nil {
		return nil
	}
	rpc := config.Cfg_.Rpc
	if rpc == nil {
		rpc = config.Cfg_.RpcPort
	}
	if rpc == nil {
		return nil
	}
	return rpc.Auth
}

func authenticate(ctx context.Context) error {
	mode := authMode()
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
		cfg := rpcAuthCfg()
		for _, name := range cfg.AllowList {
			if name == clientName {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "client-name not in allowlist")
	case "token":
		token := bearerToken(md)
		expected := rpcAuthCfg().Token
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
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
	if strings.HasPrefix(raw, prefix) {
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
	if config.Cfg_ != nil {
		rpc := config.Cfg_.Rpc
		if rpc == nil {
			rpc = config.Cfg_.RpcPort
		}
		if rpc != nil && rpc.TLS != nil && rpc.TLS.Enable && rpc.TLS.SkipForIntranet {
			vars.Warning("gRPC[%s] skip_for_intranet=true，内网明文可被伪造身份；生产请设为 false", name)
		}
	}
	if authMode() == "none" {
		vars.Warning("gRPC[%s] auth.mode=none，仅校验 client-name 是否存在，可被伪造", name)
	}
}
