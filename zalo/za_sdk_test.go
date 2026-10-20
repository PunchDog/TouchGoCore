package zalo

// 本文件是 SDK 边界的活闸：纯映射（用户/群）的字段搬运、排序、map 键补齐、
// AdminIDs 深拷贝，以及 newZASDK 的构造。真实网络调用不在这里测：
// 登录/收发需要 Zalo 服务端，由 fake conn 与真机验证覆盖。

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	zcagoapi "github.com/amrakk/zcago/api"
	"github.com/amrakk/zcago/model"
)

// TestZANewZASDK 断言工厂返回可用的真实现。
func TestZANewZASDK(t *testing.T) {
	t.Parallel()

	sdk := newZASDK()
	if sdk == nil {
		t.Fatal("newZASDK 返回 nil")
	}
	if _, ok := sdk.(*zaRealSDK); !ok {
		t.Fatalf("newZASDK 类型=%T，期望 *zaRealSDK", sdk)
	}
}

// TestZANewZAHTTPClientIsolated 钉住 P0：每次登录必须拿到独立的 client 与
// cookie jar。zcago 默认 client 是 http.DefaultClient，且 newContextImpl 会把
// 新建 jar 写回它（session/options.go:70、context_impl.go:48-53）；共用会让
// 第二个账号复用第一个账号的 cookie。
func TestZANewZAHTTPClientIsolated(t *testing.T) {
	t.Parallel()

	c1, c2 := newZAHTTPClient(), newZAHTTPClient()
	if c1 == c2 {
		t.Fatal("两次登录复用了同一个 http.Client")
	}
	if c1.Jar == nil || c2.Jar == nil {
		t.Fatal("私有 client 必须带 cookie jar")
	}
	if c1.Jar == c2.Jar {
		t.Fatal("两次登录复用了同一个 cookie jar")
	}
	u := &url.URL{Scheme: "https", Host: "chat.zalo.me"}
	c1.Jar.SetCookies(u, []*http.Cookie{{Name: "secret", Value: "a", Path: "/", Secure: true}})
	if got := c2.Jar.Cookies(u); len(got) != 0 {
		t.Fatalf("账号 A 的 cookie 泄漏到账号 B 的 jar: %v", got)
	}
}

// TestZAMapZAUser 断言 model.User → UserInfo 的字段一一对应。
func TestZAMapZAUser(t *testing.T) {
	t.Parallel()

	in := model.User{
		UserId:      "u-1",
		Username:    "alice",
		DisplayName: "Alice A.",
		ZaloName:    "Alice Zalo",
		PhoneNumber: "+84000000000",
		Avatar:      "https://example.invalid/avatar.png",
		Status:      "active",
	}
	want := UserInfo{
		UserID:      "u-1",
		Username:    "alice",
		DisplayName: "Alice A.",
		ZaloName:    "Alice Zalo",
		Phone:       "+84000000000",
		Avatar:      "https://example.invalid/avatar.png",
		Status:      "active",
	}

	if got := mapZAUser(in); got != want {
		t.Fatalf("mapZAUser=%+v，期望 %+v", got, want)
	}
}

// TestZAMapZAUsersSort 断言按 UserID 升序，且不依赖输入顺序。
func TestZAMapZAUsersSort(t *testing.T) {
	t.Parallel()

	in := []model.User{
		{UserId: "u-3", DisplayName: "C"},
		{UserId: "u-1", DisplayName: "A"},
		{UserId: "u-2", DisplayName: "B"},
	}
	got := mapZAUsers(in)
	wantIDs := []string{"u-1", "u-2", "u-3"}
	gotIDs := make([]string, 0, len(got))
	for _, u := range got {
		gotIDs = append(gotIDs, u.UserID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("mapZAUsers 顺序=%v，期望 %v", gotIDs, wantIDs)
	}
	// 输入切片的顺序不能被改写（排序只作用于新切片）。
	if in[0].UserId != "u-3" || in[2].UserId != "u-2" {
		t.Fatalf("mapZAUsers 改写了输入序列：%+v", in)
	}
}

// TestZAMapZAUsersEmpty 断言空输入的返回是空切片而不是 nil，调用方可直接序列化。
func TestZAMapZAUsersEmpty(t *testing.T) {
	t.Parallel()

	if got := mapZAUsers(nil); got == nil {
		t.Fatal("mapZAUsers(nil) 返回 nil，期望空切片")
	} else if len(got) != 0 {
		t.Fatalf("mapZAUsers(nil) 长度=%d，期望 0", len(got))
	}
}

// TestZAMapZAGroupFields 断言群字段映射，含 Type 转 int、LockSendMsg 的 0/非 0 口径。
func TestZAMapZAGroupFields(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		lock      int
		wantLock  bool
		groupType model.GroupType
	}{
		{name: "unlocked", lock: 0, wantLock: false, groupType: model.GroupTypeGroup},
		{name: "locked", lock: 1, wantLock: true, groupType: model.GroupTypeCommunity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := zcagoapi.GroupInfo{
				GroupInfo: model.GroupInfo{
					GroupID:     "g-1",
					Name:        "Group One",
					Description: "desc",
					CreatorID:   "u-owner",
					Avatar:      "https://example.invalid/g.png",
					Version:     "ver-9",
					TotalMember: 12,
					MaxMember:   200,
					Type:        tc.groupType,
					AdminIDs:    []string{"u-admin-1", "u-admin-2"},
					Setting:     model.GroupSetting{LockSendMsg: tc.lock},
				},
			}
			want := GroupInfo{
				GroupID:     "g-1",
				Name:        "Group One",
				Description: "desc",
				CreatorID:   "u-owner",
				Avatar:      "https://example.invalid/g.png",
				Version:     "ver-9",
				TotalMember: 12,
				MaxMember:   200,
				Type:        int(tc.groupType),
				AdminIDs:    []string{"u-admin-1", "u-admin-2"},
				LockSendMsg: tc.wantLock,
			}

			if got := mapZAGroup(in); !reflect.DeepEqual(got, want) {
				t.Fatalf("mapZAGroup=%+v，期望 %+v", got, want)
			}
		})
	}
}

// TestZAMapZAGroupsKeyFallback 断言 map 键在 GroupID 为空时补齐，
// 内嵌 GroupID 非空时以它为准，结果按 GroupID 升序。
func TestZAMapZAGroupsKeyFallback(t *testing.T) {
	t.Parallel()

	in := map[string]zcagoapi.GroupInfo{
		"g-key-2": {GroupInfo: model.GroupInfo{Name: "fallback"}},
		"g-key-1": {GroupInfo: model.GroupInfo{GroupID: "g-explicit", Name: "explicit"}},
	}
	got := mapZAGroups(in)
	if len(got) != 2 {
		t.Fatalf("mapZAGroups 长度=%d，期望 2", len(got))
	}
	if got[0].GroupID != "g-explicit" || got[0].Name != "explicit" {
		t.Fatalf("首个元素=%+v，期望 GroupID=g-explicit（显式值优先）", got[0])
	}
	if got[1].GroupID != "g-key-2" || got[1].Name != "fallback" {
		t.Fatalf("次个元素=%+v，期望 GroupID=g-key-2（map 键补齐）", got[1])
	}
}

// TestZAMapZAGroupAdminIDsDeepCopy 断言 AdminIDs 与输入切片互不影响。
func TestZAMapZAGroupAdminIDsDeepCopy(t *testing.T) {
	t.Parallel()

	admins := []string{"u-admin-1", "u-admin-2"}
	in := zcagoapi.GroupInfo{
		GroupInfo: model.GroupInfo{GroupID: "g-1", AdminIDs: admins},
	}
	got := mapZAGroup(in)
	if len(got.AdminIDs) != 2 {
		t.Fatalf("AdminIDs 长度=%d，期望 2", len(got.AdminIDs))
	}

	admins[0] = "mutated-input"
	if got.AdminIDs[0] != "u-admin-1" {
		t.Fatalf("修改输入污染了输出：AdminIDs[0]=%q", got.AdminIDs[0])
	}

	got.AdminIDs[1] = "mutated-output"
	if admins[1] != "u-admin-2" {
		t.Fatalf("修改输出污染了输入：admins[1]=%q", admins[1])
	}
}
