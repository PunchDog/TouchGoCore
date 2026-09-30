package zalo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"touchgocore/pay"
)

// friendEndpoints 在既有端点表上补出好友列表端点：
// 供应商没登记 endpoints.friend_list 时请求必须就地拒绝（见缺端点用例）。
func friendEndpoints() map[string]string {
	m := allEndpoints()
	m[pay.EndpointFriendList] = "/api/friends"
	return m
}

// TestFriendSignValuesMatchPayloadOrder 签名域顺序必须与报文里登记的字段的先后一致。
func TestFriendSignValuesMatchPayloadOrder(t *testing.T) {
	got := (&friendListRequest{AppID: "app1", Account: "86138"}).signValues()
	want := []string{"app1", "86138"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("好友查询签名域=%v，期望 %v", got, want)
	}
}

// TestFriendsAccountTrimmedIntoPayload account 进报文前必须 trim：
// 带空白的账号在供应商侧是另一个查不到的键。
func TestFriendsAccountTrimmedIntoPayload(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	f.response["/api/friends"] = `{"code":"0","data":{"list":[]}}`
	startWith(t, payCfg(url, friendEndpoints()))
	recorded()

	if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	list, err := ZaloFriends(nil, " 8613800000000 ")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("空列表回执=%+v", list)
	}
	req := f.seen()[1]
	if !strings.Contains(req.body, `"account":"8613800000000"`) {
		t.Fatalf("账号未 trim 或没进报文: %s", req.body)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("好友查询是读操作，不该广播回执: %+v", got)
	}
}

// TestFriendsObjectAndArrayResponsesParse 兼容两种供应商形态：
// data 为 {"list":[...]} 对象或直接就是数组，字段逐一读回。
func TestFriendsObjectAndArrayResponsesParse(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"对象形态", `{"code":"0","data":{"list":[{"user_id":"U1","phone":"86138","nickname":"阿明","status":"active"}]}}`},
		{"数组形态", `{"code":"0","data":[{"user_id":"U1","phone":"86138","nickname":"阿明","status":"active"}]}`},
	}
	for _, cs := range cases {
		f, url := newFakeSupplier(t)
		f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
		f.response["/api/friends"] = cs.payload
		startWith(t, payCfg(url, friendEndpoints()))

		if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
			t.Fatal(err)
		}
		list, err := ZaloFriends(nil, "U9")
		if err != nil {
			t.Fatalf("%s 查询失败: %v", cs.name, err)
		}
		if len(list) != 1 {
			t.Fatalf("%s 列表长度=%d，期望 1: %+v", cs.name, len(list), list)
		}
		fr := *list[0]
		if fr.UserID != "U1" || fr.Phone != "86138" || fr.Nickname != "阿明" || fr.Status != "active" {
			t.Fatalf("%s 好友记录不符: %+v", cs.name, fr)
		}
		// 会话凭证走请求头，不进报文；签名覆盖 AppID+Account 两个登记字段。
		req := f.seen()[1]
		if req.auth != "TK-1" {
			t.Fatalf("%s 后续请求未带会话凭证: %q", cs.name, req.auth)
		}
		wantSign := pay.SignMD5(testSecret, "app1", "U9")
		if req.sign != wantSign {
			t.Fatalf("%s 签名=%s，期望 %s", cs.name, req.sign, wantSign)
		}
		if strings.Contains(req.body, "TK-1") || strings.Contains(req.body, testSecret) {
			t.Fatalf("%s 凭证进了报文: %s", cs.name, req.body)
		}
	}
}

// TestFriendsUnparsableResponseIsError 解不开列表就报错，不交空切片：
// 空切片会被上游读成「这个账号没有好友」，把回执格式问题伪装成业务事实。
func TestFriendsUnparsableResponseIsError(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	f.response["/api/friends"] = `{"code":"0","data":"not-a-list"}`
	startWith(t, payCfg(url, friendEndpoints()))

	if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	list, err := ZaloFriends(nil, "U9")
	if err == nil {
		t.Fatalf("不可解析的回执应当报错，实得 %+v", list)
	}
	var pe *pay.ProviderError
	if !errors.As(err, &pe) || pe.Code != "bad_friend_data" {
		t.Fatalf("错误类型不符: %v", err)
	}
	if list != nil {
		t.Errorf("报错时不得同时返回列表: %+v", list)
	}
}

// TestFriendsMissingEndpointSendsNothing 供应商段没登记 friend_list 端点时
// 必须就地拒绝、零请求，错误里指明缺哪个端点。
func TestFriendsMissingEndpointSendsNothing(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	cfg := payCfg(url, allEndpoints()) // 刻意不含 friend_list
	startWith(t, cfg)

	if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := ZaloFriends(nil, "U9"); err == nil {
		t.Fatal("未登记端点应当报错")
	} else if !strings.Contains(err.Error(), "endpoints."+pay.EndpointFriendList) {
		t.Fatalf("错误未指明缺哪个端点: %v", err)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("除登录外不应发出请求，实得 %d 次", n)
	}
}

// TestFriendsEmptyAccountRefusedBeforeSending 账号为空（含纯空白）在发出请求之前就拒。
func TestFriendsEmptyAccountRefusedBeforeSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	startWith(t, payCfg(url, friendEndpoints()))

	if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"", "   "} {
		if _, err := ZaloFriends(nil, acct); err == nil {
			t.Errorf("空账号 %q 未被拦下", acct)
		}
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("空账号却发出了 %d 次额外请求", n-1)
	}
}

// TestFriendsLogoutClearsToken 退出后好友请求不再带旧会话凭证：
// 好友数据挂在会话下，拿已失效凭证查询只会换回一个 401。
func TestFriendsLogoutClearsToken(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	f.response["/api/friends"] = `{"code":"0","data":{"list":[{"user_id":"U1"}]}}`
	startWith(t, payCfg(url, friendEndpoints()))

	if _, err := ZaloLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := ZaloFriends(nil, "U9"); err != nil {
		t.Fatal(err)
	}
	if req := f.seen()[1]; req.auth != "TK-1" {
		t.Fatalf("登录后好友请求未带会话凭证: %q", req.auth)
	}
	ZaloLogout()
	if _, err := ZaloFriends(nil, "U9"); err != nil {
		t.Fatal(err)
	}
	if req := f.seen()[2]; req.auth != "" {
		t.Fatalf("退出后仍带会话凭证: %q", req.auth)
	}
}

// TestFriendListDataUnmarshalShapes 回执兜底解析的单测：对象、数组、空数组三种形态，
// 以及两者都不中的坏形态必须返回错误。
func TestFriendListDataUnmarshalShapes(t *testing.T) {
	var d friendListData
	if err := json.Unmarshal([]byte(`{"list":[{"user_id":"U1"}]}`), &d); err != nil || len(d.List) != 1 || d.List[0].UserID != "U1" {
		t.Fatalf("对象形态解不符: err=%v d=%+v", err, d)
	}
	if err := json.Unmarshal([]byte(`[{"user_id":"U2"}]`), &d); err != nil || len(d.List) != 1 || d.List[0].UserID != "U2" {
		t.Fatalf("数组形态解不符: err=%v d=%+v", err, d)
	}
	if err := json.Unmarshal([]byte(`[]`), &d); err != nil || len(d.List) != 0 {
		t.Fatalf("空数组应当解成空列表: err=%v d=%+v", err, d)
	}
	for _, bad := range []string{`"str"`, `123`, `{"other":1}`} {
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("坏形态 %s 未被判错", bad)
		}
	}
}
