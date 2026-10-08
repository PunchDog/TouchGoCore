package nft

// T7 包级行为闸第二件：注册表入参面（§4 补强，既有文件只测拒覆盖与名单）、
// 脱敏面全表面扫描（§0-6/PRD §7-4：secret/key/token/mnemonic/metadata 模式零命中，
// 含 ProviderError.Error()、五动作错误文案、RawNote、未配置端点拒绝路径）、
// 状态词表边界（§3.1 词表的既有用例未覆盖项）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// qaDelRegistry 在测试前后清掉包级 registry 的探针名，保证 -count≥2 可重复。
func qaDelRegistry(name string) {
	registryMu.Lock()
	delete(registry, name)
	registryMu.Unlock()
}

// ---- 闸 4（补强）：注册表入参校验面 ----

func TestQaRegistryInputValidation(t *testing.T) {
	okFactory := func(_ ProviderOptions) (NftChannel, error) { return &stubChannel{tag: "qa-ok"}, nil }

	cases := []struct {
		name    string
		probe   string // 需要预登记的驱动名，空表示无需
		action  func() error
		wantSub string
	}{
		{
			name:    "Register 空名拒绝",
			action:  func() error { return Register("", okFactory) },
			wantSub: "驱动名不得为空",
		},
		{
			name:    "Register 纯空白名拒绝",
			action:  func() error { return Register("  \t ", okFactory) },
			wantSub: "驱动名不得为空",
		},
		{
			name:    "Register nil 构造器拒绝",
			action:  func() error { return Register("qa_nil_probe", nil) },
			wantSub: "构造函数为 nil",
		},
		{
			name:    "Open 空名拒绝",
			action:  func() error { _, err := Open("", ProviderOptions{}); return err },
			wantSub: "未指定驱动名",
		},
		{
			name:    "Open 纯空白名拒绝",
			action:  func() error { _, err := Open("   ", ProviderOptions{}); return err },
			wantSub: "未指定驱动名",
		},
		{
			name:  "Factory 报错被包装且可 errors.Is",
			probe: "qa_err_probe",
			action: func() error {
				sentinel := errors.New("qa-factory-sentinel")
				qaDelRegistry("qa_err_probe")
				if err := Register("qa_err_probe", func(_ ProviderOptions) (NftChannel, error) {
					return nil, sentinel
				}); err != nil {
					return err
				}
				_, err := Open("qa_err_probe", ProviderOptions{})
				if !errors.Is(err, sentinel) {
					return errors.New("包装丢失原错误链（未用 %w）")
				}
				return err
			},
			wantSub: "打开驱动[qa_err_probe] 失败",
		},
		{
			name:  "Factory 返回空通道拒绝",
			probe: "qa_nilchan_probe",
			action: func() error {
				qaDelRegistry("qa_nilchan_probe")
				if err := Register("qa_nilchan_probe", func(_ ProviderOptions) (NftChannel, error) {
					return nil, nil
				}); err != nil {
					return err
				}
				_, err := Open("qa_nilchan_probe", ProviderOptions{})
				return err
			},
			wantSub: "返回了空通道",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.probe != "" {
				t.Cleanup(func() { qaDelRegistry(tc.probe) })
			}
			err := tc.action()
			if err == nil {
				t.Fatalf("%s 应返回错误", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错应含 %q，实得 %v", tc.wantSub, err)
			}
		})
	}
}

func TestQaOpenBackfillsDriverAndDriversSorted(t *testing.T) {
	t.Cleanup(func() { qaDelRegistry("qa_backfill_probe") })
	qaDelRegistry("qa_backfill_probe")

	var gotOpt ProviderOptions
	if err := Register("qa_backfill_probe", func(opt ProviderOptions) (NftChannel, error) {
		gotOpt = opt
		return &stubChannel{tag: "qa-backfill"}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ch, err := Open("qa_backfill_probe", ProviderOptions{SecretKey: "k"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ch.Driver() != "qa-backfill" {
		t.Fatalf("应取到 Factory 产出的通道: %q", ch.Driver())
	}
	if gotOpt.Driver != "qa_backfill_probe" {
		t.Fatalf("opt.Driver 留空应回填注册名，实得 %q", gotOpt.Driver)
	}

	// Drivers() 字典序（§4）。
	for _, n := range []string{"qa_zzz_sort", "qa_aaa_sort", "qa_mmm_sort"} {
		t.Cleanup(func() { qaDelRegistry(n) })
		qaDelRegistry(n)
		if err := Register(n, func(_ ProviderOptions) (NftChannel, error) { return &stubChannel{}, nil }); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	ds := Drivers()
	for i := 1; i < len(ds); i++ {
		if ds[i-1] > ds[i] {
			t.Fatalf("Drivers() 应字典序，实得 %v", ds)
		}
	}
	for _, want := range []string{DriverGeneric, "qa_aaa_sort", "qa_mmm_sort", "qa_zzz_sort"} {
		found := false
		for _, d := range ds {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Drivers() 应含 %q，实得 %v", want, ds)
		}
	}
}

// ---- 闸 5（补强）：脱敏面全表面扫描 ----
// PRD §7-4：错误文案/日志面/RawNote 扫 secret/key/token/code 模式零命中。
// 既有 TestSecretKeyNeverLeaks 只走 Mint 三路径；这里把五个动作 + ProviderError.Error()
// 逐字面 + 未配置端点拒绝 + 传输层错误面全部纳入 canary 扫描。

const (
	qaCanarySecret   = "qaCANARYsecretKx9vQ3"
	qaCanaryToken    = "qaCANARYtokenM3p8Z1"
	qaCanaryMetadata = "ipfs://qaCANARYmetaT7w2"
	qaCanaryMnemonic = "qaCANARYmnemonicR5j4"
	qaCanaryPrivKey  = "qaCANARYprivkeyB8n6"
)

// qaCanaries 是全部禁露凭证 canary。
func qaCanaries() []string {
	return []string{qaCanarySecret, qaCanaryToken, qaCanaryMetadata, qaCanaryMnemonic, qaCanaryPrivKey}
}

func TestQaSecretPatternScanAllSurfaces(t *testing.T) {
	envFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "-1", "msg": "rejected by upstream"})
	}))
	defer envFail.Close()
	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("~~~not-json~~~"))
	}))
	defer badJSON.Close()
	status500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer status500.Close()

	ctx := context.Background()
	// surfaces 收集所有「可能外泄」的文本面：error 文案 + RawNote + ProviderError.Error() 逐字段面。
	var surfaces []string
	collect := func(label string, res *NftResult, err error) {
		if err != nil {
			surfaces = append(surfaces, label+"|err:"+err.Error())
			if pe, ok := ProviderErrorOf(err); ok {
				surfaces = append(surfaces, label+"|peError:"+pe.Error(), label+"|peMsg:"+pe.Msg, label+"|peCode:"+pe.Code)
			}
		}
		if res != nil {
			surfaces = append(surfaces, label+"|RawNote:"+res.RawNote)
		}
	}

	for i, srv := range []*httptest.Server{envFail, badJSON, status500} {
		p := qaProvider(t, srv.URL, qaCanarySecret, 0) // 脱敏扫描不测重试：MaxRetry=0 保持快速确定。
		p.SetToken(qaCanaryToken)
		base := fmt.Sprintf("%s-srv%d", t.Name(), i)

		order := qaMintOrder(base + "-mint")
		order.MetadataURI = qaCanaryMetadata
		mres, merr := p.Mint(ctx, order)
		collect("mint", mres, merr)

		tres, terr := p.Transfer(ctx, qaTransferOrder(base+"-transfer"))
		collect("transfer", tres, terr)

		qres, qerr := p.QueryOrder(ctx, base+"-query")
		collect("query", qres, qerr)

		hres, herr := p.QueryHoldings(ctx, &HoldingsQuery{Address: "0xQaAddr"})
		if hres != nil {
			surfaces = append(surfaces, "holdings|RawNote:"+hres.RawNote)
		}
		if herr != nil {
			surfaces = append(surfaces, "holdings|err:"+herr.Error())
		}

		ierr := func() error {
			_, err := p.QueryToken(ctx, &TokenQuery{Chain: ChainETH, Network: NetworkMainnet, Contract: "0xQaC", TokenID: "1"})
			return err
		}()
		if ierr != nil {
			surfaces = append(surfaces, "token|err:"+ierr.Error())
		}
	}

	// 本地拒绝面（零外呼）：前置校验 / 未配置端点。
	pNo, errNoP := NewProvider(ProviderOptions{
		Name: "nft", Driver: DriverGeneric, SecretKey: qaCanarySecret,
		BaseURL:  "http://127.0.0.1:1",
		Endpoint: func(string) (string, bool) { return "", false },
	})
	if errNoP != nil {
		t.Fatalf("NewProvider: %v", errNoP)
	}
	_, err := pNo.Mint(ctx, qaMintOrder(t.Name()+"-noendpoint"))
	if err == nil || !strings.Contains(err.Error(), "未配置 endpoints") {
		t.Fatalf("未配置端点应就地拒绝且不发请求，实得 %v", err)
	}
	surfaces = append(surfaces, "endpoint-missing|err:"+err.Error())
	if _, err := pNo.Mint(ctx, &NftOrder{OrderNo: "x"}); err == nil {
		t.Fatal("非法订单应前置拒绝")
	} else {
		surfaces = append(surfaces, "check|err:"+err.Error())
	}

	// Extra 值里放助记词/私钥 canary：值进报文（供应商可见）是事实，
	// 但任何本包产出的 error/RawNote 文本面都不得复读它。
	pX := qaProvider(t, envFail.URL, qaCanarySecret, -1)
	xo := qaMintOrder(t.Name() + "-extra")
	xo.Extra = map[string]string{"mnemonic": qaCanaryMnemonic, "priv_key": qaCanaryPrivKey}
	xres, xerr := pX.Mint(ctx, xo)
	collect("extra-mint", xres, xerr)

	// 逐面扫描：canary 明文零命中。
	joined := strings.Join(surfaces, "\n")
	for _, c := range qaCanaries() {
		if strings.Contains(joined, c) {
			t.Fatalf("脱敏面泄露 canary %q，命中面：\n%s", c, joined)
		}
	}

	// 模式扫描（PRD §7-4 的 secret/key/token/code 模式口径）：
	// 错误面不得出现「key= secret= token= mnemonic=」这类键值对形态（供应商 msg 是自由文本，
	// 不含等号赋值）；ProviderError.Error() 的字面格式必须逐字为
	// 「<channel> 通道返回失败 code=<code>: <msg>」，除这三项外不得携带其它字段。
	kvPattern := regexp.MustCompile(`(?i)\b(secret|secret_?key|app_?id|token|mnemonic|priv_?key)\s*[:=]\s*\S`)
	for _, s := range surfaces {
		if loc := kvPattern.FindStringIndex(s); loc != nil {
			t.Fatalf("错误/RawNote 面出现凭证键值对形态 %q：%q", s[loc[0]:loc[1]], s)
		}
	}
	pe := &ProviderError{Channel: "nft", Code: "c1", Msg: "m1", HTTPStatus: 502}
	if got, want := pe.Error(), "nft 通道返回失败 code=c1: m1"; got != want {
		t.Fatalf("ProviderError.Error() 字面格式漂移：got %q want %q", got, want)
	}
	peNilCh := &ProviderError{Code: "c1", Msg: "m1"}
	if got, want := peNilCh.Error(), "通道返回失败 code=c1: m1"; got != want {
		t.Fatalf("ProviderError.Error() 无名分支格式漂移：got %q want %q", got, want)
	}
	// 构造面：HTTPStatus/Retryable 不得进文案（文案只含通道名/错误码/消息，§2.6）。
	if strings.Contains(pe.Error(), "Retryable") {
		t.Fatalf("Error() 不得复读内部字段: %q", pe.Error())
	}
}

// ---- 状态词表边界（§3.1，与既有 8 词用例互补）----

func TestQaStatusVocabularyBoundary(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"succeeded", StatusSuccess},
		{"COMPLETE", StatusSuccess},
		{" success ", StatusSuccess}, // TrimSpace
		{"ok", StatusSuccess},
		{"error", StatusFailed},
		{"REJECT", StatusFailed},
		{"cancelled", StatusFailed},
		{"processing", StatusPending},
		{"Paying", StatusPending},
		{"waiting", StatusPending},
		{"-1", StatusFailed},
		{"1", StatusSuccess},
		{"0", StatusPending},
		{"queued-for-review", StatusUnknown},
		{"3", StatusUnknown}, // 未登记数字绝不落进 pending/success
		{"null", StatusUnknown},
		{"\t ", StatusUnknown},
	}
	for _, tc := range cases {
		if got := NormalizeStatus(tc.raw); got != tc.want {
			t.Errorf("NormalizeStatus(%q)=%q, 期望 %q", tc.raw, got, tc.want)
		}
	}
	// 四态封闭集：任何输入只能落在四个常量之一。
	allowed := map[string]bool{StatusPending: true, StatusSuccess: true, StatusFailed: true, StatusUnknown: true}
	for _, tc := range append(cases, struct{ raw, want string }{"随便中文词", StatusUnknown}) {
		if !allowed[NormalizeStatus(tc.raw)] {
			t.Fatalf("归一结果跳出四态封闭集: %q", NormalizeStatus(tc.raw))
		}
	}
}
