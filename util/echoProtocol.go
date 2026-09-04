package util

import (
	"encoding/binary"
	"fmt"
	"sync"
	"touchgocore/network/message"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

var (
	protoTypeByKey sync.Map // uint64 -> protoreflect.FullName
	allowedProto   sync.Map // FullName -> struct{}
)

func protocolKey(p1, p2 int32) uint64 {
	return (uint64(uint32(p1)) << 32) | uint64(uint32(p2))
}

func RegisterProtocolType(protocol1, protocol2 int32, pb proto.Message) {
	if pb == nil {
		return
	}
	name := proto.MessageName(pb)
	protoTypeByKey.Store(protocolKey(protocol1, protocol2), name)
	allowedProto.Store(name, struct{}{})
}

// ProtocolBinding 协议号到消息类型的绑定
type ProtocolBinding struct {
	Protocol1 int32
	Protocol2 int32
	Message   proto.Message
}

func RegisterProtocolTypes(bindings ...ProtocolBinding) {
	for _, b := range bindings {
		RegisterProtocolType(b.Protocol1, b.Protocol2, b.Message)
	}
}

func LookupProtocolType(protocol1, protocol2 int32) (protoreflect.FullName, bool) {
	v, ok := protoTypeByKey.Load(protocolKey(protocol1, protocol2))
	if !ok {
		return "", false
	}
	return v.(protoreflect.FullName), true
}

type EchoPacket struct {
	buff []byte
}

func InitEchoPacket(buff []byte) *EchoPacket {
	return &EchoPacket{buff: buff}
}

func (this *EchoPacket) Serialize() []byte {
	return this.buff
}

func (this *EchoPacket) GetLength() uint32 {
	return binary.BigEndian.Uint32(this.buff[0:4])
}

func (this *EchoPacket) GetProtocol2() int32 {
	return int32(binary.BigEndian.Uint32(this.buff[4:8]))
}

func (this *EchoPacket) GetProtocol1() int32 {
	return int32(binary.BigEndian.Uint32(this.buff[8:12]))
}

func (this *EchoPacket) GetBody() []byte {
	return this.buff[12 : 12+this.GetLength()]
}

func NewEchoPacket(protocol1 int32, protocol2 int32, buff []byte, bufflen int) *EchoPacket {
	p := new(EchoPacket)
	p.buff = make([]byte, 12+bufflen)
	binary.BigEndian.PutUint32(p.buff[0:4], uint32(bufflen))
	binary.BigEndian.PutUint32(p.buff[4:8], uint32(protocol2))
	binary.BigEndian.PutUint32(p.buff[8:12], uint32(protocol1))
	copy(p.buff[12:], buff)
	return p
}

func NewFSMessage(protocol1 int32, protocol2 int32, pb proto.Message) *message.FSMessage {
	return NewFSMessageWithID(protocol1, protocol2, 0, pb)
}

func NewFSMessageWithID(protocol1 int32, protocol2 int32, requestID uint64, pb proto.Message) *message.FSMessage {
	fnname := proto.MessageName(pb)
	RegisterProtocolType(protocol1, protocol2, pb)
	data, err := proto.Marshal(pb)
	if err != nil {
		vars.Error("打包数据失败:", err)
		return nil
	}

	head := &message.Head{
		Protocol1: proto.Int32(protocol1),
		Protocol2: proto.Int32(protocol2),
		Cmd:       proto.String(string(fnname)),
	}
	if requestID != 0 {
		head.RequestId = proto.Uint64(requestID)
	}
	return &message.FSMessage{
		Head: head,
		Body: data,
	}
}

func PasreFSMessage(buff interface{}) proto.Message {
	var pb *message.FSMessage = nil
	switch buff.(type) {
	case []byte:
		pb = &message.FSMessage{}
		if err := proto.Unmarshal(buff.([]byte), pb); err != nil {
			vars.Error("PasreFSMessage unmarshal error: %v", err)
			return nil
		}
	case *message.FSMessage:
		pb = buff.(*message.FSMessage)
	default:
		vars.Error("PasreFSMessage buff type error")
		return nil
	}

	//通過pb.Cmd找到对应的消息处理函数
	if pb.GetHead() == nil {
		vars.Error("PasreFSMessage missing head")
		return nil
	}

	p1 := pb.GetHead().GetProtocol1()
	p2 := pb.GetHead().GetProtocol2()
	v, ok := protoTypeByKey.Load(protocolKey(p1, p2))
	if !ok {
		vars.Error("未注册协议 %d:%d，拒绝解析 Cmd=%s", p1, p2, pb.GetHead().GetCmd())
		return nil
	}
	cmdName := v.(protoreflect.FullName)

	msgType, err := protoregistry.GlobalTypes.FindMessageByName(cmdName)
	if err != nil {
		vars.Error(fmt.Sprintf("找不到消息类型 %s (protocol %d:%d): %v", cmdName, p1, p2, err))
		return nil
	}
	msg1, ok := msgType.New().Interface().(proto.Message)
	if !ok {
		vars.Error("消息类型 %s 未实现 proto.Message", cmdName)
		return nil
	}
	err = proto.Unmarshal(pb.GetBody(), msg1)
	if err != nil {
		vars.Error(fmt.Sprintf("proto[%v].Unmarshal error : %v. ---> msg:%+v.", msgType, err, msg1))
		return nil
	}
	return msg1
}
