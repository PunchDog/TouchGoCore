package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"touchgocore/pay"
)

// LoginResult 是登录成功后的会话信息。
//
// 这里刻意不回传 token：它由 Provider 内部持有并只写进请求头，
// 一旦返回给调用方就多了一条会进日志、进缓存、被转手的路径。
type LoginResult struct {
	UserID string
	Phone  string
}

// WhatsappSendCode 向手机号下发登录验证码。
//
// 验证码明文只在供应商报文里出现，本包不落内存、不写日志、不返回给调用方。
func WhatsappSendCode(ctx context.Context, phone string) error {
	p, err := currentLogin()
	if err != nil {
		return err
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errors.New("whatsapp 下发验证码失败: 手机号为空")
	}
	cfg := whatsappCfg()
	req := &sendCodeRequest{AppID: p.AppID(), Phone: phone}
	if cfg != nil {
		req.Template = cfg.Templates["auth_verify"]
	}
	if _, err := p.Call(ctx, pay.EndpointSendCode, req, req.signValues()); err != nil {
		return err
	}
	return nil
}

// WhatsappLogin 校验验证码并换取会话凭证；成功后凭证留在客户端内部，
// 后续充值/提现请求自动带上 token 请求头。
func WhatsappLogin(ctx context.Context, phone, code string) (*LoginResult, error) {
	p, err := currentLogin()
	if err != nil {
		return nil, err
	}
	phone, code = strings.TrimSpace(phone), strings.TrimSpace(code)
	if phone == "" || code == "" {
		return nil, errors.New("whatsapp 登录失败: 手机号或验证码为空")
	}
	req := &loginRequest{AppID: p.AppID(), Phone: phone, Code: code}
	data, err := p.Call(ctx, pay.EndpointLogin, req, req.signValues())
	if err != nil {
		return nil, err
	}
	var d loginData
	if err := json.Unmarshal(data, &d); err != nil {
		// 登录没成功却解不开回执：不能凭「HTTP 成功」就认为拿到了会话，
		// 否则后续每个请求都会带着空 token 出去挨 401。
		return nil, &pay.ProviderError{Channel: "whatsapp", Code: "bad_login_data", Msg: "登录回执不可解析"}
	}
	if strings.TrimSpace(d.Token) == "" {
		return nil, &pay.ProviderError{Channel: "whatsapp", Code: "no_token", Msg: "供应商未返回会话凭证"}
	}
	p.SetToken(d.Token)
	return &LoginResult{UserID: d.UserID, Phone: d.Phone}, nil
}

// WhatsappLogout 清除会话凭证。未登录时为 no-op。
//
// 两条链路都要清：资金链路可能带着从登录链路同步过去的同一份凭证，
// 只清一边等于退出后还能用旧会话出款。
func WhatsappLogout() {
	if p := login.Load(); p != nil {
		p.SetToken("")
	}
	if p := fund.Load(); p != nil {
		p.SetToken("")
	}
}
