package instagram

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

// InstagramSendCode 向手机号下发登录用的一次性凭证。
//
// 凭证明文只在供应商报文里出现，本包不落内存、不写日志、不返回给调用方。
func InstagramSendCode(ctx context.Context, phone string) error {
	p, err := currentLogin()
	if err != nil {
		return err
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return errors.New("instagram 下发验证码失败: 手机号为空")
	}
	cfg := instagramCfg()
	req := &sendCodeRequest{AppID: p.AppID(), Phone: phone}
	if cfg != nil {
		req.Template = cfg.Templates["auth_verify"]
	}
	if _, err := p.Call(ctx, pay.EndpointSendCode, req, req.signValues()); err != nil {
		return err
	}
	return nil
}

// InstagramLogin 校验一次性凭证并换取会话凭证；成功后凭证留在客户端内部，
// 后续充值/提现请求自动带上 token 请求头。
//
// code 是供应商侧一次性凭证——短信验证码、OAuth 授权码、或回调 ticket，
// 以供应商在 pay_sdks 的登记口径为准，本包不猜：它只负责把这个串原样送进报文。
func InstagramLogin(ctx context.Context, phone, code string) (*LoginResult, error) {
	p, err := currentLogin()
	if err != nil {
		return nil, err
	}
	phone, code = strings.TrimSpace(phone), strings.TrimSpace(code)
	if phone == "" || code == "" {
		return nil, errors.New("instagram 登录失败: 手机号或验证码为空")
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
		return nil, &pay.ProviderError{Channel: "instagram", Code: "bad_login_data", Msg: "登录回执不可解析"}
	}
	if strings.TrimSpace(d.Token) == "" {
		return nil, &pay.ProviderError{Channel: "instagram", Code: "no_token", Msg: "供应商未返回会话凭证"}
	}
	p.SetToken(d.Token)
	return &LoginResult{UserID: d.UserID, Phone: d.Phone}, nil
}

// InstagramLogout 清除会话凭证。未登录时为 no-op。
//
// 两条链路都要清：资金链路可能带着从登录链路同步过去的同一份凭证，
// 只清一边等于退出后还能用旧会话出款。
func InstagramLogout() {
	if p := login.Load(); p != nil {
		p.SetToken("")
	}
	if p := fund.Load(); p != nil {
		p.SetToken("")
	}
}

// FriendInfo 是好友列表里的一条账号记录。
//
// Instagram 没有「好友」概念：在本平台它描述的是一条关联账号（同属该会话账号的
// 关联关系），逻辑名仍沿用 pay.EndpointFriendList，口径见包注释。
//
// 字段按「文档到位后照抄」的位置留着；它只描述供应商回什么，
// 不保证都非空——供应商没给的列就是空串。
type FriendInfo struct {
	UserID   string `json:"user_id"`
	Phone    string `json:"phone"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"`
}

// InstagramFriends 按传入的 instagram 账号向供应商查关联账号列表。
//
// 语义读作「按账号返回的关联账号列表」而不是「好友列表」：Instagram 没有好友概念，
// 但逻辑名仍沿用 pay.EndpointFriendList、门面仍叫 InstagramFriends，理由见包注释
// （四条社交通道各起一个逻辑名，端点表就散成四个名字，取路径那侧只按逻辑名查，
// 多一个名字就是多一处配漏了静默失败的地方）。
//
// 走登录链路：关联账号挂在会话（token）下，而不是资金链路上。
// account 是手机号或供应商侧 user_id，两者取其一，trim 后为空即拒。
//
// 同步返回、不广播回调：这是读操作，下游要的是当场拿结果，
// 口径与 InstagramAccount 一致（回执广播只留给会变更的资金动作）。
func InstagramFriends(ctx context.Context, account string) ([]*FriendInfo, error) {
	p, err := currentLogin()
	if err != nil {
		return nil, err
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, errors.New("instagram 查询好友列表失败: 账号为空")
	}
	if ctx == nil {
		ctx = runCtx()
	}
	req := &friendListRequest{AppID: p.AppID(), Account: account}
	data, err := p.Call(ctx, pay.EndpointFriendList, req, req.signValues())
	if err != nil {
		return nil, err
	}
	var d friendListData
	if err := json.Unmarshal(data, &d); err != nil {
		// 解不开列表就报错，不交空切片：空切片会被上游读成「这个账号没有好友」，
		// 把一次回执格式问题伪装成一个业务事实（对齐登录侧「不回空会话」的原则）。
		return nil, &pay.ProviderError{Channel: "instagram", Code: "bad_friend_data", Msg: "好友列表回执不可解析"}
	}
	friends := make([]*FriendInfo, 0, len(d.List))
	for i := range d.List {
		f := d.List[i]
		friends = append(friends, &f)
	}
	return friends, nil
}
