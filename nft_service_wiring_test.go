package touchgocore

// NFT 通道在装配层的跨包接线闸（T9 合流批）。
//
// 为什么必须在根包而不是 nft 包：nft/ 的用例只编译 nft 包自身，services.go 的适配器
// 与 app.go 的服务列表根本不参与编译——把 services.go 里那句 nft.NftStart 删掉、
// 或把 app.go 列表里的 &nftService{} 漏掉，包级套件照绿（变异检验时实测同类：
// 装配类缺陷在包内副本里杀不掉）。本文件钉的就是这几条接线本身：服务列表登记、
// Start/Stop 转调、未配置不阻断整机、回调前缀与下游字面量契约。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/nft"
	"touchgocore/util"
)

// nftWireCfg 造一份「配好就能起链路」的 nft 段配置，指向调用方给的假供应商。
func nftWireCfg(baseURL string) (*config.Cfg, *config.NftSDKConfig) {
	sdk := &config.NftSDKConfig{
		Enable: "on", Driver: nft.DriverGeneric, BaseURL: baseURL,
		AppID: "wiring-app", SecretKey: "wiring-secret",
		Endpoints: map[string]string{"mint": "/mint", "query": "/query"},
		Accounts: map[string]*config.NftMerchantAccount{
			"main": {Enable: "on", MerchantID: "m-wire"},
		},
	}
	return &config.Cfg{
		Nft: &config.NftConfig{
			Provider: &config.NftSDKRef{SDK: "mynft", Account: "main"},
			Chain:    nft.ChainETH, Network: nft.NetworkMainnet,
		},
		NftSDks: map[string]*config.NftSDKConfig{"mynft": sdk},
	}, sdk
}

// TestNftServiceRegisteredInStartupList 服务列表必须真的登记了 nft 通道，且早于 gin：
// 网关先于资产通道起来时，第一批对外请求拿到的是「NFT 通道未启动」而不是结果。
func TestNftServiceRegisteredInStartupList(t *testing.T) {
	app := &App{}
	app.registerServices()

	order := map[string]int{}
	for i, s := range app.services {
		order[s.Name()] = i
	}
	if _, ok := order["nft"]; !ok {
		t.Fatal("服务列表漏登记 nft 通道：NFT 门面永远拿不到客户端")
	}
	if ginIdx, ok := order["gin"]; ok && order["nft"] > ginIdx {
		t.Fatalf("✘ nft 必须早于 gin 启动: nft=%d gin=%d", order["nft"], ginIdx)
	}
}

// TestNftServiceAdapterDelegatesToChannel 适配器的 Start/Stop 必须转调 nft 包，
// 而不是只「返回 nil 看着像成功了」。
func TestNftServiceAdapterDelegatesToChannel(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	cfg, _ := nftWireCfg(srv.URL)
	ctx := corectx.WithCfg(context.Background(), cfg)
	t.Cleanup(func() { nft.NftStop(nil) })

	s := &nftService{}
	if s.Name() != "nft" {
		t.Fatalf("服务名必须是 nft（配置段与运维口径认这个名字），实得 %q", s.Name())
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("已配好的 nft 段启动不应报错: %v", err)
	}
	_, mintErr := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "wiring-mint-1", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	})
	if mintErr != nil && strings.Contains(mintErr.Error(), "未启动") {
		t.Fatalf("Start 没有真的把通道装配起来（nft.NftStart 是死调用？）: %v", mintErr)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatalf("前置不成立：装配后一次外呼都没发生，这条闸就是空跑（mintErr=%v）", mintErr)
	}

	// Stop 必须摘掉客户端：可重复调用，且停后门面逐字拒绝。
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop 不应报错: %v", err)
	}
	if _, err := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "wiring-mint-2", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	}); err == nil || !strings.Contains(err.Error(), "未启动") {
		t.Fatalf("Stop 后门面必须报未启动，实得 err=%v", err)
	}
}

// TestNftServiceStartWithoutConfigDoesNotBlockApp nft 段没配时 Start 必须返回 nil：
// 一个可选通道配错，不该让整机起不来（口径同 bscService）。
func TestNftServiceStartWithoutConfigDoesNotBlockApp(t *testing.T) {
	t.Cleanup(func() { nft.NftStop(nil) })

	if err := (&nftService{}).Start(corectx.WithCfg(context.Background(), &config.Cfg{})); err != nil {
		t.Fatalf("未配置 nft 段必须不阻断整机，实得: %v", err)
	}
	if _, err := nft.NftMint(context.Background(), &nft.NftOrder{
		OrderNo: "wiring-mint-3", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://wiring",
	}); err == nil || !strings.Contains(err.Error(), "未启动") {
		t.Fatalf("未配置就不该有可用链路，实得 err=%v", err)
	}
}

// TestNftCallbackPrefixContract 回调前缀是与下游（游戏逻辑/Lua 侧订阅方）的书面契约，
// 下游按字面量 "NftMsg"+动作 注册，本包常量一改它们就静默收不到回执。
// 自指引用（util.CallNftMsg）探不到这种改动，故这里按字面量钉，并证明广播真走这个键。
func TestNftCallbackPrefixContract(t *testing.T) {
	if util.CallNftMsg != "NftMsg" || util.CallNftSDKMsg != "NftSDK" {
		t.Fatalf("回调前缀契约漂移: CallNftMsg=%q CallNftSDKMsg=%q", util.CallNftMsg, util.CallNftSDKMsg)
	}

	var hits, broadcasts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","data":{"order_no":"prefix-mint-1","status":"success","token_id":"1001","tx_hash":"0xtx"}}`))
	}))
	defer srv.Close()
	cfg, _ := nftWireCfg(srv.URL)
	ctx := corectx.WithCfg(context.Background(), cfg)
	t.Cleanup(func() { nft.NftStop(nil) })

	// 键名逐字写死，不用 util.CallNftMsg 拼——否则常量漂移时两侧一起变，探不到。
	id := util.DefaultCallFunc.Register("NftMsgMint", func(*nft.NftResult) {
		atomic.AddInt64(&broadcasts, 1)
	})
	defer util.DefaultCallFunc.Unregister("NftMsgMint", id)

	if err := (&nftService{}).Start(ctx); err != nil {
		t.Fatalf("启动不应报错: %v", err)
	}
	res, err := nft.NftMint(ctx, &nft.NftOrder{
		OrderNo: "prefix-mint-1", ToAddress: "0xabc", Quantity: 1, MetadataURI: "ipfs://prefix",
	})
	if err != nil {
		t.Fatalf("夹具应拿到成功回执，实得 err=%v res=%+v", err, res)
	}
	if atomic.LoadInt64(&hits) == 0 {
		t.Fatal("前置不成立：一次外呼都没发生")
	}
	if atomic.LoadInt64(&broadcasts) != 1 {
		t.Fatalf("下游按契约键名注册后必须收到 1 次回执，实得 %d", broadcasts)
	}
}
