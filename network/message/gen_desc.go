//go:build ignore

package main

import (
	"fmt"
	"os"

	"google.golang.org/protobuf/proto"
	descpb "google.golang.org/protobuf/types/descriptorpb"
)

func main() {
	labelOpt := descpb.FieldDescriptorProto_LABEL_OPTIONAL
	labelReq := descpb.FieldDescriptorProto_LABEL_REQUIRED
	tInt32 := descpb.FieldDescriptorProto_TYPE_INT32
	tUint64 := descpb.FieldDescriptorProto_TYPE_UINT64
	tString := descpb.FieldDescriptorProto_TYPE_STRING
	tBytes := descpb.FieldDescriptorProto_TYPE_BYTES
	tMsg := descpb.FieldDescriptorProto_TYPE_MESSAGE

	fd := &descpb.FileDescriptorProto{
		Name:    proto.String("FSMessage.proto"),
		Package: proto.String("network.message"),
		Options: &descpb.FileOptions{
			GoPackage: proto.String("touchgocore/network/message"),
		},
		MessageType: []*descpb.DescriptorProto{
			{
				Name: proto.String("Head"),
				Field: []*descpb.FieldDescriptorProto{
					{Name: proto.String("protocol1"), Number: proto.Int32(1), Label: &labelOpt, Type: &tInt32, JsonName: proto.String("protocol1")},
					{Name: proto.String("protocol2"), Number: proto.Int32(2), Label: &labelOpt, Type: &tInt32, JsonName: proto.String("protocol2")},
					{Name: proto.String("cmd"), Number: proto.Int32(3), Label: &labelOpt, Type: &tString, JsonName: proto.String("cmd")},
					{Name: proto.String("request_id"), Number: proto.Int32(4), Label: &labelOpt, Type: &tUint64, JsonName: proto.String("requestId")},
				},
			},
			{
				Name: proto.String("FSMessage"),
				Field: []*descpb.FieldDescriptorProto{
					{Name: proto.String("head"), Number: proto.Int32(1), Label: &labelReq, Type: &tMsg, TypeName: proto.String(".network.message.Head"), JsonName: proto.String("head")},
					{Name: proto.String("body"), Number: proto.Int32(2), Label: &labelReq, Type: &tBytes, JsonName: proto.String("body")},
				},
			},
		},
	}
	b, err := proto.Marshal(fd)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(os.Stdout, "len=%d\n", len(b))
	for i, x := range b {
		if i > 0 {
			fmt.Print(", ")
		}
		if i%16 == 0 {
			fmt.Print("\n")
		}
		fmt.Printf("0x%02x", x)
	}
	fmt.Println()
}
