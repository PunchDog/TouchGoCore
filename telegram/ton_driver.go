package telegram

import "touchgocore/pay"

// DriverTonAPI 是「TON 出款直接上链」这一类驱动的登记名。
//
// 名字登记在本包而不是 pay：pay 是零内部依赖的契约叶子包，把某条链的接入点名字
// 写进它的常量表，等于让契约层反过来知道 TON 的事（与 ChainRule 同口径）。
// 配置里把 pay_sdks.<段>.driver 换成这个名字，就整段换成本地签名广播的出款路径；
// 换回 generic_md5 即整段退回供应商，通道门面与上游调用代码都不动。
const DriverTonAPI = "ton_tonapi"

func init() {
	// 重名 panic 而不是记日志：两处实现抢同一个驱动名时，生效的是包初始化顺序而不是配置意图，
	// 后果是「以为出款走的是链上、实际发给了供应商」——那是资损级事故，必须炸在启动期。
	if err := pay.Register(DriverTonAPI, newTonOnchainChannel); err != nil {
		panic(err)
	}
}
