package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
)

// cloudTestCfg 组一份「Cloud 段引用齐备」的配置，BaseURL 指向假 Graph server。
func cloudTestCfg(t *testing.T, baseURL string) *config.Cfg {
	t.Helper()
	return &config.Cfg{Whatsapp: &config.WhatsappConfig{Cloud: &config.WhatsappCloudConfig{
		AccessToken:   "tok",
		PhoneNumberID: "PNID",
		BaseURL:       baseURL,
		APIVersion:    "vTest",
	}}}
}

// newFakeGraph 记录收到的请求体并返回成功回执。同包其他测试文件复用此函数。
func newFakeGraph(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.x"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestNewCloudClientHasTimeout 钉住出站兜底超时：实现曾复用无超时的 http.DefaultClient，
// 调用方 ctx 不带 deadline 时会无限挂住一个进程级共享 client。
func TestNewCloudClientHasTimeout(t *testing.T) {
	c := newCloudClient(&config.WhatsappCloudConfig{AccessToken: "tok", PhoneNumberID: "PNID"})
	if c.httpClient.Timeout != cloudSendTimeout {
		t.Fatalf("出站 client 兜底超时=%v，期望 %v", c.httpClient.Timeout, cloudSendTimeout)
	}
}

func TestSendText(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("缺 Authorization: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messaging_product":"whatsapp","messages":[{"id":"wamid.1"}]}`)
	}))
	t.Cleanup(srv.Close)

	startWith(t, cloudTestCfg(t, srv.URL))
	id, err := WhatsappCloudSendText(context.Background(), "86138", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if id != "wamid.1" {
		t.Fatalf("返回 ID=%s", id)
	}
	if !strings.Contains(gotBody, `"to":"86138"`) || !strings.Contains(gotBody, `"body":"hello"`) {
		t.Fatalf("报文不符: %s", gotBody)
	}
}

func TestSendTemplate(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"messages":[{"id":"wamid.2"}]}`)
	}))
	t.Cleanup(srv.Close)

	startWith(t, cloudTestCfg(t, srv.URL))
	if _, err := WhatsappCloudSendTemplate(context.Background(), "86138", "bind_otp", "en_US", "123456", "5"); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "template" {
		t.Fatalf("type 错误: %v", got["type"])
	}
	tpl := got["template"].(map[string]any)
	if tpl["name"] != "bind_otp" {
		t.Fatalf("模板名错误: %v", tpl["name"])
	}
}

func TestSendInteractiveButtons(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"messages":[{"id":"wamid.3"}]}`)
	}))
	t.Cleanup(srv.Close)
	startWith(t, cloudTestCfg(t, srv.URL))

	btns := []CloudButton{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}
	if _, err := WhatsappCloudSendInteractiveButtons(context.Background(), "86138", "choose", btns); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "interactive" {
		t.Fatalf("type 错误: %v", got["type"])
	}
}

func TestSendInteractiveTooManyButtons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"messages":[{"id":"x"}]}`)
	}))
	t.Cleanup(srv.Close)

	startWith(t, cloudTestCfg(t, srv.URL))
	btns := []CloudButton{{ID: "1", Title: "1"}, {ID: "2", Title: "2"}, {ID: "3", Title: "3"}, {ID: "4", Title: "4"}}
	if _, err := WhatsappCloudSendInteractiveButtons(context.Background(), "86138", "x", btns); err == nil {
		t.Fatal("超过 3 个按钮应被拒绝")
	}
}

func TestSendAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"Invalid parameter","code":131009}}`)
	}))
	t.Cleanup(srv.Close)

	startWith(t, cloudTestCfg(t, srv.URL))
	_, err := WhatsappCloudSendText(context.Background(), "86138", "hi")
	if err == nil {
		t.Fatal("应当报错")
	}
	var ae *CloudAPIError
	if !errors.As(err, &ae) || ae.Code != 131009 {
		t.Fatalf("错误类型/码不符: %v", err)
	}
}

// TestSendEmptyMessagesContract 钉下游 wa.Provider 依赖的契约：回执 messages 为空必须
// 「空 id + 非 nil error」成对出现，不能被抹平成 id="" 且 err=nil 的伪成功。
func TestSendEmptyMessagesContract(t *testing.T) {
	hits := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		io.WriteString(w, `{"messaging_product":"whatsapp","messages":[]}`)
	}))
	t.Cleanup(srv.Close)

	startWith(t, cloudTestCfg(t, srv.URL))
	id, err := WhatsappCloudSendTemplate(context.Background(), "86138", "bind_otp", "en_US", "123456", "5")
	// 没有这次请求，后面两条断言会因为「未启动」而空跑，故先钉请求真的打出去。
	if n := len(hits); n != 1 {
		t.Fatalf("空回执路径应打到 Graph server 1 次，实得 %d 次", n)
	}
	if err == nil {
		t.Fatal("空 messages 回执必须报错，不能当发送成功")
	}
	if id != "" {
		t.Fatalf("报错时消息 ID 必须为空，实得 %q", id)
	}
}

func TestSendMissingConfig(t *testing.T) {
	// 未配置 Cloud 段：WhatsappCloud 应为 nil，门面返回明确错误而不是 panic。
	WhatsappStop(nil)
	WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	if c := WhatsappCloud(); c != nil {
		t.Fatalf("未配置 Cloud 时 WhatsappCloud 应为 nil，实得 %+v", c)
	}
	if _, err := WhatsappCloudSendText(context.Background(), "86138", "hi"); err == nil {
		t.Fatal("未启动应报错")
	}
}
