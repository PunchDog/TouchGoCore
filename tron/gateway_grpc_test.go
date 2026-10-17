package tron

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 广播响应码的分类是防双付的第一道闸：判错一类，要么把钱再动一次，要么让一单永远卡死。

func TestBroadcastOutcomeClassification(t *testing.T) {
	cases := []struct {
		name          string
		result        bool
		code          api.ReturnResponseCode
		wantAccepted  bool
		wantRejected  bool
		wantUncertain bool
	}{
		{"受理", true, api.Return_SUCCESS, true, false, false},
		{"签名错", false, api.Return_SIGERROR, false, true, false},
		{"TAPOS 过期块", false, api.Return_TAPOS_ERROR, false, true, false},
		{"交易已过期", false, api.Return_TRANSACTION_EXPIRATION_ERROR, false, true, false},
		{"交易过大", false, api.Return_TOO_BIG_TRANSACTION_ERROR, false, true, false},
		{"合约校验不过", false, api.Return_CONTRACT_VALIDATE_ERROR, false, true, false},
		{"带宽不足", false, api.Return_BANDWITH_ERROR, false, true, false},
		{"重复交易", false, api.Return_DUP_TRANSACTION_ERROR, false, false, true},
		{"服务繁忙", false, api.Return_SERVER_BUSY, false, false, true},
		{"节点无连接", false, api.Return_NO_CONNECTION, false, false, true},
		{"区块未固化", false, api.Return_BLOCK_UNSOLIDIFIED, false, false, true},
		{"合约执行异常", false, api.Return_CONTRACT_EXE_ERROR, false, false, true},
		{"其它错误", false, api.Return_OTHER_ERROR, false, false, true},
		{"自相矛盾（result 假而码为成功）", false, api.Return_SUCCESS, false, false, true},
	}
	for _, cs := range cases {
		got := broadcastOutcome(&api.Return{Result: cs.result, Code: cs.code})
		if got.Accepted != cs.wantAccepted || got.Rejected != cs.wantRejected || got.Uncertain != cs.wantUncertain {
			t.Errorf("%s: 分成 accepted=%v rejected=%v uncertain=%v，期望 %v/%v/%v",
				cs.name, got.Accepted, got.Rejected, got.Uncertain, cs.wantAccepted, cs.wantRejected, cs.wantUncertain)
		}
		if got.Code == "" {
			t.Errorf("%s: 响应码没留痕，对账时无从判断", cs.name)
		}
	}
	// 空响应（节点回了 nil）不能当成受理。
	if got := broadcastOutcome(nil); got.Accepted || !got.Uncertain {
		t.Errorf("空响应被读成 %+v", got)
	}
}

func TestIsDefinitelyUnsent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"参数非法（请求没被处理）", status.Error(codes.InvalidArgument, "bad"), true},
		{"前置条件不满足", status.Error(codes.FailedPrecondition, "no"), true},
		{"资源超限", status.Error(codes.ResourceExhausted, "rate"), true},
		{"超时（可能已入池）", status.Error(codes.DeadlineExceeded, "slow"), false},
		{"服务不可用（可能已入池）", status.Error(codes.Unavailable, "down"), false},
		{"连接被重置", errors.New("read: connection reset by peer"), false},
		{"裸错误一律按可能已发出", errors.New("boom"), false},
	}
	for _, cs := range cases {
		if got := isDefinitelyUnsent(cs.err); got != cs.want {
			t.Errorf("%s: 判成 %v，期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestCrossCheckTxID 节点回的交易号与本地按 sha256(raw_data) 算出的必须逐字一致：
// 广播结果不确定时，手里那个号必须与链上那个号是同一个；节点没回号时跳过
// （那是节点的缺失，不是分歧）。
func TestCrossCheckTxID(t *testing.T) {
	tx := buildableTx()
	want, err := txIDOf(tx)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := hex.DecodeString(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := crossCheckTxID(nodeID, tx); err != nil {
		t.Fatalf("两份实现算出的交易号一致，却报了对不上: %v", err)
	}
	bad := append([]byte{}, nodeID...)
	bad[0] ^= 0xFF
	if err := crossCheckTxID(bad, tx); err == nil {
		t.Fatal("交易号不一致必须就地拒绝")
	}
	if err := crossCheckTxID(nil, tx); err != nil {
		t.Fatalf("节点没回交易号时应当跳过而不是报错: %v", err)
	}
}

func TestNewGRPCGatewayRequiresNode(t *testing.T) {
	if _, err := newGRPCGateway("", fakeAPIKey, time.Second, true); err == nil {
		t.Fatal("没配节点时必须拒绝建网关")
	}
	// 超时留空要落到一个正的缺省值：SDK 把它直接交给 context.WithTimeout，
	// 传 0 就是「一出生就过期」，表现是每个链上调用都超时。
	gw, err := newGRPCGateway("127.0.0.1:50051", fakeAPIKey, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if gw.g == nil {
		t.Fatal("网关没持有客户端")
	}
	// 传输安全必须显式声明：grpc-go 对「没给凭据」的拨号直接拒绝建连。
	if _, err := newGRPCGateway("grpc.trongrid.io:50051", fakeAPIKey, time.Second, false); err != nil {
		t.Fatalf("缺省（TLS）应当能建连，实得 %v", err)
	}
}

func TestGRPCGatewayStopsFailingClosed(t *testing.T) {
	gw, err := newGRPCGateway("127.0.0.1:50051", "", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.close(); err != nil {
		t.Fatal(err)
	}
	// 关停之后每个方法都要明确失败，而不是 panic 或返回空成功。
	if _, err := gw.buildTransfer(context.Background(), tronRangeAcct, tronAllZeroAcct, tronOfficialUSDT, 1, 1); err == nil {
		t.Error("关停后仍能构造交易")
	}
	if _, err := gw.broadcast(context.Background(), &core.Transaction{}); err == nil {
		t.Error("关停后仍能广播")
	}
	if _, err := gw.lookupTx(context.Background(), "x"); err == nil {
		t.Error("关停后仍能查证")
	}
	if _, err := gw.tokenBalance(context.Background(), tronRangeAcct, tronOfficialUSDT); err == nil {
		t.Error("关停后仍能读余额")
	}
	if _, err := gw.account(context.Background(), tronRangeAcct); err == nil {
		t.Error("关停后仍能读账户")
	}
	if _, err := gw.headBlock(context.Background()); err == nil {
		t.Error("关停后仍能读链头")
	}
}

func TestGRPCGatewayHasNoTransferListing(t *testing.T) {
	// 全节点 gRPC 没有「按地址列转账历史」这一接口，反查只能靠接入点读侧。
	// 这条能力边界必须显式报错，不能让调用方以为「查过了，没有」。
	gw, err := newGRPCGateway("127.0.0.1:50051", "", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.outboundTransfers(context.Background(), tronRangeAcct, tronOfficialUSDT, 1); !errors.Is(err, errNoTransferListing) {
		t.Fatalf("应当回能力边界错误，实得 %v", err)
	}
}
