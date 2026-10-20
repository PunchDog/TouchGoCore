package zalo

// 本文件是入站映射的活闸：UserMessage/GroupMessage（值/指针）、self 判定、
// 空文本/非文本过滤、坏时间戳归零。
//
// listener 生命周期（Start/Stop、断线重连、关闭码）不在本文件测：真实 listener
// 需要 Zalo 的 WebSocket 服务器，由 fake conn 与真机验证覆盖。

import (
	"context"
	"errors"
	"testing"

	"github.com/amrakk/zcago/model"
)

// lstStrPtr 返回字符串指针，便于构造 Content.String。
func lstStrPtr(s string) *string { return &s }

// TestZAStartListenerNilAPI 断言没有登录句柄时 StartListener 立即报错而不是 panic，
// 未启动状态下的 StopListener 可重复调用。真实启停/重连/关闭码由 fake conn 与真机验证覆盖。
func TestZAStartListenerNilAPI(t *testing.T) {
	t.Parallel()

	c := &zaRealConn{}
	if err := c.StartListener(context.Background(), nil, nil); !errors.Is(err, errZANilAPI) {
		t.Fatalf("StartListener err=%v，期望 %v", err, errZANilAPI)
	}
	c.StopListener()
	c.StopListener()
}

// lstFakeMessage 是一个不被 messageToInbound 认识的 model.Message，
// 用于覆盖类型开关的 default 分支。
type lstFakeMessage struct{}

func (lstFakeMessage) Type() model.ThreadType { return model.ThreadTypeUser }
func (lstFakeMessage) ThreadID() string       { return "thread-x" }
func (lstFakeMessage) IsSelf() bool           { return false }

// TestZAMessageToInbound 表驱动覆盖映射与过滤规则。
func TestZAMessageToInbound(t *testing.T) {
	t.Parallel()

	ptrUser := model.NewUserMessage("self-uid", model.TMessage{
		MsgID:   "m-ptr",
		UIDFrom: "peer-ptr",
		IDTo:    "self-uid",
		DName:   "Ptr User",
		TS:      "7",
		Content: model.Content{String: lstStrPtr("from pointer")},
	})
	ptrGroup := model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
		MsgID:   "gm-ptr",
		UIDFrom: "member-ptr",
		IDTo:    "group-ptr",
		DName:   "Ptr Member",
		TS:      "8",
		Content: model.Content{String: lstStrPtr("group pointer")},
	}})

	cases := []struct {
		name   string
		msg    model.Message
		want   InboundMessage
		wantOK bool
	}{
		{
			name: "user_text",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				MsgID:   "m-1",
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				DName:   "Alice",
				TS:      "1700000000123",
				Content: model.Content{String: lstStrPtr("hello")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				IsSelf:      false,
				MsgID:       "m-1",
				FromID:      "peer-1",
				ToID:        "self-uid",
				SenderName:  "Alice",
				Text:        "hello",
				TimestampMS: 1700000000123,
			},
			wantOK: true,
		},
		{
			name: "user_self",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				MsgID:   "m-2",
				UIDFrom: "0",
				IDTo:    "peer-1",
				DName:   "Me",
				TS:      "1",
				Content: model.Content{String: lstStrPtr("yo")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				IsSelf:      true,
				MsgID:       "m-2",
				FromID:      "self-uid",
				ToID:        "peer-1",
				SenderName:  "Me",
				Text:        "yo",
				TimestampMS: 1,
			},
			wantOK: true,
		},
		{
			name: "group_text",
			msg: model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
				MsgID:   "gm-1",
				UIDFrom: "member-1",
				IDTo:    "group-1",
				DName:   "Bob",
				TS:      "42",
				Content: model.Content{String: lstStrPtr("hi group")},
			}}),
			want: InboundMessage{
				ThreadID:    "group-1",
				ThreadType:  "group",
				IsSelf:      false,
				MsgID:       "gm-1",
				FromID:      "member-1",
				ToID:        "group-1",
				SenderName:  "Bob",
				Text:        "hi group",
				TimestampMS: 42,
			},
			wantOK: true,
		},
		{
			name: "group_self",
			msg: model.NewGroupMessage("self-uid", model.TGroupMessage{TMessage: model.TMessage{
				MsgID:   "gm-2",
				UIDFrom: "0",
				IDTo:    "group-1",
				DName:   "Me",
				TS:      "43",
				Content: model.Content{String: lstStrPtr("self in group")},
			}}),
			want: InboundMessage{
				ThreadID:    "group-1",
				ThreadType:  "group",
				IsSelf:      true,
				MsgID:       "gm-2",
				FromID:      "self-uid",
				ToID:        "group-1",
				SenderName:  "Me",
				Text:        "self in group",
				TimestampMS: 43,
			},
			wantOK: true,
		},
		{
			name: "user_pointer",
			msg:  &ptrUser,
			want: InboundMessage{
				ThreadID:    "peer-ptr",
				ThreadType:  "user",
				MsgID:       "m-ptr",
				FromID:      "peer-ptr",
				ToID:        "self-uid",
				SenderName:  "Ptr User",
				Text:        "from pointer",
				TimestampMS: 7,
			},
			wantOK: true,
		},
		{
			name: "group_pointer",
			msg:  &ptrGroup,
			want: InboundMessage{
				ThreadID:    "group-ptr",
				ThreadType:  "group",
				MsgID:       "gm-ptr",
				FromID:      "member-ptr",
				ToID:        "group-ptr",
				SenderName:  "Ptr Member",
				Text:        "group pointer",
				TimestampMS: 8,
			},
			wantOK: true,
		},
		{
			name: "text_kept_verbatim",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{String: lstStrPtr("  spaced  ")},
			}),
			want: InboundMessage{
				ThreadID:    "peer-1",
				ThreadType:  "user",
				FromID:      "peer-1",
				ToID:        "self-uid",
				Text:        "  spaced  ",
				TimestampMS: 5,
			},
			wantOK: true,
		},
		{
			name: "blank_text_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{String: lstStrPtr("   \t\n")},
			}),
			wantOK: false,
		},
		{
			name: "nil_text_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
			}),
			wantOK: false,
		},
		{
			name: "attachment_skipped",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "5",
				Content: model.Content{Attachment: &model.TAttachmentContent{Title: "file"}},
			}),
			wantOK: false,
		},
		{
			name: "bad_ts_zeroed",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "not-a-number",
				Content: model.Content{String: lstStrPtr("ok")},
			}),
			want: InboundMessage{
				ThreadID:   "peer-1",
				ThreadType: "user",
				FromID:     "peer-1",
				ToID:       "self-uid",
				Text:       "ok",
			},
			wantOK: true,
		},
		{
			name: "empty_ts_zeroed",
			msg: model.NewUserMessage("self-uid", model.TMessage{
				UIDFrom: "peer-1",
				IDTo:    "self-uid",
				TS:      "",
				Content: model.Content{String: lstStrPtr("ok")},
			}),
			want: InboundMessage{
				ThreadID:   "peer-1",
				ThreadType: "user",
				FromID:     "peer-1",
				ToID:       "self-uid",
				Text:       "ok",
			},
			wantOK: true,
		},
		{
			name:   "unknown_type_skipped",
			msg:    lstFakeMessage{},
			wantOK: false,
		},
		{
			name:   "nil_pointer_skipped",
			msg:    (*model.UserMessage)(nil),
			wantOK: false,
		},
		{
			name:   "nil_interface_skipped",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := messageToInbound(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v（got=%+v）", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				if got != (InboundMessage{}) {
					t.Fatalf("被过滤的消息返回了非零值：%+v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("InboundMessage=%+v，期望 %+v", got, tc.want)
			}
		})
	}
}
