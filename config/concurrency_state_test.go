package config

// 本文件钉的是 L9「config 包级全局变量数据竞争」：_confDir / _defaultFile / _basePath /
// _configDirFlag / *_defServerId / _confDirField / _featureDir / _featureDirSet 这些包级
// 可变状态过去没有任何同步保护，RegisterFunc 与 LoadWithError 并发调用时既有 string
// 也有 bool 的数据竞争。
//
// 本机没有 C 工具链，go test -race 跑不了，因此这里用两条互补的证据代替 race 检测：
//
//  1. 互斥覆盖论证（静态、机器可查）：TestPackageStateAccessIsGuarded 用 go/ast 扫本包
//     全部非测试源文件，凡引用受保护全局的函数，必须自己持 _stateMu 或调用某个
//     已持锁的函数（不动点推导），否则直接失败。这条把「所有读写点统一走保护路径」
//     从口头约定变成会红的断言，将来新增读写点漏加锁就会被拦下。
//  2. 压力轮次（动态）：多协程并发 RegisterFunc + LoadWithError + 各路径 getter，
//     断言不死锁（看门狗 + 全协程栈转储）、不 panic（每协程 recover）、
//     最终状态一致（注册过的功能配置全部加载、路径三元组成组一致）。
//
// 两条都不能替代 race detector；交付说明里必须保留「未跑 -race」这一条。

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
