package facebook

import (
	"encoding/json"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// openFund 装配充值/提现链路。走 paysdk 的严格口径：必须点名到一个商户账户，
// 否则「这一单从哪个商户号出钱」就成了初始化顺序的函数。
func openFund(cfg *config.Cfg, ref *config.PaySDKRef) (*pay.Provider, bool) {
	res, err := paysdk.Open(cfg, "facebook", ref, nil)
	return providerOf("facebook", res, err)
}

// openLogin 装配验证码下发与登录链路。这类链路只发消息、不出钱，
// 被引用的 SDK 段没有商户账户也照样能用。
func openLogin(cfg *config.Cfg, ref *config.PaySDKRef) (*pay.Provider, bool) {
	res, err := paysdk.OpenService(cfg, "facebook.login", ref)
	return providerOf("facebook.login", res, err)
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

// loginRequest 是校验一次性凭证并换取会话凭证的报文。
type loginRequest struct {
	AppID string `json:"app_id,omitempty"`
	Phone string `json:"phone"`
	// Code 是供应商侧的一次性凭证：短信验证码、OAuth 授权码、或回调 ticket，
	// 以供应商在 pay_sdks 的登记口径为准，本包不猜是哪一种。三种形态都只在
	// 「这一次请求」里有效，换回来的都是会话 token，报文与签名域的形状因此不变。
	Code string `json:"code"`
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

// friendListRequest 是按账号查好友列表的报文。
//
// 字段名与签名域顺序都按「文档到位后照抄」的位置留着：改报文只改这个结构体的
// json tag，改签名域只改 signValues 的顺序，两者写在相邻几行是刻意的——
// 报文加了字段而签名域漏掉，是供应商全量拒签最典型的成因。
type friendListRequest struct {
	AppID string `json:"app_id,omitempty"`
	// Account 是传入的 facebook 账号（手机号或供应商侧 user_id），两者取其一，
	// 口径由供应商登记的那个账号为准——本包不猜它指的是哪种。
	Account string `json:"account"`
}

func (r *friendListRequest) signValues() []string {
	return []string{r.AppID, r.Account}
}

// friendListData 是好友列表的回执载荷。
//
// 兼容两种供应商形态：data 为 {"list":[...]} 或直接就是数组。
// 解不开列表时报错而不是交空切片：空切片会被上游读成「这个账号没有好友」，
// 把一次回执格式问题伪装成一个业务事实。
type friendListData struct {
	List []FriendInfo
}

// UnmarshalJSON 先按对象形态解，不中再按数组形态解；两者都不中报错。
func (d *friendListData) UnmarshalJSON(b []byte) error {
	var obj struct {
		List []FriendInfo `json:"list"`
	}
	if err := json.Unmarshal(b, &obj); err == nil && obj.List != nil {
		d.List = obj.List
		return nil
	}
	var arr []FriendInfo
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	d.List = arr
	return nil
}
