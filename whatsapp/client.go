package whatsapp

import (
	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// openFund 装配充值/提现链路。走 paysdk 的严格口径：必须点名到一个商户账户，
// 否则「这一单从哪个商户号出钱」就成了初始化顺序的函数。
//
// 本包只剩这一条 pay_sdks 链路：消息侧（登录/发信/删会话）已改由 whatsmeow 直连
// WhatsApp 协议（wa_client.go），不再经过 HTTP 消息网关，所以原来的 openLogin
// 与那一族报文结构体一并撤掉。资金动作必须有持牌主体，仍然留在供应商侧。
//
// 装配结果必须收口成本包要的形状：资金链路要用会话凭证头（供应商给的 token 每笔
// 请求都带），而凭证只由 pay.Provider 持有——将来若登记了不给会话态的驱动，
// 这里拒绝启动，而不是让每一笔都 401。
func openFund(cfg *config.Cfg, ref *config.PaySDKRef) (*pay.Provider, bool) {
	res, err := paysdk.Open(cfg, "whatsapp", ref, nil)
	if err != nil {
		vars.Info("whatsapp 链路不启动: %v", err)
		return nil, false
	}
	p, ok := res.Channel.(*pay.Provider)
	if !ok {
		vars.Error("whatsapp 链路的驱动 %s 不提供会话凭证，无法用于本包", res.Driver)
		return nil, false
	}
	vars.Info("whatsapp 链路已就绪: sdk=%s 商户号=%s", res.Section, res.MerchantID)
	return p, true
}
