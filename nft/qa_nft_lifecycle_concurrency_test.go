package nft

// T7 包级行为闸第三件：
// 闸 6 —— NftStop 之后（区别于既有「从未启动」用例：这里先真实启动并证明广播/外呼发生过，
//        再停）六门面逐字拒绝 SPEC §8 文案、httptest 零新增命中、回调广播零新增触发；
// 闸 7 —— 契约反耦合的源码级闸（go/parser 读非测试文件的 import 面，
//        闭包反证另有 `go list -deps ./nft`，此处把「不得 import 六包+paysdk、
//        types/channel/provider/client 四核心文件零本仓 import、config 只准出现在 wire.go」
//        钉成活闸）；
// 闸 8 —— 并发共享状态补强：同指纹并发重发（orderLedger）、token 读写（atomic）、
//        注册表同名竞争（registryMu）、门面与 NftStop 竞态（active）。
//        -race 本机不可用（无 C 工具链），以本组用例 + -count=3 确定性绿替代，race 归 Lead 合流批。

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"touchgocore/util"
)

// qaNotStarted 逐字钉 SPEC §8 的未启动错误全文（与 nft_test.go 的常量独立定义，
// 保证本文件被单独复制进沙箱副本变异跑时仍自足）。
const qaNotStarted = "NFT 通道未启动（检查 nft.provider 是否引用了已开启的 nft_sdks 段与签发账户）"

// ---- 闸 6：停机的就地拒绝必须是「无副作用」的拒绝 ----

func TestQaStopAfterStartRejectsInPlaceNoRequestNoBroadcast(t *testing.T) {
	var hits int64
	srv := qaCountingServer(t, &hits, "5001")
	p := qaProvider(t, srv.URL, "k", -1)

	var broadcasts int64
	id := util.DefaultCallFunc.Register(util.CallNftMsg+"Mint", func(*NftResult) {
		atomic.AddInt64(&broadcasts, 1)
	})
	defer util.DefaultCallFunc.Unregister(util.CallNftMsg+"Mint", id)

	active.Store(&client{ch: p, chain: ChainETH, network: NetworkMainnet})
	t.Cleanup(func() { active.Store(nil) })
	ctx := context.Background()

	// 先证明「启动态」下门面无拒绝、外呼与广播各发生一次——否则停机后的零命中是空跑。
	if _, err := NftMint(ctx, qaMintOrder(t.Name()+"-live")); err != nil {
		t.Fatalf("启动态 NftMint 不应拒绝: %v", err)
	}
	if atomic.LoadInt64(&hits) != 1 || atomic.LoadInt64(&broadcasts) != 1 {
		t.Fatalf("启动态应各发生 1 次外呼与广播，实得 hits=%d bc=%d",
			atomic.LoadInt64(&hits), atomic.LoadInt64(&broadcasts))
	}

	// 停机（可重复调用，§8）。
	NftStop(nil)
	NftStop(nil)

	hitsBase := atomic.LoadInt64(&hits)
	bcBase := atomic.LoadInt64(&broadcasts)

	checks := []struct {
		name string
		err  error
	}{
		{"NftMint", func() error { _, err := NftMint(ctx, qaMintOrder(t.Name()+"-s1")); return err }()},
		{"NftTransfer", func() error { _, err := NftTransfer(ctx, qaTransferOrder(t.Name()+"-s2")); return err }()},
		{"NftQuery", func() error { _, err := NftQuery(ctx, t.Name()+"-s3"); return err }()},
		{"NftHoldings", func() error { _, err := NftHoldings(ctx, &HoldingsQuery{Address: "0xQa"}); return err }()},
		{"NftToken", func() error {
			_, err := NftToken(ctx, &TokenQuery{Chain: ChainETH, Contract: "0xC", TokenID: "1"})
			return err
		}()},
		{"NftReconcile", func() error { _, err := NftReconcile(ctx, t.Name()+"-s4"); return err }()},
	}
	for _, c := range checks {
		if c.err == nil {
			t.Fatalf("%s 停机后应就地拒绝", c.name)
		}
		if c.err.Error() != qaNotStarted {
			t.Fatalf("%s 未启动错误应逐字匹配 SPEC §8，实得 %q", c.name, c.err.Error())
		}
		// 拒绝不经 orderCall 的「NFT %s 失败: %w」二次包装（§8 精确口径）。
		if strings.Contains(c.err.Error(), "失败:") {
			t.Fatalf("%s 不应被 orderCall 包装: %q", c.name, c.err.Error())
		}
	}
	if got := atomic.LoadInt64(&hits); got != hitsBase {
		t.Fatalf("停机拒绝不得外呼，httptest 计数应冻结在 %d，实得 %d", hitsBase, got)
	}
	if got := atomic.LoadInt64(&broadcasts); got != bcBase {
		t.Fatalf("停机拒绝不得广播，计数应冻结在 %d，实得 %d", bcBase, got)
	}
}

// ---- 闸 7：契约反耦合（源码级）----

func TestQaSourceImportsDecoupled(t *testing.T) {
	banned := []string{
		"touchgocore/pay", "touchgocore/usdt", "touchgocore/bsc",
		"touchgocore/sol", "touchgocore/tron", "touchgocore/telegram", "touchgocore/paysdk",
	}
	// §1 文件划分表：核心四文件必须零本仓 import（纯 stdlib）。
	coreStdlibOnly := map[string]bool{"types.go": true, "channel.go": true, "provider.go": true, "client.go": true}

	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	parsed := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed++
		file, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", f, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if path == b || strings.HasPrefix(path, b+"/") {
					t.Errorf("%s import 了禁耦合包 %q（硬约束 1）", filepath.Base(f), path)
				}
			}
			if strings.HasPrefix(path, "touchgocore/") {
				if coreStdlibOnly[filepath.Base(f)] {
					t.Errorf("%s 属核心契约文件，不得 import 本仓包 %q（§1 表：纯 stdlib）", filepath.Base(f), path)
				}
				if path == "touchgocore/config" && filepath.Base(f) != "wire.go" {
					t.Errorf("%s 不得 import config（ADR-1：装配文件独占）", filepath.Base(f))
				}
			}
		}
		// 分层红线（硬约束 4）：核心文件不得直接引 SQL 驱动。
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "database/sql" || strings.HasPrefix(path, "github.com/go-sql-driver") {
				t.Errorf("%s 直接 import SQL 驱动 %q，破分层红线", filepath.Base(f), path)
			}
		}
	}
	if want := len(coreStdlibOnly) + 3; parsed < want {
		t.Fatalf("非测试文件应至少 %d 个（§1 表 7 文件），实得 %d——测试目录不对？", want, parsed)
	}
	if _, err := os.Stat("wire.go"); err != nil {
		t.Fatalf("wire.go 缺失: %v", err)
	}
}

// ---- 闸 8：并发共享状态补强（与既有 TestConcurrentSharedState 互补）----

func TestQaConcurrentSameFingerprintResubmits(t *testing.T) {
	var hits int64
	srv := qaCountingServer(t, &hits, "9001")
	p := qaProvider(t, srv.URL, "k", -1)
	orderNo := t.Name() + "-same"

	const N = 32
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 同 OrderNo 同指纹并发重发：闸应全部放行（透明重发），不得有半个拒绝。
			if _, err := p.Mint(context.Background(), qaMintOrder(orderNo)); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("同指纹并发重发应全部放行: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != N {
		t.Fatalf("同指纹并发重发应各外呼一次（共 %d），实得 %d", N, got)
	}
	// 事后异指纹提交仍被拒（并发放行没有把闸打漏）。
	changed := qaMintOrder(orderNo)
	changed.Quantity = 7
	if _, err := p.Mint(context.Background(), changed); err == nil {
		t.Fatal("并发放行后闸仍须拒绝异指纹变更")
	}
}

func TestQaConcurrentTokenAndRegistry(t *testing.T) {
	srv := qaCountingServer(t, new(int64), "1")
	p := qaProvider(t, srv.URL, "k", -1)

	// token 是 per-provider atomic.Pointer：并发写读不得读到撕裂值。
	const N = 32
	writes := make([]string, N)
	for i := range writes {
		writes[i] = "qa-tok-" + string(rune('a'+i%26)) + string(rune('A'+i/26))
	}
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.SetToken(writes[i])
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := p.Token()
			if got == "" {
				return
			}
			for _, w := range writes {
				if got == w {
					return
				}
			}
			t.Errorf("Token() 读到非写入值（撕裂/脏值）: %q", got)
		}()
	}
	// 同名注册竞争：恰好一个成功，其余全是「已登记，不得覆盖」。
	probe := "qa_race_probe"
	qaDelRegistry(probe)
	t.Cleanup(func() { qaDelRegistry(probe) })
	var okCount, dupCount int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := Register(probe, func(_ ProviderOptions) (NftChannel, error) { return &stubChannel{}, nil })
			switch {
			case err == nil:
				atomic.AddInt64(&okCount, 1)
			case strings.Contains(err.Error(), "已登记，不得覆盖"):
				atomic.AddInt64(&dupCount, 1)
			default:
				t.Errorf("Register 意外错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok := atomic.LoadInt64(&okCount); ok != 1 {
		t.Fatalf("同名并发注册应恰好 1 个成功，实得 %d", ok)
	}
	if dup := atomic.LoadInt64(&dupCount); dup != N-1 {
		t.Fatalf("其余 %d 个应命中拒覆盖，实得 %d", N-1, dup)
	}

	// 白名单锁面在读写混战下不 fatal（与既有用例互补：这里与门面无谓竞争）。
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			SetRetryableCodes("qa-conc-code")
			if !IsRetryableCode("qa-conc-code") {
				t.Error("登记后立读应为成员")
			}
		}(i)
	}
	wg.Wait()
	DeleteRetryableCode("qa-conc-code")
}

func TestQaConcurrentFacadesVersusStop(t *testing.T) {
	// 门面调用与 NftStop/再装载竞态：只允许两种结局——成功返回，或逐字未启动错误。
	// 不得 panic、不得半途包装错误、不得静默吞拒。
	stub := &stubChannel{tag: "qa-stub"}
	ctx := context.Background()
	const N = 16
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	wg.Add(1)
	go func() { // 反复起停。
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stopCh:
				return
			default:
			}
			if i%2 == 0 {
				active.Store(&client{ch: stub, chain: ChainETH, network: NetworkMainnet})
			} else {
				active.Store(nil)
			}
		}
	}()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := NftQuery(ctx, "qa-race-query")
			if err != nil && err.Error() != qaNotStarted {
				t.Errorf("门面竞态出现第三种错误: %q", err.Error())
			}
		}(i)
	}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := NftHoldings(ctx, &HoldingsQuery{Address: "0xQa"})
			if err != nil && err.Error() != qaNotStarted {
				t.Errorf("持有门面竞态出现第三种错误: %q", err.Error())
			}
		}(i)
	}
	close(stopCh)
	wg.Wait()
	active.Store(nil)
}
