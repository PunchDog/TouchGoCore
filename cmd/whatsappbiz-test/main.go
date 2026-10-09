// Command whatsappbiz-test 是 WhatsApp Business Cloud API 的测试脚手架（Go + Gin）。
//
// 覆盖四个测试模块：WhatsApp 绑定、分享、客服与帮助、引导流程。
// 配合 Meta 测试号 + ngrok 公开 Webhook 使用；所有凭证通过环境变量注入，不写进代码。
//
// 分层口径：本文件扮演「业务方」。绑定态（验证码、绑定标记）与引导进度都存在这里的进程内存
// map 中，框架包 whatsapp/ 不承载它们——框架只发信、只解协议。生产业务方必须把这份状态换成
// 持久化 + 不可反推明文的摘要存储，并按发码防护加冷却与预算闸；本演示为了能在单机点通链路，
// 既把验证码明文存内存、又把它回显在响应里，两者都不可用于生产。
//
// 运行：
//
//	export WA_ACCESS_TOKEN=...      # 系统用户永久 token 或 24h 临时 token
//	export WA_PHONE_NUMBER_ID=...   # 测试号 Phone Number ID
//	export WA_WABA_ID=...           # WhatsApp Business Account ID
//	export WA_VERIFY_TOKEN=myverify # Webhook 验证挑战串（自定义）
//	export WA_APP_SECRET=...        # 用于 webhook 签名校验
//	go run ./cmd/whatsappbiz-test -addr :8080
//
// 路由：
//
//	GET  /                        说明页
//	GET  /webhook                 Webhook 验证挑战
//	POST /webhook                 接收消息（签名校验 + 解析 + 分发到客服/引导）
//	POST /test/bind               绑定：{"phone":"...","template":""} → 下发验证码（回显便于测试）
//	POST /test/bind/verify        校验：{"phone":"...","code":"..."} → 完成绑定
//	GET  /test/bind/status?phone=...   查询绑定状态
//	GET  /test/share?text=...&phone=...  生成 wa.me 分享链接
//	POST /test/onboarding         引导：{"phone":"..."} → 发送第一步（交互式按钮）
//	POST /test/support            客服：{"phone":"...","message":"..."} → 窗口内主动回复
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/vars"
	"touchgocore/whatsapp"

	"github.com/gin-gonic/gin"
)

// demoCode 是一条待校验的验证码及其过期时刻。
type demoCode struct {
	code      string
	expiresAt time.Time
}

// demoStore 是演示用的进程内存业务态：验证码、绑定标记、引导进度。
// 真源应落在业务库（如 whatsapp_bind 表 + 码摘要），这里只是可跑的样例。
type demoStore struct {
	mu    sync.Mutex
	codes map[string]demoCode
	bound map[string]bool
	steps map[string]int
}

func newDemoStore() *demoStore {
	return &demoStore{
		codes: map[string]demoCode{},
		bound: map[string]bool{},
		steps: map[string]int{},
	}
}

func (s *demoStore) saveCode(phone, code string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[phone] = demoCode{code: code, expiresAt: time.Now().Add(ttl)}
}

// loadCode 取未过期的验证码；过期即删。
func (s *demoStore) loadCode(phone string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.codes[phone]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		delete(s.codes, phone)
		return "", false
	}
	return e.code, true
}

// markBound 标记已绑定，并让旧码失效。
func (s *demoStore) markBound(phone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound[phone] = true
	delete(s.codes, phone)
}

func (s *demoStore) isBound(phone string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound[phone]
}

func (s *demoStore) getStep(phone string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps[phone]
}

func (s *demoStore) setStep(phone string, step int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps[phone] = step
}

// randDigits 生成 n 位数字码；crypto/rand 失败直接报错，绝不回落到可猜的固定码。
func randDigits(n int) (string, error) {
	const digits = "0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成验证码失败: %w", err)
	}
	for i := range buf {
		buf[i] = digits[int(buf[i])%10]
	}
	return string(buf), nil
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	flag.Parse()

	wcfg := &config.WhatsappCloudConfig{
		AccessToken:   os.Getenv("WA_ACCESS_TOKEN"),
		PhoneNumberID: os.Getenv("WA_PHONE_NUMBER_ID"),
		WABAID:        os.Getenv("WA_WABA_ID"),
		VerifyToken:   os.Getenv("WA_VERIFY_TOKEN"),
		AppSecret:     os.Getenv("WA_APP_SECRET"),
	}
	cfg := &config.Cfg{Whatsapp: &config.WhatsappConfig{Cloud: wcfg}}
	whatsapp.WhatsappStart(corectx.WithCfg(context.Background(), cfg))
	if whatsapp.WhatsappCloud() == nil {
		vars.Warning("缺少 WA_ACCESS_TOKEN / WA_PHONE_NUMBER_ID：Cloud 快照未建立，发送类与 Webhook 验证/验签都会返回「未启动」（验签件经快照读 verify_token / app_secret），只有分享与窗口判定这类纯函数仍可用")
	}

	store := newDemoStore()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.GET("/", func(g *gin.Context) {
		g.Data(http.StatusOK, "text/html; charset=utf-8", []byte(indexHTML))
	})

	// Webhook：GET 验证挑战，POST 接收消息
	r.GET("/webhook", func(g *gin.Context) {
		chal, err := whatsapp.WhatsappCloudVerifyWebhook(g.Query("hub.mode"), g.Query("hub.verify_token"), g.Query("hub.challenge"))
		if err != nil {
			g.String(http.StatusForbidden, "forbidden")
			return
		}
		g.String(http.StatusOK, chal)
	})
	r.POST("/webhook", func(g *gin.Context) {
		body, err := g.GetRawData()
		if err != nil {
			g.String(http.StatusBadRequest, "read body failed")
			return
		}
		if err := whatsapp.WhatsappCloudVerifyWebhookSignature(body, g.GetHeader("X-Hub-Signature-256")); err != nil {
			vars.Error("webhook 验签失败: %v", err)
			g.String(http.StatusForbidden, "bad signature")
			return
		}
		env, err := whatsapp.WhatsappCloudParseWebhook(body)
		if err != nil {
			g.String(http.StatusBadRequest, "bad payload")
			return
		}
		ctx := g.Request.Context()
		total := len(wcfg.OnboardSteps())
		for _, m := range whatsapp.WhatsappCloudCollectMessages(env) {
			if m.Type == "interactive" && m.Interactive != nil && m.Interactive.ButtonReply != nil {
				// 进度归属业务方：按钮 id 的分支语义与「走到第几步」都在这里判定。
				phone := whatsapp.WhatsappCloudNormalizePhone(m.From)
				next := store.getStep(phone) + 1
				if next >= total {
					store.setStep(phone, total)
					vars.Info("引导完成 from=%s button=%s", phone, m.Interactive.ButtonReply.ID)
					continue
				}
				store.setStep(phone, next)
				if _, e := whatsapp.WhatsappCloudSendOnboardingStep(ctx, phone, next); e != nil {
					vars.Error("引导第 %d 步发送失败: %v", next, e)
				}
			} else if m.Text != nil {
				vars.Info("客服收消息 from=%s body=%s", m.From, m.Text.Body)
				ts, _ := strconv.ParseInt(m.Timestamp, 10, 64)
				within := whatsapp.WhatsappCloudWithinWindow(ts, time.Now())
				if _, e := whatsapp.WhatsappCloudSupportReply(m.From, "已收到您的消息，客服将尽快处理。", within); e != nil {
					vars.Warning("客服自动回复失败（可能超出窗口）: %v", e)
				}
			}
		}
		for _, st := range whatsapp.WhatsappCloudCollectStatuses(env) {
			vars.Debug("消息状态 from=%s id=%s status=%s", st.RecipientID, st.ID, st.Status)
		}
		g.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 绑定模块
	r.POST("/test/bind", func(g *gin.Context) {
		var req struct {
			Phone    string `json:"phone"`
			Template string `json:"template"`
		}
		if err := g.ShouldBindJSON(&req); err != nil {
			g.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		phone := whatsapp.WhatsappCloudNormalizePhone(req.Phone)
		if phone == "" {
			g.JSON(http.StatusBadRequest, gin.H{"error": "手机号为空"})
			return
		}
		code, err := randDigits(6)
		if err != nil {
			g.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ttl := wcfg.CodeTTL()
		store.saveCode(phone, code, ttl)
		template := req.Template
		if template == "" {
			template = wcfg.BindTemplateName()
		}
		minutes := fmt.Sprintf("%d", int(ttl/time.Minute))
		if _, err := whatsapp.WhatsappCloudSendTemplate(g.Request.Context(), phone, template, "en_US", code, minutes); err != nil {
			g.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// 演示专属：回显验证码以便单机点通链路。生产既不能回显，也不能存明文。
		g.JSON(http.StatusOK, gin.H{"phone": phone, "code": code, "sent": true})
	})
	r.POST("/test/bind/verify", func(g *gin.Context) {
		var req struct {
			Phone string `json:"phone"`
			Code  string `json:"code"`
		}
		if err := g.ShouldBindJSON(&req); err != nil {
			g.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		phone := whatsapp.WhatsappCloudNormalizePhone(req.Phone)
		want, ok := store.loadCode(phone)
		if phone == "" || req.Code == "" || !ok || want != req.Code {
			g.JSON(http.StatusBadRequest, gin.H{"error": "验证码不正确或已过期"})
			return
		}
		store.markBound(phone)
		g.JSON(http.StatusOK, gin.H{"bound": true})
	})
	r.GET("/test/bind/status", func(g *gin.Context) {
		phone := whatsapp.WhatsappCloudNormalizePhone(g.Query("phone"))
		g.JSON(http.StatusOK, gin.H{"phone": phone, "bound": store.isBound(phone)})
	})

	// 分享模块（纯客户端逻辑，服务端仅生成链接）
	r.GET("/test/share", func(g *gin.Context) {
		text := g.Query("text")
		phone := g.Query("phone")
		var link string
		if phone != "" {
			link = whatsapp.WhatsappCloudShareTo(phone, text)
		} else {
			link = whatsapp.WhatsappCloudShareText(text)
		}
		g.JSON(http.StatusOK, gin.H{"link": link})
	})

	// 引导模块（业务方持有进度，框架只按步号渲染发送）
	r.POST("/test/onboarding", func(g *gin.Context) {
		var req struct {
			Phone string `json:"phone"`
		}
		if err := g.ShouldBindJSON(&req); err != nil {
			g.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		phone := whatsapp.WhatsappCloudNormalizePhone(req.Phone)
		if phone == "" {
			g.JSON(http.StatusBadRequest, gin.H{"error": "手机号为空"})
			return
		}
		if _, err := whatsapp.WhatsappCloudSendOnboardingStep(g.Request.Context(), phone, 0); err != nil {
			g.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		store.setStep(phone, 0)
		g.JSON(http.StatusOK, gin.H{"started": true, "phone": phone, "steps": len(wcfg.OnboardSteps())})
	})

	// 客服模块（演示：窗口内主动发一条）
	r.POST("/test/support", func(g *gin.Context) {
		var req struct {
			Phone   string `json:"phone"`
			Message string `json:"message"`
		}
		if err := g.ShouldBindJSON(&req); err != nil {
			g.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := whatsapp.WhatsappCloudSupportReply(req.Phone, req.Message, true)
		if err != nil {
			g.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		g.JSON(http.StatusOK, gin.H{"message_id": id})
	})

	vars.Info("WhatsApp 测试脚手架监听 %s", *addr)
	if err := r.Run(*addr); err != nil {
		vars.Error("服务退出: %v", err)
		os.Exit(1)
	}
}

const indexHTML = `<!doctype html><html><head><meta charset="utf-8"><title>WhatsApp 测试脚手架</title></head>
<body>
<h1>WhatsApp Business Cloud API 测试脚手架</h1>
<p>覆盖四个模块：绑定 / 分享 / 客服 / 引导。绑定态与引导进度存在本进程内存里（演示用，生产归业务库）。</p>
<ul>
<li>POST /test/bind {"phone":"86138"} — 下发绑定验证码（模板）</li>
<li>POST /test/bind/verify {"phone":"86138","code":"123456"} — 校验完成绑定</li>
<li>GET /test/bind/status?phone=86138 — 查询绑定状态</li>
<li>GET /test/share?text=hello&amp;phone=86138 — 生成 wa.me 分享链接</li>
<li>POST /test/onboarding {"phone":"86138"} — 发送引导第一步（交互式按钮）</li>
<li>POST /test/support {"phone":"86138","message":"hi"} — 客服窗口内主动回复</li>
<li>GET/POST /webhook — Webhook 验证与消息接收</li>
</ul>
</body></html>`
