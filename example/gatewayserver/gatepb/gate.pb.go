package gatepb

import (
	"reflect"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/runtime/protoimpl"
	descpb "google.golang.org/protobuf/types/descriptorpb"
)

const (
	ProtocolPing1 int32 = 1
	ProtocolPing2 int32 = 1
	ProtocolPong1 int32 = 1
	ProtocolPong2 int32 = 2
)

type GatePing struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Payload string `protobuf:"bytes,1,opt,name=payload,proto3" json:"payload,omitempty"`
}

func (x *GatePing) Reset() {
	*x = GatePing{}
	mi := &file_gate_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GatePing) String() string { return protoimpl.X.MessageStringOf(x) }
func (*GatePing) ProtoMessage()    {}
func (x *GatePing) ProtoReflect() protoreflect.Message {
	mi := &file_gate_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *GatePing) GetPayload() string {
	if x != nil {
		return x.Payload
	}
	return ""
}

type GatePong struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Payload string `protobuf:"bytes,1,opt,name=payload,proto3" json:"payload,omitempty"`
}

func (x *GatePong) Reset() {
	*x = GatePong{}
	mi := &file_gate_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GatePong) String() string { return protoimpl.X.MessageStringOf(x) }
func (*GatePong) ProtoMessage()    {}
func (x *GatePong) ProtoReflect() protoreflect.Message {
	mi := &file_gate_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *GatePong) GetPayload() string {
	if x != nil {
		return x.Payload
	}
	return ""
}

var File_gate_proto protoreflect.FileDescriptor

var (
	file_gate_proto_msgTypes = make([]protoimpl.MessageInfo, 2)
	file_gate_proto_goTypes  = []any{
		(*GatePing)(nil),
		(*GatePong)(nil),
	}
	file_gate_proto_depIdxs = []int32{
		0, 0, 0, 0, 0,
	}
)

func init() { file_gate_proto_init() }

func file_gate_proto_init() {
	if File_gate_proto != nil {
		return
	}
	file_gate_proto_msgTypes[0].Exporter = func(v any, i int) any {
		switch v := v.(*GatePing); i {
		case 0:
			return &v.state
		case 1:
			return &v.sizeCache
		case 2:
			return &v.unknownFields
		default:
			return nil
		}
	}
	file_gate_proto_msgTypes[1].Exporter = func(v any, i int) any {
		switch v := v.(*GatePong); i {
		case 0:
			return &v.state
		case 1:
			return &v.sizeCache
		case 2:
			return &v.unknownFields
		default:
			return nil
		}
	}
	labelOpt := descpb.FieldDescriptorProto_LABEL_OPTIONAL
	tString := descpb.FieldDescriptorProto_TYPE_STRING
	fd := &descpb.FileDescriptorProto{
		Name:    proto.String("gate.proto"),
		Package: proto.String("example.gate"),
		Syntax:  proto.String("proto3"),
		Options: &descpb.FileOptions{
			GoPackage: proto.String("touchgocore/example/gatewayserver/gatepb"),
		},
		MessageType: []*descpb.DescriptorProto{
			{
				Name: proto.String("GatePing"),
				Field: []*descpb.FieldDescriptorProto{
					{Name: proto.String("payload"), Number: proto.Int32(1), Label: &labelOpt, Type: &tString, JsonName: proto.String("payload")},
				},
			},
			{
				Name: proto.String("GatePong"),
				Field: []*descpb.FieldDescriptorProto{
					{Name: proto.String("payload"), Number: proto.Int32(1), Label: &labelOpt, Type: &tString, JsonName: proto.String("payload")},
				},
			},
		},
	}
	raw, err := proto.Marshal(fd)
	if err != nil {
		panic(err)
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: raw,
			NumEnums:      0,
			NumMessages:   2,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_gate_proto_goTypes,
		DependencyIndexes: file_gate_proto_depIdxs,
		MessageInfos:      file_gate_proto_msgTypes,
	}.Build()
	File_gate_proto = out.File
	file_gate_proto_goTypes = nil
	file_gate_proto_depIdxs = nil
}
