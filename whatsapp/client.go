package whatsapp

import (
	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// openFund 装配充值/提现链路。走 paysdk 的严格口径：必须点名到一个商户账户，
// 否则「这一单从哪个商户号出钱」就成了初始化顺序的函数。
func openFund(cfg *config.Cfg, ref *config.PaySDKRef) (*pay.Provider, bool) {
	res, err := paysdk.Open(cfg, "whatsapp", ref, nil)
	return providerOf("whatsapp", res, err)
}

// openLogin 装配验证码下发与登录链路。这类链路只发消息、不出钱，
// 被引用的 SDK 段没有商户账户也照样能用。
func openLogin(cfg *config.Cfg, ref *config.PaySDKRef) (*pay.Provider, bool) {
	res, err := paysdk.OpenService(cfg, "whatsapp.login", ref)
	return providerOf("whatsapp.login", res, err)
}

// providerOf 把一次装配的结果收口成本包要的形状。
//
// 本包的两条链路都要用会话凭证头（登录后每笔请求都带 token），而凭证只由
// pay.Provider 持有：将来若登记了不给会话态的驱动，这里拒绝启动，
// 而不是让登录成功变成「后面每一笔都 401」。
func providerOf(channel string, res *paysdk.Resolved, err error) (*pay.Provider, bool) {
	if err != nil {
		vars.Info("%s 链路不启动: %v", channel, err)
		return nil, false
	}
	p, ok := res.Channel.(*pay.Provider)
	if !ok {
		vars.Error("%s 链路的驱动 %s 不提供会话凭证，无法用于本包", channel, res.Driver)
		return nil, false
	}
	vars.Info("%s 链路已就绪: sdk=%s 商户号=%s", channel, res.Section, res.MerchantID)
	return p, true
}

// sendCodeRequest 是下发验证码报文。
//
// 字段名与签名域顺序都按「文档到位后照抄」的位置留着：改报文只改这个结构体的
// json tag，改签名域只改 signValues 的顺序，两者写在相邻几行是刻意的——
// 报文加了字段而签名域漏掉，是供应商全量拒签最典型的成因。
type sendCodeRequest struct {
	AppID    string `json:"app_id,omitempty"`
	Phone    string `json:"phone"`
	Template string `json:"template,omitempty"`
}

func (r *sendCodeRequest) signValues() []string {
	return []string{r.AppID, r.Phone, r.Template}
}

// loginRequest 是校验验证码并换取会话凭证的报文。
type loginRequest struct {
	AppID string `json:"app_id,omitempty"`
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

func (r *loginRequest) signValues() []string {
	return []string{r.AppID, r.Phone, r.Code}
}

// loginData 是登录回执载荷。
type loginData struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
	Phone  string `json:"phone"`
}
