package zalo

// 本文件是 ZaloDeleteChat（清空本端会话）的活闸：
//   - 单聊/群聊线程类型与 threadID 原路透传；
//   - boundary 三个字段不做补造，只有 OwnerID 留空时回填当前登录 UID；
//   - 空 threadID / conn 错误 / 未启动 / 非法账号 id 的分支；
//   - 清会话**不占**滚动 1 小时发送配额（它不是发消息）。
//
// 全部走 fake SDK/conn，不连网、不读真实时间。

import (
	"context"
	"errors"
	"testing"
)

// TestZaloDeleteChatRoutesThread 断言单聊/群聊各走一次 conn.DeleteChat，
// 参数原样透传，OwnerID 留空时回填账号 UID。
func TestZaloDeleteChatRoutesThread(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)
	ctx := context.Background()

	if err := ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("单聊清会话: %v", err)
	}
	if err := ZaloDeleteChat(ctx, "acct", "group-1", true, ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "g-1"}); err != nil {
		t.Fatalf("群聊清会话: %v", err)
	}

	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("清会话后没有 conn")
	}
	calls := conn.recordedDeletes()
	if len(calls) != 2 {
		t.Fatalf("DeleteChat 调用 %d 次，期望 2", len(calls))
	}
	if calls[0].group || calls[0].threadID != "peer-1" {
		t.Fatalf("单聊参数不符: %+v", calls[0])
	}
	if calls[0].boundary.OwnerID != "uid-fac" {
		t.Fatalf("OwnerID 留空应回填账号 UID，实得 %q", calls[0].boundary.OwnerID)
	}
	if calls[0].boundary.CliMsgID != "m-1" || calls[0].boundary.GlobalMsgID != "m-1" {
		t.Fatalf("边界归一后应为 cliMsgId=globalMsgId: %+v", calls[0].boundary)
	}
	if !calls[1].group || calls[1].threadID != "group-1" {
		t.Fatalf("群聊参数不符: %+v", calls[1])
	}
	if calls[1].boundary != (ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "g-1"}) {
		t.Fatalf("调用方给的 boundary 被改写: %+v", calls[1].boundary)
	}
}

// TestZaloDeleteChatRejectsBadInput 覆盖空 threadID 与 conn 错误：
// 前者不该打到 conn，后者必须能 errors.Is 穿包装。
func TestZaloDeleteChatRejectsBadInput(t *testing.T) {
	t.Run("empty_thread", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		err := ZaloDeleteChat(context.Background(), "acct", "  ", false, ChatBoundary{GlobalMsgID: "m-1"})
		if !errors.Is(err, errZAFacadeEmptyThread) {
			t.Fatalf("空 threadID err=%v，期望 errZAFacadeEmptyThread", err)
		}
		if conn := sdk.lastConn(); conn != nil && len(conn.recordedDeletes()) != 0 {
			t.Fatalf("空 threadID 仍调了 DeleteChat")
		}
	})

	t.Run("conn_error", func(t *testing.T) {
		_, sdk, _ := facNewSendFixture(t, 0)
		sdk.configure = func(c *facConn) { c.deleteErr = errFacBoom }
		err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"})
		if !errors.Is(err, errFacBoom) {
			t.Fatalf("conn 错误 err=%v，期望可 errors.Is(errFacBoom)", err)
		}
	})
}

// TestZaloDeleteChatDoesNotConsumeSendQuota 钉住「清会话不是发消息」：
// 小时配额 1 时，发一条 + 清一次后，第二条仍应被配额拒绝。
func TestZaloDeleteChatDoesNotConsumeSendQuota(t *testing.T) {
	facNewSendFixture(t, 1)
	ctx := context.Background()

	if _, err := ZaloSendMessage(ctx, "acct", "peer-1", "hello"); err != nil {
		t.Fatalf("首条发送: %v", err)
	}
	if err := ZaloDeleteChat(ctx, "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("发送后清会话: %v", err)
	}
	if _, err := ZaloSendMessage(ctx, "acct", "peer-2", "hello"); !errors.Is(err, ErrZaloSendLimit) {
		t.Fatalf("清会话后第二条 err=%v，期望 ErrZaloSendLimit（清会话不该占发送配额）", err)
	}
}

// TestZaloDeleteChatFillsBoundary 钉住边界归一（2026-10-27 真实接口核实）：
//   - OwnerID 留空 ⇒ 取当前登录 UID（空 owner 会被上游拒成 114）；
//   - CliMsgID 留空 ⇒ 用 GlobalMsgID 顶替（空 cliMsgId 会被上游拒成 112，
//     整次删除不生效 —— 线上「消息发出去但对话没清」就是这个原因）。
func TestZaloDeleteChatFillsBoundary(t *testing.T) {
	_, sdk, _ := facNewSendFixture(t, 0)

	if err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false, ChatBoundary{GlobalMsgID: "m-1"}); err != nil {
		t.Fatalf("清会话: %v", err)
	}
	conn := sdk.lastConn()
	if conn == nil {
		t.Fatal("清会话后没有 conn")
	}
	calls := conn.recordedDeletes()
	if len(calls) != 1 {
		t.Fatalf("DeleteChat 调用 %d 次，期望 1", len(calls))
	}
	if calls[0].boundary.OwnerID != "uid-fac" {
		t.Fatalf("OwnerID 留空应回填当前登录 UID，实得 %q", calls[0].boundary.OwnerID)
	}
	if calls[0].boundary.CliMsgID != "m-1" {
		t.Fatalf("CliMsgID 留空应回填 GlobalMsgID，实得 %q", calls[0].boundary.CliMsgID)
	}

	// 调用方给了值就不能覆盖。
	if err := ZaloDeleteChat(context.Background(), "acct", "peer-1", false,
		ChatBoundary{OwnerID: "uid-other", CliMsgID: "c-9", GlobalMsgID: "m-2"}); err != nil {
		t.Fatalf("清会话: %v", err)
	}
	calls = conn.recordedDeletes()
	last := calls[len(calls)-1]
	if last.boundary.OwnerID != "uid-other" || last.boundary.CliMsgID != "c-9" {
		t.Fatalf("调用方给的边界被改写: %+v", last.boundary)
	}
}
