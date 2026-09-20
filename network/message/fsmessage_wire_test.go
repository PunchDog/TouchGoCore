package message

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestHeadRequestIDOnWire 校验 request_id 真的参与序列化。
//
// FSMessage.pb.go 的 Go 结构体里有 RequestId 字段，但 rawDesc（protoimpl 用来
// 编解码的描述符）一度只声明了 protocol1/protocol2/cmd 三个字段：字段在结构体上
// 赋值成功、本地读得到， Marshal 出来却是空的，对端 GetRequestId() 恒为 0。
// RPC 的请求/响应关联因此整体退化成「谁先等着就给谁」，同协议的并发请求会串号。
func TestHeadRequestIDOnWire(t *testing.T) {
	fields := (&Head{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != 4 {
		t.Fatalf("✘ Head 描述符只有 %d 个字段，与结构体不一致（request_id 不会被序列化）", fields.Len())
	}
	f4 := fields.ByNumber(4)
	if f4 == nil || f4.Name() != "request_id" {
		t.Fatalf("✘ 描述符缺少 request_id(4)：%v", f4)
	}

	in := &Head{
		Protocol1: proto.Int32(3),
		Protocol2: proto.Int32(1),
		Cmd:       proto.String("google.protobuf.StringValue"),
		RequestId: proto.Uint64(18446744073709551615),
	}
	data, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Head
	if err := proto.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.GetRequestId(); got != in.GetRequestId() {
		t.Fatalf("✘ request_id 经序列化丢失: got %d want %d", got, in.GetRequestId())
	}
}
