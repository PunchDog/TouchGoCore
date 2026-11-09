package config

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// supplier 是一份可直接用于判定的内联供应商字段：一段单账户、一段多账户、一段被关闭。
func supplierSection() *ChannelSupplierConfig {
	return &ChannelSupplierConfig{
		Enable: "on", Driver: "tron_grid", BaseURL: "https://one.example",
		Endpoints: map[string]string{"query": "pay/query", "account": " pay/account "},
		Accounts: map[string]*ChannelAccountConfig{
			"default": {Enable: "on", MerchantID: "MCH-1"},
		},
	}
}

// TestSupplierEndpoint 端点表取值：缺前导斜杠要补上、两端空白要去掉、未登记要报 ok=false。
//
// 未登记必须报 false 而不是给个默认路径：配置不全的操作要在发请求之前就拒掉，
// 拼一个猜出来的路径上去只会让供应商回一个语义不明的 404。
func TestSupplierEndpoint(t *testing.T) {
	s := supplierSection()
	if got, has := s.Endpoint("query"); !has || got != "/pay/query" {
		t.Errorf("Endpoint(query)=%q,%v，期望 /pay/query,true", got, has)
	}
	if got, has := s.Endpoint("account"); !has || got != "/pay/account" {
		t.Errorf("Endpoint(account)=%q,%v，期望 /pay/account,true（两端空白要 trim 后再补斜杠）", got, has)
	}
	if _, has := s.Endpoint("withdraw"); has {
		t.Error("未登记的端点不得报成功")
	}
	if _, has := (*ChannelSupplierConfig)(nil).Endpoint("query"); has {
		t.Error("nil 接收者不得报成功")
	}
}

// TestSupplierAccount 商户账户解析：唯一账户可留空别名，多账户必须点名，
// 停用或缺商户号一律不放行——出账主体说不清就是不能出钱。
func TestSupplierAccount(t *testing.T) {
	s := supplierSection()
	acc, err := s.Account("")
	if err != nil {
		t.Fatalf("唯一账户留空应当可解析: %v", err)
	}
	if acc.MerchantID != "MCH-1" {
		t.Fatalf("解析出的商户号不符: %+v", acc)
	}
	// 点名与留空在单账户表上等价。
	if acc, err := s.Account("default"); err != nil || acc.MerchantID != "MCH-1" {
		t.Fatalf("点名唯一账户应当可解析: %+v / %v", acc, err)
	}

	multi := supplierSection()
	multi.Accounts["reserve"] = &ChannelAccountConfig{Enable: "off", MerchantID: "MCH-2"}
	multi.Accounts["leaky"] = &ChannelAccountConfig{Enable: "on"}

	cases := []struct {
		name  string
		sec   *ChannelSupplierConfig
		alias string
		want  string
	}{
		{"nil 段", nil, "", "accounts"},
		{"没有账户表", &ChannelSupplierConfig{Enable: "on"}, "", "accounts"},
		{"多账户未点名", multi, "", "点名"},
		{"点名了停用账户", multi, "reserve", "未显式开启"},
		{"点名了缺商户号的账户", multi, "leaky", "merchant_id"},
		{"账户别名不存在", multi, "ghost", "ghost"},
	}
	for _, cs := range cases {
		_, err := cs.sec.Account(cs.alias)
		if err == nil {
			t.Errorf("%s 应当报错", cs.name)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s 报错文案缺 %q，实得 %v", cs.name, cs.want, err)
		}
	}
	// 点名之后多账户表同样可解析：歧义靠点名消除，不是靠只允许一个账户。
	if acc, err := multi.Account("default"); err != nil || acc.MerchantID != "MCH-1" {
		t.Fatalf("多账户点名后应当可解析: %+v / %v", acc, err)
	}
}

// TestSupplierAccountNeverLeaksSecret 解析报错会进启动日志，密钥不得顺带出去。
func TestSupplierAccountNeverLeaksSecret(t *testing.T) {
	s := supplierSection()
	s.SecretKey = "TOPSECRET"
	if _, err := s.Account("ghost"); err == nil {
		t.Fatal("应当报错")
	} else if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
}

// TestValidateInlineChannels 配置期就能确定的错（缺驱动、缺基址、缺商户号、
// 缺接入点、缺出款地址）必须阻止启动，而不是等第一笔出款暴露。
//
// 口径对四个通道段一致：判定范围只有「已经开启的部分」，enable 不为 on 的段整体跳过——
// 关掉一条通道不该同时要求它的每一项配置都仍然完整。
func TestValidateInlineChannels(t *testing.T) {
	base := func() *Cfg {
		return &Cfg{
			Tron: &TronConfig{Usdt: &TronUsdtConfig{
				ChannelSupplierConfig: *supplierSection(),
				Network:               "mainnet",
				Contract:              "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
				Decimals:              6,
				Chain: &TronChainConfig{
					Node:          "grpc.trongrid.io:50051",
					WalletAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
				},
			}},
			Bsc: &BscConfig{Usdt: &BscUsdtConfig{
				Enable: "on", Driver: "bsc_evm", Network: "mainnet",
				Contract: "0x55d398326f99059fF775485246999027B3197955", Decimals: 18,
				Chain: &BscChainConfig{
					RPC: "https://bsc-dataseed.binance.org", ChainID: 56,
					WalletAddress: "0x1111111111111111111111111111111111111111",
				},
			}},
			Sol: &SolConfig{Usdt: &SolUsdtConfig{
				Enable: "on", Driver: "sol_spl", Network: "mainnet-beta",
				Contract: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6,
				Chain: &SolChainConfig{
					RPC:           "https://api.mainnet-beta.solana.com",
					WalletAddress: "11111111111111111111111111111111",
				},
			}},
			Whatsapp: &WhatsappConfig{},
			Telegram: &TelegramConfig{BotToken: "123456:ABC"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	// 纯链上 + 按单出款参数：没有供应商、合约、热钱包也必须能开启。
	perOrder := base()
	perOrder.Tron.Usdt.BaseURL = ""
	perOrder.Tron.Usdt.AppID = ""
	perOrder.Tron.Usdt.SecretKey = ""
	perOrder.Tron.Usdt.Accounts = nil
	perOrder.Tron.Usdt.Contract = ""
	perOrder.Tron.Usdt.Chain.WalletAddress = ""
	perOrder.Tron.Usdt.Chain.PrivateKey = ""
	if err := perOrder.Validate(); err != nil {
		t.Fatalf("按单携带出款参数时，缺供应商/合约/热钱包应当放行: %v", err)
	}

	cases := map[string]func(*Cfg){
		"段缺驱动标记":         func(c *Cfg) { c.Tron.Usdt.Driver = "" },
		"段缺基址":           func(c *Cfg) { c.Tron.Usdt.BaseURL = " " },
		"账户缺商户号":         func(c *Cfg) { c.Tron.Usdt.Accounts["default"].MerchantID = "" },
		"出款引用没有账户":       func(c *Cfg) { c.Tron.Usdt.Accounts = nil },
		"出款引用了停用的账户":     func(c *Cfg) { c.Tron.Usdt.Accounts["default"].Enable = "off" },
		"链上段缺失":          func(c *Cfg) { c.Tron.Usdt.Chain = nil },
		"链上段缺接入点":        func(c *Cfg) { c.Tron.Usdt.Chain.Node = "" },
		"network 填了代币标准": func(c *Cfg) { c.Tron.Usdt.Network = "trc20" },
		"小数位与合约不符":       func(c *Cfg) { c.Tron.Usdt.Decimals = 18 },
		"BSC 缺链 ID":      func(c *Cfg) { c.Bsc.Usdt.Chain.ChainID = 0 },
		"BSC 小数位不符":      func(c *Cfg) { c.Bsc.Usdt.Decimals = 6 },
		"SOL 缺 RPC":      func(c *Cfg) { c.Sol.Usdt.Chain.RPC = " " },
		"SOL 小数位不符":      func(c *Cfg) { c.Sol.Usdt.Decimals = 18 },
	}
	for name, mutate := range cases {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 未被拦下", name)
		} else if strings.Contains(err.Error(), "TOPSECRET") {
			t.Errorf("%s 报错含敏感值: %v", name, err)
		}
	}

	// 「写好但没开」是合法状态：四个通道段都只剩半截配置也不得拦启动。
	off := base()
	for _, s := range []*ChannelSupplierConfig{&off.Tron.Usdt.ChannelSupplierConfig} {
		s.Enable = "off"
		s.Driver = ""
		s.BaseURL = ""
		s.Accounts = nil
	}
	off.Tron.Usdt.Chain = nil
	off.Bsc.Usdt.Enable = "off"
	off.Bsc.Usdt.Chain = nil
	off.Sol.Usdt.Enable = "off"
	off.Sol.Usdt.Chain = nil
	if err := off.Validate(); err != nil {
		t.Errorf("未开启的段不应参与判定: %v", err)
	}

	// 全空配置（没有任何资金段）是正常缺省，不得拦启动。
	empty := &Cfg{}
	if err := empty.Validate(); err != nil {
		t.Errorf("未配置资金通道时 Validate 应当放行: %v", err)
	}
}

// waitOrDumpStacks 等待 done 关闭；超时即判定疑似死锁，并转储全部协程栈作为证据。
// 不用 go test 的全局 timeout：那会让整包挂住且丢失现场。
func waitOrDumpStacks(t *testing.T, done <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s 在 %s 内未结束，疑似死锁；全部协程栈：\n%s", what, timeout, buf[:n])
	}
}

// runGuarded 在独立协程里跑 body，捕获 panic 转成测试失败（非测试协程里不能调 t.Fatal）。
func runGuarded(t *testing.T, wg *sync.WaitGroup, name string, body func()) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 1<<16)
				n := runtime.Stack(buf, false)
				t.Errorf("%s panic: %v\n%s", name, r, buf[:n])
			}
		}()
		body()
	}()
}

// writeFeatureConfTree 造一份带功能配置目录的临时 conf 树，返回 confDir 与 featureDir。
func writeFeatureConfTree(t *testing.T, featureNames []string) (confDir, featureDir string) {
	t.Helper()
	base := t.TempDir()
	confDir = filepath.Join(base, "conf")
	featureDir = filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	iniText := "[GLOBAL]\ndebug=true\n\n[GateWayServer]\nini=gatewayserverbus.json\nconf_dir=feature_configs\n"
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniText), 0o644); err != nil {
		t.Fatal(err)
	}
	mainJSON := `{"log_level":"debug","map_path":"off"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, name := range featureNames {
		body := `{"value":` + itoa(i) + `}`
		if err := os.WriteFile(filepath.Join(featureDir, name+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return confDir, featureDir
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

type probeFeature struct {
	Value int `json:"value"`
}

// overlapProbe 是自带「并发解析重叠」探测的目标类型：json.Unmarshal 会走它的
// UnmarshalJSON，进入时 inside+1、退出时 inside-1；若发现同一个 target 上有两次解析
// 同时在飞，就累加 overlaps。没有 -race 的机器上，这是唯一能直接看见「注册路径与
// 加载路径并发写同一个用户结构体」的手段（_loadMu 要挡的正是这件事）。
type overlapProbe struct {
	Value    int
	want     int
	inside   int32
	overlaps int32
}

func (p *overlapProbe) UnmarshalJSON(data []byte) error {
	if atomic.AddInt32(&p.inside, 1) > 1 {
		atomic.AddInt32(&p.overlaps, 1)
	}
	defer atomic.AddInt32(&p.inside, -1)
	// 停留一小会儿，把可能存在的重叠窗口撑大到可观测
	time.Sleep(300 * time.Microsecond)
	var raw struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Value = raw.Value
	return nil
}

// useFeatureState 隔离全局状态：先快照路径与功能注册表，测试结束回滚，
// 避免本文件的并发用例把 _configDirFlag / _featureReg 留给后面的用例。
func useFeatureState(t *testing.T) {
	t.Helper()
	prevBase, prevConf, prevFile, prevFlag := snapshotPathState()
	resetFeatureState()
	t.Cleanup(func() {
		resetFeatureState()
		restorePathState(prevBase, prevConf, prevFile, prevFlag)
	})
}

// TestConcurrentRegisterFuncAndLoadWithError 多协程并发注册功能配置并加载主配置。
// 覆盖的竞争面：RegisterFunc 写 _featureReg 与读 _featureDirSet，LoadWithError 写
// _confDirField / _featureDir / _featureDirSet 并 Range _featureReg，同时读路径三元组。
//
// 最终状态一致的判据不是「恰好加载了多少个」，而是「每个注册过的名字都必须已加载」：
// 注册与加载共用 _loadMu 后，无论注册落在加载前还是加载后，都必然走到一条加载路径上；
// 若有人把锁拆掉，这里会出现两边都不加载的静默丢失。
func TestConcurrentRegisterFuncAndLoadWithError(t *testing.T) {
	useFeatureState(t)

	const (
		goroutines = 8
		rounds     = 6
	)
	names := make([]string, 0, goroutines*rounds)
	for g := range goroutines {
		for r := range rounds {
			names = append(names, "probe_g"+itoa(g)+"_r"+itoa(r))
		}
	}
	confDir, featureDir := writeFeatureConfTree(t, names)
	setConfigDirFlagForTest(confDir)

	// name -> 期望值（writeFeatureConfTree 按序号写 {"value": i}）
	want := make(map[string]int, len(names))
	for i, name := range names {
		want[name] = i
	}

	targets := make(map[string]*probeFeature, len(names))
	for _, name := range names {
		targets[name] = &probeFeature{}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		regErrs []string
		loadErr []string
	)
	for g := range goroutines {
		runGuarded(t, &wg, "register+load goroutine", func() {
			for r := range rounds {
				name := names[g*rounds+r]
				if err := RegisterFunc(name, targets[name]); err != nil {
					mu.Lock()
					regErrs = append(regErrs, name+": "+err.Error())
					mu.Unlock()
					continue
				}
				cfg := &Cfg{}
				if err := cfg.LoadWithError("GateWayServer"); err != nil {
					mu.Lock()
					loadErr = append(loadErr, name+": "+err.Error())
					mu.Unlock()
					continue
				}
				if cfg.LogLevel != "debug" {
					mu.Lock()
					loadErr = append(loadErr, name+": log_level="+cfg.LogLevel)
					mu.Unlock()
				}
				// 加载途中穿插只读 getter，制造与写侧的真实交错
				_ = GetConfDir()
				_ = GetDefaultFile()
				_ = GetBasePath()
				_ = GetFeatureDir()
				_ = GetServerID()
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitOrDumpStacks(t, done, 120*time.Second, "并发 RegisterFunc + LoadWithError")

	for _, e := range regErrs {
		t.Errorf("RegisterFunc 失败: %s", e)
	}
	for _, e := range loadErr {
		t.Errorf("LoadWithError 失败: %s", e)
	}

	// 最终状态一致：注册过的全部已加载，且加载出来的就是各自的 target 指针与期望值。
	for _, name := range names {
		if !IsFeatureLoaded(name) {
			t.Errorf("%s 注册后未被加载：注册与加载之间存在静默丢失窗口", name)
			continue
		}
		v, ok := GetFeatureConfig(name)
		if !ok {
			t.Errorf("GetFeatureConfig(%s) 未命中", name)
			continue
		}
		got, ok := v.(*probeFeature)
		if !ok {
			t.Errorf("GetFeatureConfig(%s) 类型 = %T，期望 *probeFeature", name, v)
			continue
		}
		if got != targets[name] {
			t.Errorf("GetFeatureConfig(%s) 返回了另一个指针", name)
		}
		if got.Value != want[name] {
			t.Errorf("%s.value = %d，期望 %d", name, got.Value, want[name])
		}
	}

	// 路径三元组必须成组一致，且指向本次的临时目录。
	wantConf, _ := filepath.Abs(confDir)
	if got := GetConfDir(); got != wantConf {
		t.Errorf("GetConfDir() = %s，期望 %s", got, wantConf)
	}
	if got, want := GetDefaultFile(), filepath.Join(wantConf, "config.ini"); got != want {
		t.Errorf("GetDefaultFile() = %s，期望 %s", got, want)
	}
	if got, want := GetBasePath(), filepath.Dir(wantConf); got != want {
		t.Errorf("GetBasePath() = %s，期望 %s", got, want)
	}
	wantFeature, _ := filepath.Abs(featureDir)
	if got := GetFeatureDir(); got != wantFeature {
		t.Errorf("GetFeatureDir() = %s，期望 %s", got, wantFeature)
	}
	if GetDefaultFie() != GetDefaultFile() {
		t.Errorf("GetDefaultFie() = %s 与 GetDefaultFile() = %s 不一致", GetDefaultFie(), GetDefaultFile())
	}
}

// TestConcurrentLoadWithErrorOnSharedCfg 多个协程往同一个 *Cfg 加载（app.go 里
// config.Cfg_ 就是这个形态）。json.Unmarshal 并发写同一目标是数据竞争，
// _loadMu 的作用之一就是把它串行化；这里断言不 panic 且最终字段等于文件内容。
func TestConcurrentLoadWithErrorOnSharedCfg(t *testing.T) {
	useFeatureState(t)
	confDir, _ := writeFeatureConfTree(t, nil)
	setConfigDirFlagForTest(confDir)

	shared := &Cfg{}
	var wg sync.WaitGroup
	for range 6 {
		runGuarded(t, &wg, "shared cfg loader", func() {
			for range 20 {
				if err := shared.LoadWithError("GateWayServer"); err != nil {
					t.Errorf("LoadWithError 失败: %v", err)
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitOrDumpStacks(t, done, 60*time.Second, "共享 *Cfg 并发加载")

	if shared.LogLevel != "debug" {
		t.Errorf("共享 Cfg.LogLevel = %q，期望 %q", shared.LogLevel, "debug")
	}
	if shared.MapPath != "off" {
		t.Errorf("共享 Cfg.MapPath = %q，期望 %q", shared.MapPath, "off")
	}
}

// TestConcurrentPathResolutionKeepsGroupConsistent 一侧反复切换 conf 目录并重解析，
// 另一侧高频读路径状态。断言的是「成组一致性」：_confDir / _defaultFile / _basePath
// 三者永远来自同一次解析——这正是单变量 atomic 给不了、必须用一把锁盖住整组的理由。
func TestConcurrentPathResolutionKeepsGroupConsistent(t *testing.T) {
	withConfigPaths(t)
	_, confA := writePlainConf(t, "a")
	_, confB := writePlainConf(t, "b")
	absA, _ := filepath.Abs(confA)
	absB, _ := filepath.Abs(confB)
	// 先落到 A 再开读协程：否则第一轮 ApplyFlags 之前，读侧会看到上一个用例留下的
	// 全局路径（-count=2 起的「不属于 A/B」就是这条窗口），那是测试隔离问题不是路径撕裂。
	setConfigDirFlagForTest(confA)
	ApplyFlags()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧：只允许看到 A 组或 B 组，混出 A 的目录配 B 的 ini 即为组内撕裂。
	for range 4 {
		runGuarded(t, &wg, "path reader", func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				base, conf, defFile, _ := snapshotPathState()
				if conf == "" {
					runtime.Gosched()
					continue
				}
				if defFile != filepath.Join(conf, "config.ini") {
					t.Errorf("路径组撕裂：conf=%s 而 ini=%s", conf, defFile)
					return
				}
				if base != filepath.Dir(conf) {
					t.Errorf("路径组撕裂：conf=%s 而 base=%s", conf, base)
					return
				}
				if conf != absA && conf != absB {
					t.Errorf("conf=%s 不属于任何一次解析结果", conf)
					return
				}
				// 公开 getter 与兼容包装在并发下都必须返回合法值。
				// 注意不能断言 GetDefaultFie() == GetDefaultFile()：那是两次独立调用，
				// 中间写侧完全可能已切目录（调用方的 TOCTOU，不是数据竞争）；
				// 相等性放到静止后的 TestConcurrentRegisterFuncAndLoadWithError 里断。
				iniA := filepath.Join(absA, "config.ini")
				iniB := filepath.Join(absB, "config.ini")
				for _, v := range []string{GetDefaultFie(), GetDefaultFile()} {
					if v != iniA && v != iniB {
						t.Errorf("并发下 config.ini 路径非法: %s", v)
						return
					}
				}
				_ = GetServerID()
				_ = GetFeatureDir()
				_ = GetBasePath()
				runtime.Gosched()
			}
		})
	}

	// 写侧：交替把 -c 指到 A/B 并走公开的 ApplyFlags 重解析（内含 setConfDir 写入）。
	const switches = 400
	runGuarded(t, &wg, "path resolver", func() {
		defer close(stop)
		for i := range switches {
			if i%2 == 0 {
				setConfigDirFlagForTest(confA)
			} else {
				setConfigDirFlagForTest(confB)
			}
			ApplyFlags()
			runtime.Gosched()
		}
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitOrDumpStacks(t, done, 60*time.Second, "并发路径解析")

	// 静止后必须精确等于最后一次解析结果（最后一次下标是 switches-1，为奇数 → confB）。
	base, conf, defFile, flagDir := snapshotPathState()
	if conf != absB || defFile != filepath.Join(absB, "config.ini") || base != filepath.Dir(absB) {
		t.Errorf("静止后路径组不一致：base=%s conf=%s ini=%s，期望均属于 %s", base, conf, defFile, absB)
	}
	if flagDir != confB {
		t.Errorf("静止后 -c = %s，期望 %s", flagDir, confB)
	}
}

// writePlainConf 造一份只有主配置、没有功能目录的 conf 树。
func writePlainConf(t *testing.T, tag string) (base, conf string) {
	t.Helper()
	base = t.TempDir()
	conf = filepath.Join(base, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	iniText := "[GateWayServer]\nini=gate_" + tag + ".json\n"
	if err := os.WriteFile(filepath.Join(conf, "config.ini"), []byte(iniText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "gate_"+tag+".json"), []byte(`{"log_level":"info"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, conf
}

// TestConcurrentRegisterFuncDuplicateOnlyOneWins 同名注册在多协程下必须恰好一个成功。
// _featureReg.LoadOrStore 本身是原子的，这条钉的是「加锁改造没有把去重语义改坏」，
// 同时验证败者拿到的是明确的错误而不是静默成功。
func TestConcurrentRegisterFuncDuplicateOnlyOneWins(t *testing.T) {
	useFeatureState(t)
	confDir, featureDir := writeFeatureConfTree(t, []string{"dup_probe"})
	// 把探针值改成非零，这样「胜者确实读到了文件」才有区分度。
	if err := os.WriteFile(filepath.Join(featureDir, "dup_probe.json"), []byte(`{"value":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setConfigDirFlagForTest(confDir)

	// 先跑一次加载，让 _featureDirSet 置位，使 RegisterFunc 走「立即加载」分支。
	if err := (&Cfg{}).LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("预热加载失败: %v", err)
	}

	const contenders = 16
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		winners int
		losers  int
		badErr  []string
	)
	for range contenders {
		runGuarded(t, &wg, "duplicate registrar", func() {
			<-start
			target := &probeFeature{}
			err := RegisterFunc("dup_probe", target)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
				if target.Value != 7 {
					badErr = append(badErr, "胜者未加载到文件内容，value="+itoa(target.Value)+" 期望 7")
				}
			case strings.Contains(err.Error(), "功能配置已注册"):
				losers++
			default:
				badErr = append(badErr, err.Error())
			}
		})
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitOrDumpStacks(t, done, 60*time.Second, "同名并发注册")

	if winners != 1 {
		t.Errorf("胜者数 = %d，期望 1（去重语义被并发破坏）", winners)
	}
	if losers != contenders-1 {
		t.Errorf("败者数 = %d，期望 %d", losers, contenders-1)
	}
	for _, e := range badErr {
		t.Errorf("非预期错误: %s", e)
	}
	if !IsFeatureLoaded("dup_probe") {
		t.Error("dup_probe 最终未加载")
	}
}

// TestRegisterFuncRacingSingleLoadNoLossNoDoubleUnmarshal 复刻生产形态：启动时只加载
// 一次，业务在各协程里注册功能配置（app.loadConfig 只有一次 LoadWithError，之后没人再拉）。
// 断言两件事：
//
//  1. 注册返回成功的配置最终必须已加载，且读到的就是文件里的值（端到端不丢、不串）；
//  2. 同一个 target 上从未出现并发解析重叠（overlaps == 0）——这是 _loadMu 真正拦住的
//     缺陷：注册方读到 _featureDirSet 已置位而立即解析，而加载方的 Range 又收走了同一
//     个名字，两个协程就会并发 json.Unmarshal 写同一个用户结构体。
//
// 多轮重复 + 解析侧停留是为了放大重叠窗口的命中率；它只能捕获较宽的重叠，窄窗口仍
// 靠 _loadMu 的临界区覆盖与固定锁序论证（本机无 C 工具链，-race 不可用）。
func TestRegisterFuncRacingSingleLoadNoLossNoDoubleUnmarshal(t *testing.T) {
	useFeatureState(t)

	const (
		rounds     = 12
		registrars = 8
	)
	all := make([]string, 0, rounds*registrars)
	for r := range rounds {
		for i := range registrars {
			all = append(all, "race_r"+itoa(r)+"_i"+itoa(i))
		}
	}
	confDir, _ := writeFeatureConfTree(t, all)
	setConfigDirFlagForTest(confDir)

	// name -> 探针与期望值（writeFeatureConfTree 按序号写 {"value": i}）
	probes := make(map[string]*overlapProbe, len(all))
	for i, name := range all {
		probes[name] = &overlapProbe{want: i}
	}

	for r := range rounds {
		var wg sync.WaitGroup
		for i := range registrars {
			name := "race_r" + itoa(r) + "_i" + itoa(i)
			target := probes[name]
			runGuarded(t, &wg, "racing registrar", func() {
				if i%2 == 1 {
					// 一半注册者让出一个调度位，使其更可能落在加载途中
					runtime.Gosched()
				}
				if err := RegisterFunc(name, target); err != nil {
					t.Errorf("RegisterFunc(%s) 失败: %v", name, err)
				}
			})
		}
		runGuarded(t, &wg, "single loader", func() {
			if err := (&Cfg{}).LoadWithError("GateWayServer"); err != nil {
				t.Errorf("第 %d 轮 LoadWithError 失败: %v", r, err)
			}
		})

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		waitOrDumpStacks(t, done, 60*time.Second, "注册与单次加载竞速")

		for i := range registrars {
			name := "race_r" + itoa(r) + "_i" + itoa(i)
			probe := probes[name]
			if !IsFeatureLoaded(name) {
				t.Fatalf("第 %d 轮：%s 注册返回成功却未被加载", r, name)
			}
			if n := atomic.LoadInt32(&probe.overlaps); n != 0 {
				t.Fatalf("第 %d 轮：%s 上观测到 %d 次并发解析重叠——注册路径与加载路径同时写同一个 target", r, name, n)
			}
			if probe.Value != probe.want {
				t.Fatalf("第 %d 轮：%s.value = %d，期望 %d", r, name, probe.Value, probe.want)
			}
		}
	}
}

// TestPackageStateAccessIsGuarded 互斥覆盖论证的机器可查版本。
//
// 规则：本包任何非测试源文件里的函数，只要引用了受保护的包级全局，就必须
//   - 自己持 _stateMu（Lock/Unlock/RLock/RUnlock 任一），或
//   - 调用了一个已满足上述条件的函数（不动点迭代推导，故 getter/setter 封装自动传递）。
//
// 唯一豁免是 init：Go 规范保证包初始化在单协程内、且早于任何用户协程完成。
func TestPackageStateAccessIsGuarded(t *testing.T) {
	guarded := map[string]bool{
		"_configDirFlag": true,
		"_basePath":      true,
		"_confDir":       true,
		"_defaultFile":   true,
		"_defServerId":   true,
		"_confDirField":  true,
		"_featureDir":    true,
		"_featureDirSet": true,
	}
	lockMethods := map[string]bool{"Lock": true, "Unlock": true, "RLock": true, "RUnlock": true}

	type funcInfo struct {
		where string
		refs  map[string]bool
		locks bool
		calls map[string]bool
	}

	fset := token.NewFileSet()
	infos := map[string]*funcInfo{}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("未找到任何 .go 源文件，测试的工作目录不对")
	}
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, src, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", src, err)
		}
		if file.Name.Name != "config" {
			continue // 带 //go:build ignore 的示例文件不属于本包，不进调用图
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = receiverName(fn.Recv.List[0].Type) + "." + name
			}
			info := &funcInfo{
				where: fset.Position(fn.Pos()).String(),
				refs:  map[string]bool{},
				calls: map[string]bool{},
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.Ident:
					if guarded[node.Name] {
						info.refs[node.Name] = true
					}
				case *ast.SelectorExpr:
					if x, ok := node.X.(*ast.Ident); ok && x.Name == "_stateMu" && lockMethods[node.Sel.Name] {
						info.locks = true
					}
				case *ast.CallExpr:
					// 只记包内顶层函数调用（Ident 形式）；方法调用不参与推导，
					// 否则会因同名方法把不加锁的函数误判成已保护。
					if id, ok := node.Fun.(*ast.Ident); ok {
						info.calls[id.Name] = true
					}
				}
				return true
			})
			if prev, dup := infos[name]; dup {
				t.Fatalf("函数名重复，无法建立调用图: %s（%s 与 %s）", name, prev.where, info.where)
			}
			infos[name] = info
		}
	}

	// 不动点：guarding(f) = f 自己加锁 || f 调用的某个包内函数已 guarding
	guarding := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, info := range infos {
			if guarding[name] {
				continue
			}
			if info.locks {
				guarding[name] = true
				changed = true
				continue
			}
			for callee := range info.calls {
				if guarding[callee] {
					guarding[name] = true
					changed = true
					break
				}
			}
		}
	}

	for name, info := range infos {
		if len(info.refs) == 0 || guarding[name] {
			continue
		}
		if name == "init" {
			continue // 包初始化由 Go 规范保证单协程且早于用户协程
		}
		refs := make([]string, 0, len(info.refs))
		for r := range info.refs {
			refs = append(refs, r)
		}
		t.Errorf("%s（%s）引用受保护全局 %s，但既未持 _stateMu 也未调用已持锁的封装函数",
			name, info.where, strings.Join(refs, ", "))
	}
}

// receiverName 取方法接收者的类型名，用于把方法与顶层函数在调用图里区分开。
func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	default:
		return "unknown"
	}
}

// TestRegisterFuncBeforeLoad 测试在 LoadWithError 之前注册功能配置
func TestRegisterFuncBeforeLoad(t *testing.T) {
	// 创建临时目录结构
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 创建 config.ini
	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建主配置文件
	mainJSON := `{
		"log_level": "info"
	}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建功能配置文件
	type GameRules struct {
		MaxPlayers int    `json:"max_players"`
		TimeLimit  int    `json:"time_limit"`
		Mode       string `json:"mode"`
	}
	featureJSON := `{
		"max_players": 10,
		"time_limit": 300,
		"mode": "classic"
	}`
	if err := os.WriteFile(filepath.Join(featureDir, "game_rules.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// 重置全局状态
	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	// 在 LoadWithError 之前注册
	rules := &GameRules{}
	if err := RegisterFunc("game_rules", rules); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// 加载配置
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证 struct 被正确填充
	if rules.MaxPlayers != 10 {
		t.Errorf("MaxPlayers = %d, want 10", rules.MaxPlayers)
	}
	if rules.TimeLimit != 300 {
		t.Errorf("TimeLimit = %d, want 300", rules.TimeLimit)
	}
	if rules.Mode != "classic" {
		t.Errorf("Mode = %q, want %q", rules.Mode, "classic")
	}

	// 验证 GetFeatureConfig
	v, ok := GetFeatureConfig("game_rules")
	if !ok {
		t.Fatal("GetFeatureConfig returned false")
	}
	if v != rules {
		t.Error("GetFeatureConfig returned different pointer")
	}

	// 验证 IsFeatureLoaded
	if !IsFeatureLoaded("game_rules") {
		t.Error("IsFeatureLoaded returned false")
	}
}

// TestRegisterFuncWithNil 测试注册 nil 时按 map[string]any 读取
func TestRegisterFuncWithNil(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{
		"key1": "value1",
		"key2": 42,
		"key3": true
	}`
	if err := os.WriteFile(filepath.Join(featureDir, "dynamic_config.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	// 注册 nil
	if err := RegisterFunc("dynamic_config", nil); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证 map[string]any
	m, ok := GetFeatureConfigMap("dynamic_config")
	if !ok {
		t.Fatal("GetFeatureConfigMap returned false")
	}
	if m["key1"] != "value1" {
		t.Errorf("key1 = %v, want %q", m["key1"], "value1")
	}
	if m["key2"] != float64(42) {
		t.Errorf("key2 = %v, want 42", m["key2"])
	}
	if m["key3"] != true {
		t.Errorf("key3 = %v, want true", m["key3"])
	}
}

// TestRegisterFuncAfterLoad 测试在 LoadWithError 之后注册功能配置
func TestRegisterFuncAfterLoad(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{"reward": 100}`
	if err := os.WriteFile(filepath.Join(featureDir, "reward.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	// 先加载主配置
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	// 验证功能配置未加载
	if IsFeatureLoaded("reward") {
		t.Error("reward should not be loaded yet")
	}

	// 之后注册
	type RewardConfig struct {
		Reward int `json:"reward"`
	}
	reward := &RewardConfig{}
	if err := RegisterFunc("reward", reward); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// 验证立即加载
	if !IsFeatureLoaded("reward") {
		t.Error("reward should be loaded after RegisterFunc")
	}
	if reward.Reward != 100 {
		t.Errorf("Reward = %d, want 100", reward.Reward)
	}
}

// TestRegisterFuncDuplicate 测试重复注册返回错误
func TestRegisterFuncDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	// 第一次注册
	if err := RegisterFunc("test", nil); err != nil {
		t.Fatalf("First RegisterFunc failed: %v", err)
	}

	// 第二次注册应该返回错误
	if err := RegisterFunc("test", nil); err == nil {
		t.Error("Second RegisterFunc should return error")
	}
}

// TestFeatureConfigMissing 测试 JSON 文件不存在时的错误处理
func TestFeatureConfigMissing(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	featureDir := filepath.Join(confDir, "feature_configs")
	if err := os.MkdirAll(featureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=feature_configs
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	// 注册不存在的配置
	if err := RegisterFunc("missing", nil); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	// LoadWithError 应该返回错误
	cfg := &Cfg{}
	err := cfg.LoadWithError("GateWayServer")
	if err == nil {
		t.Error("LoadWithError should return error for missing config")
	}
}

// TestFeatureConfigAbsolutePath 测试绝对路径
func TestFeatureConfigAbsolutePath(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "conf")
	absFeatureDir := filepath.Join(tmpDir, "absolute_features")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(absFeatureDir, 0755); err != nil {
		t.Fatal(err)
	}

	iniContent := `[GLOBAL]
debug=true

[GateWayServer]
ini=gatewayserverbus.json
conf_dir=` + absFeatureDir + `
`
	if err := os.WriteFile(filepath.Join(confDir, "config.ini"), []byte(iniContent), 0644); err != nil {
		t.Fatal(err)
	}

	mainJSON := `{"log_level": "info"}`
	if err := os.WriteFile(filepath.Join(confDir, "gatewayserverbus.json"), []byte(mainJSON), 0644); err != nil {
		t.Fatal(err)
	}

	featureJSON := `{"value": "absolute"}`
	if err := os.WriteFile(filepath.Join(absFeatureDir, "abs_config.json"), []byte(featureJSON), 0644); err != nil {
		t.Fatal(err)
	}

	resetFeatureState()
	setConfigDirFlagForTest(confDir)

	type AbsConfig struct {
		Value string `json:"value"`
	}
	absCfg := &AbsConfig{}
	if err := RegisterFunc("abs_config", absCfg); err != nil {
		t.Fatalf("RegisterFunc failed: %v", err)
	}

	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatalf("LoadWithError failed: %v", err)
	}

	if absCfg.Value != "absolute" {
		t.Errorf("Value = %q, want %q", absCfg.Value, "absolute")
	}

	// 验证 GetFeatureDir 返回绝对路径
	if GetFeatureDir() != absFeatureDir {
		t.Errorf("GetFeatureDir = %q, want %q", GetFeatureDir(), absFeatureDir)
	}
}

// TestNormalizeJSONName 测试文件名规范化
func TestNormalizeJSONName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"game_rules", "game_rules.json"},
		{"game_rules.json", "game_rules.json"},
		{"GAME_RULES.JSON", "GAME_RULES.JSON"},
		{"config", "config.json"},
		{"  test  ", "test.json"},
	}

	for _, tt := range tests {
		result := normalizeJSONName(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeJSONName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// resetFeatureState 重置功能配置全局状态（仅用于测试）。
// 必须在没有任何并发注册/加载时调用：_stateMu 只能盖住普通字段，
// 整体替换 sync.Map 要求调用方处于静止态（本包测试默认串行，满足该前提）。
func resetFeatureState() {
	_stateMu.Lock()
	_confDirField = ""
	_featureDir = ""
	_featureDirSet = false
	_stateMu.Unlock()
	_featureReg = sync.Map{}
	_featureData = sync.Map{}
	_featureLoaded = sync.Map{}
}

// TestValidateGinPath 覆盖反向代理 ginpath 项的格式轻校验：合法放行、非法逐类报错。
func TestValidateGinPath(t *testing.T) {
	valid := []string{
		"/proxy/hello|GET",
		"/proxy/user/:id|GET",
		"/onlypath",  // 省略方法 = 通配
		"/trailing|", // 显式空方法 = 通配
		"/a/b/c|POST",
		"/x/:y/:z|DELETE", // 多参数段
		"/a/:b|GET",       // 参数段冒号后有字符
	}
	for _, s := range valid {
		if err := validateGinPath(s); err != nil {
			t.Fatalf("✘ 合法项 %q 被拒: %v", s, err)
		}
	}

	invalid := []string{
		"",                // 空
		"proxy/hello|GET", // 缺前导斜杠
		"/hello|get",      // 方法小写
		"/hello|Patch",    // 方法非全大写
		"/hello|FOO",      // 非法方法名
		"/a/:|GET",        // ":name" 段冒号后为空
	}
	for _, s := range invalid {
		if err := validateGinPath(s); err == nil {
			t.Fatalf("✘ 非法项 %q 未报错", s)
		}
	}
}

// TestValidateGinPathThroughCfg 走完整 Validate：client.ginpath 非法应阻断启动，合法放行。
func TestValidateGinPathThroughCfg(t *testing.T) {
	base := func(ginpath []string) *Cfg {
		return &Cfg{
			Web: &WebConfig{HTTPPort: 1000},
			Rpc: &RpcConfig{
				Client: []*RpcAddr{{Name: "c", Addr: "127.0.0.1", Port: 7100, GinPath: ginpath}},
			},
		}
	}
	if err := base([]string{"/ok|GET", "/u/:id"}).Validate(); err != nil {
		t.Fatalf("✘ 合法 ginpath 被拒: %v", err)
	}
	if err := base([]string{"bad|GET"}).Validate(); err == nil {
		t.Fatal("✘ 非法 ginpath（缺前导斜杠）应报错")
	}
}

// snapshotPathState / restorePathState / setConfigDirFlagForTest 是测试对包级路径状态的
// 唯一入口：这些字段已由 _stateMu 保护，测试也不能绕过锁直接赋值，否则与
// 并发用例（concurrency_state_test.go）同时在飞时会自己造成竞争。
func snapshotPathState() (base, conf, defFile, flagDir string) {
	_stateMu.RLock()
	defer _stateMu.RUnlock()
	return _basePath, _confDir, _defaultFile, _configDirFlag
}

func restorePathState(base, conf, defFile, flagDir string) {
	_stateMu.Lock()
	_basePath, _confDir, _defaultFile, _configDirFlag = base, conf, defFile, flagDir
	_stateMu.Unlock()
}

func setConfigDirFlagForTest(dir string) {
	_stateMu.Lock()
	_configDirFlag = dir
	_stateMu.Unlock()
}

func withConfigPaths(t *testing.T) {
	t.Helper()
	prevBase, prevConf, prevFile, prevFlag := snapshotPathState()
	t.Cleanup(func() {
		restorePathState(prevBase, prevConf, prevFile, prevFlag)
	})
}

func writeTempConf(t *testing.T) (base, conf string) {
	t.Helper()
	base = t.TempDir()
	conf = filepath.Join(base, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := "[GateWayServer]\nini=gate.json\n"
	if err := os.WriteFile(filepath.Join(conf, "config.ini"), []byte(ini), 0o644); err != nil {
		t.Fatal(err)
	}
	json := `{"log_level":"info","map_path":"off"}`
	if err := os.WriteFile(filepath.Join(conf, "gate.json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, conf
}

func TestApplyConfigDirAsConfFolder(t *testing.T) {
	withConfigPaths(t)
	base, conf := writeTempConf(t)
	applyConfigDir(conf)
	if GetConfDir() != conf {
		if abs, _ := filepath.Abs(conf); GetConfDir() != abs {
			t.Fatalf("conf dir=%s want %s", GetConfDir(), conf)
		}
	}
	if filepath.Base(GetDefaultFile()) != "config.ini" {
		t.Fatalf("ini=%s", GetDefaultFile())
	}
	if GetBasePath() != filepath.Dir(GetConfDir()) {
		t.Fatalf("base=%s dir(conf)=%s", GetBasePath(), filepath.Dir(GetConfDir()))
	}
	if GetBasePath() != base && GetBasePath() != filepath.Clean(base) {
		abs, _ := filepath.Abs(base)
		if GetBasePath() != abs {
			t.Fatalf("base path=%s want %s", GetBasePath(), base)
		}
	}
}

func TestApplyConfigDirAsParentOfConf(t *testing.T) {
	withConfigPaths(t)
	base, conf := writeTempConf(t)
	applyConfigDir(base)
	got, _ := filepath.Abs(conf)
	if GetConfDir() != got && GetConfDir() != conf {
		t.Fatalf("conf dir=%s want %s", GetConfDir(), conf)
	}
}

func TestLoadWithErrorFromConfDir(t *testing.T) {
	withConfigPaths(t)
	_, conf := writeTempConf(t)
	setConfigDirFlagForTest(conf)
	cfg := &Cfg{}
	if err := cfg.LoadWithError("GateWayServer"); err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("log_level=%s", cfg.LogLevel)
	}
}

func TestLoadWithErrorMissingServerSection(t *testing.T) {
	withConfigPaths(t)
	_, conf := writeTempConf(t)
	setConfigDirFlagForTest(conf)
	cfg := &Cfg{}
	if err := cfg.LoadWithError("UnknownServer"); err == nil {
		t.Fatal("expected empty ini name error")
	}
}

// TestExampleJsonPostgresBinds 示例里的 postgres 段必须逐字段绑定到 PostgreDBConfig。
// encoding/json 对未知键静默丢弃：键名写错的配置看着生效、实则永远走默认值。
func TestExampleJsonPostgresBinds(t *testing.T) {
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if cfg.Postgre == nil {
		t.Fatal(`example.json 缺少 "postgres" 配置段`)
	}
	pg := cfg.Postgre
	// 逐字段核对示例值确实落进了结构体（任一 json tag 写错都会在这里露馅）
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"pg_host", pg.Host, "127.0.0.1"},
		{"pg_port", pg.Port, 5432},
		{"pg_username", pg.Username, "root"},
		{"pg_password", pg.Password, "123456"},
		{"pg_dbname", pg.DBName, "example"},
		{"pg_sslmode", pg.SSLMode, "disable"},
		{"pg_timezone", pg.TimeZone, "UTC"},
		{"pg_schema", pg.Schema, "public"},
		{"pg_max_idle_conns", pg.MaxIdleConns, 100},
		{"pg_max_open_conns", pg.MaxOpenConns, 500},
		{"pg_conn_max_lifetime", pg.ConnMaxLifetime, 86400},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 绑定错: 期望 %v, 得 %v", c.name, c.want, c.got)
		}
	}
}

// tonNativeBase 是一份可被 Validate 放行的 ton.native 配置。
// 地址只做非空校验，装配期才验派生地址；这里不编译 ton 包，避免 config → ton → config 循环。
func tonNativeBase() *Cfg {
	return &Cfg{Ton: &TonConfig{Native: &TonNativeConfig{
		Enable:  "on",
		Driver:  "ton_tonapi",
		Network: "mainnet",
		Chain: &TonChainConfig{
			WalletAddress: "EQ-CHANGE-ME-HOT-WALLET",
		},
	}}}
}

func TestTonNativeEnabled(t *testing.T) {
	cases := map[string]bool{
		"":        false,
		"off":     false,
		"on":      true,
		" ON ":    true,
		"enabled": false,
	}
	for raw, want := range cases {
		got := (&TonNativeConfig{Enable: raw}).Enabled()
		if got != want {
			t.Errorf("Enabled(%q)=%v，期望 %v", raw, got, want)
		}
	}
}

func TestValidateTonNative(t *testing.T) {
	if err := tonNativeBase().Validate(); err != nil {
		t.Fatalf("合法 ton.native 被拒: %v", err)
	}

	cases := map[string]func(*TonNativeConfig){
		"缺 driver":         func(n *TonNativeConfig) { n.Driver = " " },
		"jetton 非空":        func(n *TonNativeConfig) { n.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs" },
		"network 非法":       func(n *TonNativeConfig) { n.Network = "ton" },
		"chain 为空":         func(n *TonNativeConfig) { n.Chain = nil },
		"wallet_address 空": func(n *TonNativeConfig) { n.Chain.WalletAddress = " " },
	}
	for name, mutate := range cases {
		cfg := tonNativeBase()
		mutate(cfg.Ton.Native)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s 未被拦下", name)
		}
	}

	// 没开闸时半截配置不拦启动：示例与「先写配置、后开开关」都是这个形态。
	off := tonNativeBase()
	off.Ton.Native.Enable = "off"
	off.Ton.Native.Driver = ""
	off.Ton.Native.Chain = nil
	if err := off.Validate(); err != nil {
		t.Fatalf("未开启的 ton.native 不该被校验拦下: %v", err)
	}
}

// TestValidateRejectsLegacyTelegramTon 旧段必须明确报迁移错误，
// 不能被 encoding/json 静默丢弃后表现为「通道没启动」。
func TestValidateRejectsLegacyTelegramTon(t *testing.T) {
	var cfg Cfg
	raw := []byte(`{"telegram":{"bot_token":"x","ton":{"enable":"on"}}}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ton.native") {
		t.Fatalf("旧 telegram.ton 应明确报迁移错误，实得 %v", err)
	}
}

// TestTonNativeSectionParses 钉住新的 JSON 形状：顶层 ton.native，不再有供应商字段。
func TestTonNativeSectionParses(t *testing.T) {
	var cfg Cfg
	raw := []byte(`{"ton":{"native":{
		"enable":"on","driver":"ton_tonapi","network":"testnet","jetton":"",
		"chain":{"http_base":"https://testnet.tonapi.io","wallet_address":"EQ-TEST"}
	}}}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	n := cfg.Ton.Native
	if n == nil || !n.Enabled() || n.Driver != "ton_tonapi" || n.Network != "testnet" {
		t.Fatalf("ton.native 字段没有绑定: %+v", n)
	}
	if n.Chain == nil || n.Chain.HTTPBase != "https://testnet.tonapi.io" || n.Chain.WalletAddress != "EQ-TEST" {
		t.Fatalf("ton.native.chain 字段没有绑定: %+v", n.Chain)
	}
}

func TestValidateNil(t *testing.T) {
	var c *Cfg
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

// TestValidateCacheDBType 验证 cache.db_type 只接受框架已实现的三种后端。
func TestValidateCacheDBType(t *testing.T) {
	redis := &RedisConfig{Host: "127.0.0.1:6379"}
	tests := []struct {
		name    string
		dbType  string
		wantErr bool
	}{
		{name: "空值按 mysql", dbType: "", wantErr: false},
		{name: "mysql", dbType: DBTypeMySQL, wantErr: false},
		{name: "mongo", dbType: DBTypeMongo, wantErr: false},
		{name: "postgres", dbType: DBTypePostgres, wantErr: false},
		{name: "大小写不敏感", dbType: "MySQL", wantErr: false},
		{name: "不支持的后端", dbType: "sqlite", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCache(&CacheConfig{Enabled: true, DBType: tc.dbType}, redis)
			if (err != nil) != tc.wantErr {
				t.Fatalf("db_type=%q err=%v wantErr=%v", tc.dbType, err, tc.wantErr)
			}
		})
	}
}

func TestTimerGoroutineLimit(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Cfg
		want int
	}{
		{name: "nil config", want: DefaultMaxTimerGoroutines},
		{name: "nil server", cfg: &Cfg{}, want: DefaultMaxTimerGoroutines},
		{name: "unset value", cfg: &Cfg{Server: &ServerConfig{}}, want: DefaultMaxTimerGoroutines},
		{name: "non-positive value", cfg: &Cfg{Server: &ServerConfig{MaxTimerGoroutines: -1}}, want: DefaultMaxTimerGoroutines},
		{name: "configured value", cfg: &Cfg{Server: &ServerConfig{MaxTimerGoroutines: 24}}, want: 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.TimerGoroutineLimit(); got != tt.want {
				t.Fatalf("TimerGoroutineLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestServerMaxTimerGoroutinesJSON(t *testing.T) {
	var cfg Cfg
	if err := json.Unmarshal([]byte(`{"server":{"max_timer_goroutines":73}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.TimerGoroutineLimit(); got != 73 {
		t.Fatalf("TimerGoroutineLimit() = %d, want 73", got)
	}
}

func TestValidatePortConflict(t *testing.T) {
	c := &Cfg{
		Web:      &WebConfig{HTTPPort: 8000},
		Ws:       &WebsocketConfig{Port: []*WebsocketPort{{Port: 8000}}},
		LogLevel: "info",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected port conflict")
	}
}

func TestValidateOK(t *testing.T) {
	c := &Cfg{
		Web: &WebConfig{HTTPPort: 1000},
		Ws: &WebsocketConfig{
			Port:           []*WebsocketPort{{Port: 8000}},
			CheckOrigin:    true,
			AllowedOrigins: []string{"http://127.0.0.1:3000"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateModelAPIMultiModel(t *testing.T) {
	c := &Cfg{ModelAPI: &ModelAPIConfig{
		Enable:   "on",
		Default:  "a",
		Strategy: "cheapest",
		Providers: map[string]*ModelProviderConfig{
			"a": {
				BaseURL: "https://x/v1", APIKey: "k", Model: "m1",
				Models: []*ModelInfoConfig{
					{Name: "m1", InputPrice: 1, OutputPrice: 2, SupportsTools: true},
					{Name: "m2", InputPrice: 0.1, OutputPrice: 0.2},
				},
			},
			// 未配置 model，应取 models 首个作为默认模型
			"b": {
				BaseURL: "https://y/v1", APIKey: "k",
				Models: []*ModelInfoConfig{{Name: "n1", InputPrice: 0.01, OutputPrice: 0.02}},
			},
		},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("合法多模型配置应通过校验: %v", err)
	}
	if got := c.ModelAPI.StrategyOf(); got != ModelStrategyCheapest {
		t.Errorf("StrategyOf = %q, want %q", got, ModelStrategyCheapest)
	}
	if got := c.ModelAPI.Providers["b"].DefaultModel(); got != "n1" {
		t.Errorf("DefaultModel = %q, want n1", got)
	}
	// 未配置 strategy 时按 default 处理
	if got := (&ModelAPIConfig{}).StrategyOf(); got != ModelStrategyDefault {
		t.Errorf("StrategyOf(空) = %q, want %q", got, ModelStrategyDefault)
	}
	if !(&ModelAPIConfig{Strategy: " CHEAPEST "}).StrategyValid() {
		t.Error("策略取值应忽略大小写与空白")
	}
	if (&ModelAPIConfig{Strategy: "smart"}).StrategyValid() {
		t.Error("未知策略取值应判定为非法")
	}
}

func TestValidateModelAPIErrors(t *testing.T) {
	base := func(mut func(*ModelAPIConfig)) *Cfg {
		c := &Cfg{ModelAPI: &ModelAPIConfig{
			Enable:  "on",
			Default: "a",
			Providers: map[string]*ModelProviderConfig{
				"a": {
					BaseURL: "https://x/v1", APIKey: "k", Model: "m1",
					Models: []*ModelInfoConfig{{Name: "m1", InputPrice: 1, OutputPrice: 2}},
				},
			},
		}}
		mut(c.ModelAPI)
		return c
	}

	cases := []struct {
		name string
		mut  func(*ModelAPIConfig)
	}{
		{"策略非法", func(m *ModelAPIConfig) { m.Strategy = "smart" }},
		{"模型名为空", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].Name = " " }},
		{"模型条目为空", func(m *ModelAPIConfig) { m.Providers["a"].Models = append(m.Providers["a"].Models, nil) }},
		{"价格为负", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].InputPrice = -1 }},
		{"模型名重复", func(m *ModelAPIConfig) {
			m.Providers["a"].Models = append(m.Providers["a"].Models, &ModelInfoConfig{Name: "m1", InputPrice: 1})
		}},
		{"默认模型不在列表中", func(m *ModelAPIConfig) { m.Providers["a"].Model = "other" }},
		{"model 与 models 同为空", func(m *ModelAPIConfig) {
			m.Providers["a"].Model = ""
			m.Providers["a"].Models = nil
		}},
		{"提供方并发为负", func(m *ModelAPIConfig) { m.Providers["a"].MaxConcurrency = -1 }},
		{"提供方 RPM 为负", func(m *ModelAPIConfig) { m.Providers["a"].RPMLimit = -1 }},
		{"模型并发为负", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].MaxConcurrency = -1 }},
		{"模型 RPM 为负", func(m *ModelAPIConfig) { m.Providers["a"].Models[0].RPMLimit = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := base(tc.mut).Validate(); err == nil {
				t.Error("期望校验失败，但通过了")
			}
		})
	}
}

func TestValidatePriceSource(t *testing.T) {
	// cheapest + 无本地价格 + 在线价格源：应通过校验（原"cheapest 必须有本地价格"已放宽）
	ok := &Cfg{ModelAPI: &ModelAPIConfig{
		Enable:      "on",
		Strategy:    ModelStrategyCheapest,
		PriceSource: &PriceSourceConfig{Provider: PriceSourceLiteLLM, CacheTTL: 3600, Timeout: 5},
		Providers: map[string]*ModelProviderConfig{
			"a": {BaseURL: "https://x/v1", APIKey: "k", Models: []*ModelInfoConfig{{Name: "m1"}}},
		},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("cheapest + 在线价格源（无本地价格）应通过校验: %v", err)
	}

	bad := []struct {
		name string
		ps   *PriceSourceConfig
	}{
		{"provider 非法", &PriceSourceConfig{Provider: "rss"}},
		{"cache_ttl 为负", &PriceSourceConfig{Provider: PriceSourceLiteLLM, CacheTTL: -1}},
		{"timeout 为负", &PriceSourceConfig{Provider: PriceSourceOpenRouter, Timeout: -5}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := &Cfg{ModelAPI: &ModelAPIConfig{
				Enable:      "on",
				Strategy:    ModelStrategyCheapest,
				PriceSource: tc.ps,
				Providers: map[string]*ModelProviderConfig{
					"a": {BaseURL: "https://x/v1", APIKey: "k", Models: []*ModelInfoConfig{{Name: "m1"}}},
				},
			}}
			if err := c.Validate(); err == nil {
				t.Error("期望校验失败，但通过了")
			}
		})
	}

	// provider=off 合法（关闭在线价格）
	off := &Cfg{ModelAPI: &ModelAPIConfig{
		Enable:      "on",
		Strategy:    ModelStrategyCheapest,
		PriceSource: &PriceSourceConfig{Provider: PriceSourceOff},
		Providers: map[string]*ModelProviderConfig{
			"a": {BaseURL: "https://x/v1", APIKey: "k", Models: []*ModelInfoConfig{{Name: "m1"}}},
		},
	}}
	if err := off.Validate(); err != nil {
		t.Fatalf("provider=off 应通过校验: %v", err)
	}
	if got := off.ModelAPI.PriceSource.EffectiveProvider(); got != "" {
		t.Errorf("EffectiveProvider(off) = %q, want 空串", got)
	}
}

func TestQueueCapacity(t *testing.T) {
	var c *Cfg
	if c.QueueCapacity(4096) != 4096 {
		t.Fatal("nil cfg default")
	}
	c = &Cfg{Server: &ServerConfig{ReadBuffer: 128, WriteBuffer: 64, Backpressure: true}}
	if c.QueueCapacity(4096) != 128 || c.WriteQueueCapacity(4096) != 64 || !c.DropOnFull() {
		t.Fatal("server config not applied")
	}
}

// TestParseWaSendRateValid `秒|次数` 的合法形态（含空白容错）。
func TestParseWaSendRateValid(t *testing.T) {
	for _, c := range []struct {
		raw    string
		window time.Duration
		max    int
	}{
		{"60|1", time.Minute, 1},
		{"300|3", 5 * time.Minute, 3},
		{"30|1", 30 * time.Second, 1},
		{"3600|30", time.Hour, 30},
		{" 60 | 7 ", time.Minute, 7},
		{"86400|1", 24 * time.Hour, 1},
		{"1|1000", time.Second, 1000},
		// 前导零是纯数字，与写侧正则 `^[0-9]+$` 同口径：解析成数值而不是报错。
		{"007|7", 7 * time.Second, 7},
	} {
		got, ok := ParseWaSendRate(c.raw)
		if !ok {
			t.Errorf("✘ %q 应解析成功", c.raw)
			continue
		}
		if got.Window != c.window || got.Max != c.max {
			t.Errorf("✘ %q 解析成 window=%s max=%d，期望 window=%s max=%d",
				c.raw, got.Window, got.Max, c.window, c.max)
		}
	}
}

// TestParseWaSendRateInvalid 配错的速率必须被拒，不能静默纠错 ——
// 它会直接改变出网节奏，猜一个值比拒绝更危险。
//
// 正号两项是 2026-10-29 与后台写侧对齐时补的：strconv.Atoi 认 "+1"，而写侧的
// parseStrictInt 只认 `^[0-9]+$`。两侧不一致时 " +1|1 " 会在配置文件里生效、
// 在后台被拒，属于「同一件事两个答案」，所以这里一并钉住。
func TestParseWaSendRateInvalid(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "1", "1|", "|1", "0|60", "60|0", "-1|60", "60|-1",
		"a|b", "1|1|1", "1;1", "86401|1", "1|1001", "0|0",
		"+1|1", "1|+1", "1.5|1", "1|1.0", "0x1|1", "1|1e3",
	} {
		if got, ok := ParseWaSendRate(raw); ok {
			t.Errorf("✘ %q 应被拒，实际解析成 %+v", raw, got)
		}
	}
}

// TestEffectiveSendRatePrecedence send_rate 优先；没有/非法才用缺省 60 秒 1 条。
func TestEffectiveSendRatePrecedence(t *testing.T) {
	var nilCfg *WhatsappSessionsConfig
	if got := nilCfg.EffectiveSendRate(); got.Window != 60*time.Second || got.Max != 1 {
		t.Errorf("nil 配置缺省应为 60 秒 1 条，实得 %+v", got)
	}

	empty := &WhatsappSessionsConfig{}
	if got := empty.EffectiveSendRate(); got.Window != 60*time.Second || got.Max != 1 {
		t.Errorf("空配置缺省应为 60 秒 1 条，实得 %+v", got)
	}

	// 配了合法值就用它（首段单位是秒）。
	cfg := &WhatsappSessionsConfig{SendRate: "300|3"}
	if got := cfg.EffectiveSendRate(); got.Window != 5*time.Minute || got.Max != 3 {
		t.Errorf("send_rate=300|3 应解析成 5 分钟 3 条，实得 %+v", got)
	}

	// 配了非法值回落缺省（不回落到任何旧键：send_max_per_hour 已删除）。
	bad := &WhatsappSessionsConfig{SendRate: "oops"}
	if got := bad.EffectiveSendRate(); got.Window != 60*time.Second || got.Max != 1 {
		t.Errorf("send_rate 非法时应回落缺省 60|1，实得 %+v", got)
	}
}

// TestEffectiveSendMaxPerMinuteCompat 已废弃的薄包装仍返回窗口内条数。
func TestEffectiveSendMaxPerMinuteCompat(t *testing.T) {
	c := &WhatsappSessionsConfig{SendRate: "300|3"}
	if got := c.EffectiveSendMaxPerMinute(); got != 3 {
		t.Errorf("废弃包装应返回条数 3，实得 %d", got)
	}
}

// TestWhatsappSessionsEffectiveDefaults 表驱动钉住 Effective* 的缺省口径：
// 整机没配 whatsapp.sessions 段（nil 接收者）与配了半截（字段 <=0）都必须落到同一组
// 内置缺省，而不是 panic 或按 0 秒跑——0 秒超时会让消息侧每次连接都当场失败。
func TestWhatsappSessionsEffectiveDefaults(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *WhatsappSessionsConfig
		timeout time.Duration
		min     time.Duration
		max     time.Duration
		maxConc int
	}{
		{"nil 接收者", nil, 15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"零值", &WhatsappSessionsConfig{}, 15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"负值按缺省", &WhatsappSessionsConfig{ConnectTimeoutSec: -1, ReconnectMinSec: -3, ReconnectMaxSec: -9, MaxConcurrent: -2},
			15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"显式取值", &WhatsappSessionsConfig{ConnectTimeoutSec: 5, ReconnectMinSec: 2, ReconnectMaxSec: 20, MaxConcurrent: 8},
			5 * time.Second, 2 * time.Second, 20 * time.Second, 8},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			if got := cs.cfg.EffectiveConnectTimeout(); got != cs.timeout {
				t.Errorf("EffectiveConnectTimeout=%s，期望 %s", got, cs.timeout)
			}
			if got := cs.cfg.EffectiveReconnectMin(); got != cs.min {
				t.Errorf("EffectiveReconnectMin=%s，期望 %s", got, cs.min)
			}
			if got := cs.cfg.EffectiveReconnectMax(); got != cs.max {
				t.Errorf("EffectiveReconnectMax=%s，期望 %s", got, cs.max)
			}
			if got := cs.cfg.EffectiveMaxConcurrent(); got != cs.maxConc {
				t.Errorf("EffectiveMaxConcurrent=%d，期望 %d", got, cs.maxConc)
			}
		})
	}
}

// TestWhatsappSessionsMinOverMaxClamps 下界压过上界时按上界取：
// 配出 min>max 若不夹取，退避序列会从下界起跳、永远越过上界，重连节奏与配置意图相反。
func TestWhatsappSessionsMinOverMaxClamps(t *testing.T) {
	cfg := &WhatsappSessionsConfig{ReconnectMinSec: 30, ReconnectMaxSec: 10}
	if got := cfg.EffectiveReconnectMin(); got != 10*time.Second {
		t.Errorf("min>max 未夹取到上界: %s", got)
	}
	if got := cfg.EffectiveReconnectMax(); got != 10*time.Second {
		t.Errorf("max=%s", got)
	}
}

// TestEffectiveAntiban 防封闸三项的缺省折算：档位空串→moderate 原样透传（兜底在库
// Resolve 侧）、状态目录空→antiban_state、nil 接收者不 panic。
func TestEffectiveAntiban(t *testing.T) {
	var nilCfg *WhatsappSessionsConfig
	if got := nilCfg.EffectiveAntibanPreset(); got != "moderate" {
		t.Errorf("nil 档位=%q", got)
	}
	if got := nilCfg.EffectiveAntibanStateDir(); got != "antiban_state" {
		t.Errorf("nil 目录=%q", got)
	}
	c := &WhatsappSessionsConfig{AntibanPreset: "strict", AntibanStateDir: `D:\data\ab`}
	if got := c.EffectiveAntibanPreset(); got != "strict" {
		t.Errorf("档位=%q", got)
	}
	if got := c.EffectiveAntibanStateDir(); got != `D:\data\ab` {
		t.Errorf("目录=%q", got)
	}
	// 三态开关：段在而键缺省 → 开；显式 false → 关；整段 nil → 关。
	if !c.EffectiveAntibanEnabled() {
		t.Error("段在而键缺省该读作开")
	}
	off := false
	if (&WhatsappSessionsConfig{AntibanEnabled: &off}).EffectiveAntibanEnabled() {
		t.Error("显式 false 该读作关")
	}
	if nilCfg.EffectiveAntibanEnabled() {
		t.Error("整段缺失该读作关")
	}
}

// TestWhatsappConfigEnabled 通道总闸读数：显式 "on"（大小写与首尾空白不敏感）才算开，
// 段缺失/键缺失/off/其它值一律关。口径与 ChannelSupplierConfig.Enabled 一致——whatsmeow 是非官方
// 协议客户端，「没显式开即不连 WhatsApp 服务器」必须是缺省。
func TestWhatsappConfigEnabled(t *testing.T) {
	for _, v := range []string{"on", "ON", "On", " on ", "\ton\n"} {
		if !(&WhatsappConfig{Enable: v}).Enabled() {
			t.Errorf("enable=%q 该读作开", v)
		}
	}
	for _, v := range []string{"", "  ", "off", "true", "1", "yes"} {
		if (&WhatsappConfig{Enable: v}).Enabled() {
			t.Errorf("enable=%q 该读作关", v)
		}
	}
	var nilCfg *WhatsappConfig
	if nilCfg.Enabled() {
		t.Error("整段缺失该读作关")
	}
}

// TestZaloEnabled 总闸口径与 whatsapp/ChannelSupplierConfig 一致：只有显式 on（大小写不敏感）才算开，
// nil 接收者安全，缺省不开。
func TestZaloEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ZaloConfig
		want bool
	}{
		{"nil", nil, false},
		{"空段", &ZaloConfig{}, false},
		{"off", &ZaloConfig{Enable: "off"}, false},
		{"ON 大写", &ZaloConfig{Enable: "ON"}, true},
		{"带空格", &ZaloConfig{Enable: " on "}, true},
	}
	for _, cs := range cases {
		if got := cs.cfg.Enabled(); got != cs.want {
			t.Errorf("%s: Enabled()=%v 期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestZaloEffectiveDefaults nil 与空段都必须给出截图口径的保守缺省。
func TestZaloEffectiveDefaults(t *testing.T) {
	for _, c := range []*ZaloSessionsConfig{nil, {}} {
		if got := c.EffectiveQRTTL(); got != 5*time.Minute {
			t.Errorf("QRTTL 缺省=%v", got)
		}
		if got := c.EffectiveRequestTimeout(); got != 30*time.Second {
			t.Errorf("RequestTimeout 缺省=%v", got)
		}
		if got := c.EffectiveFriendsCacheTTL(); got != 1*time.Minute {
			t.Errorf("FriendsCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveGroupsCacheTTL(); got != 1*time.Minute {
			t.Errorf("GroupsCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveInfoMinInterval(); got != 1500*time.Millisecond {
			t.Errorf("InfoMinInterval 缺省=%v", got)
		}
		if got := c.EffectiveInfoCacheTTL(); got != 10*time.Minute {
			t.Errorf("InfoCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveSendMinDelay(); got != 3*time.Second {
			t.Errorf("SendMinDelay 缺省=%v", got)
		}
		if got := c.EffectiveSendMaxDelay(); got != 5*time.Second {
			t.Errorf("SendMaxDelay 缺省=%v", got)
		}
		if got := c.EffectiveSendMaxPerHour(); got != 30 {
			t.Errorf("SendMaxPerHour 缺省=%d", got)
		}
	}
}

// TestZaloEffectiveClamps 低于截图下限的值必须被抬到下限；send min/max 交叉时按 max 取。
func TestZaloEffectiveClamps(t *testing.T) {
	c := &ZaloSessionsConfig{
		QRTTLSec:           10,
		RequestTimeoutSec:  1,
		FriendsCacheTTLSec: 10,
		GroupsCacheTTLSec:  10,
		InfoMinIntervalMs:  100,
		InfoCacheTTLSec:    1,
		SendMinDelayMs:     100,
		SendMaxDelayMs:     1000,
		SendMaxPerHour:     -1,
	}
	if got := c.EffectiveQRTTL(); got != 2*time.Minute {
		t.Errorf("QRTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveRequestTimeout(); got != 5*time.Second {
		t.Errorf("RequestTimeout 下限钳制=%v", got)
	}
	if got := c.EffectiveFriendsCacheTTL(); got != 30*time.Second {
		t.Errorf("FriendsCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveGroupsCacheTTL(); got != 30*time.Second {
		t.Errorf("GroupsCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveInfoMinInterval(); got != 1500*time.Millisecond {
		t.Errorf("InfoMinInterval 下限钳制=%v", got)
	}
	if got := c.EffectiveInfoCacheTTL(); got != 10*time.Minute {
		t.Errorf("InfoCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveSendMinDelay(); got != 1*time.Second {
		t.Errorf("SendMinDelay 下限钳制=%v", got)
	}
	if got := c.EffectiveSendMaxDelay(); got != 1*time.Second {
		t.Errorf("SendMaxDelay 低于 min 时按 min 取=%v", got)
	}
	if got := c.EffectiveSendMaxPerHour(); got != 30 {
		t.Errorf("SendMaxPerHour 非正值应折算 30，实得 %d", got)
	}

	big := &ZaloSessionsConfig{SendMinDelayMs: 9000, SendMaxDelayMs: 5000}
	if got := big.EffectiveSendMinDelay(); got != 5*time.Second {
		t.Errorf("min>max 时应按 max 取，实得 %v", got)
	}
}

// TestZaloListenerDefault 指针区分「缺省开」与「显式关」；整段 nil 读作关。
func TestZaloListenerDefault(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		cfg  *ZaloSessionsConfig
		want bool
	}{
		{"段缺失", nil, false},
		{"键缺省", &ZaloSessionsConfig{}, true},
		{"显式关", &ZaloSessionsConfig{ListenerEnabled: &off}, false},
		{"显式开", &ZaloSessionsConfig{ListenerEnabled: &on}, true},
	}
	for _, cs := range cases {
		if got := cs.cfg.EffectiveListenerEnabled(); got != cs.want {
			t.Errorf("%s: listener=%v 期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestZaloUserAgentLanguageTrim 只 trim，留空保持空串交给 zcago 默认。
func TestZaloUserAgentLanguageTrim(t *testing.T) {
	c := &ZaloSessionsConfig{UserAgent: "  UA  ", Language: " vi "}
	if got := c.EffectiveUserAgent(); got != "UA" {
		t.Errorf("UA=%q", got)
	}
	if got := c.EffectiveLanguage(); got != "vi" {
		t.Errorf("Language=%q", got)
	}
	var nilCfg *ZaloSessionsConfig
	if nilCfg.EffectiveUserAgent() != "" || nilCfg.EffectiveLanguage() != "" {
		t.Error("nil 配置应返回空串")
	}
}

// TestZaloLegacyConfigRejected 旧 zalo.login/templates 必须在启动期报错，
// 不能被 encoding/json 静默丢弃。
//
// login 的类型是 json.RawMessage（旧供应商 SDK 表已整张删除，这里只需要「这个键
// 出现过没有」），所以字面量直接给原始 JSON 字节；`{}` 这种空对象也算写了——
// 它同样说明部署还在按旧形态配置。
func TestZaloLegacyConfigRejected(t *testing.T) {
	cases := []*ZaloConfig{
		{Login: json.RawMessage(`{"sdk":"supplier_zalo"}`)},
		{Login: json.RawMessage(`{}`)},
		{Templates: map[string]string{"auth_verify": "tpl"}},
	}
	for _, zc := range cases {
		cfg := &Cfg{Zalo: zc}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("旧配置 %+v 未被拦下", zc)
		}
		if !strings.Contains(err.Error(), "已废弃") {
			t.Errorf("报错应指明废弃迁移：%v", err)
		}
	}

	// 新配置（即使 sessions 为空）不该被墓碑检查拦下。
	if err := (&Cfg{Zalo: &ZaloConfig{Enable: "off", Sessions: &ZaloSessionsConfig{}}}).Validate(); err != nil {
		t.Errorf("新配置不应被拦: %v", err)
	}
}

// TestExampleZaloHasNoLegacyKeys 示例的 zalo 段只允许 enable + sessions；
// 旧 login/templates 是墓碑字段，不得出现在示例里（否则会被当成可照抄的配置）。
func TestExampleZaloHasNoLegacyKeys(t *testing.T) {
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var doc struct {
		Zalo map[string]json.RawMessage `json:"zalo"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if doc.Zalo == nil {
		t.Fatal(`example.json 缺少 "zalo" 配置块`)
	}
	if _, ok := doc.Zalo["sessions"]; !ok {
		t.Fatal(`example.json 的 zalo 段缺少 sessions`)
	}
	for _, legacy := range []string{"login", "templates"} {
		if _, ok := doc.Zalo[legacy]; ok {
			t.Errorf("example.json 的 zalo 段仍含已废弃键 %q", legacy)
		}
	}
}
