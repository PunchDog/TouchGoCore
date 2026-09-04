package rpc

import (
	"context"
	"testing"

	"touchgocore/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func withRPCAuth(t *testing.T, auth *config.RpcAuthConfig) {
	t.Helper()
	prev := config.Cfg_
	config.Cfg_ = &config.Cfg{Rpc: &config.RpcConfig{Auth: auth}}
	t.Cleanup(func() {
		config.Cfg_ = prev
	})
}

func TestAuthenticateNoneMissingClientName(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "none"})
	err := authenticate(context.Background())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestAuthenticateNoneOK(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "none"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if err := authenticate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateAllowlist(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "allowlist", AllowList: []string{"gate"}})
	okCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if err := authenticate(okCtx); err != nil {
		t.Fatal(err)
	}
	badCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "other"))
	if status.Code(authenticate(badCtx)) != codes.Unauthenticated {
		t.Fatal("expected allowlist reject")
	}
}

func TestAuthenticateToken(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "token", Token: "secret"})
	okMD := metadata.Pairs("client-name", "gate", "authorization", "Bearer secret")
	if err := authenticate(metadata.NewIncomingContext(context.Background(), okMD)); err != nil {
		t.Fatal(err)
	}
	badMD := metadata.Pairs("client-name", "gate", "authorization", "Bearer wrong")
	if status.Code(authenticate(metadata.NewIncomingContext(context.Background(), badMD))) != codes.Unauthenticated {
		t.Fatal("expected token reject")
	}
}

func TestAuthenticateMTLSMissingPeer(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "mtls", CAFile: "ca.pem"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if status.Code(authenticate(ctx)) != codes.Unauthenticated {
		t.Fatal("expected mTLS reject without peer cert")
	}
}
