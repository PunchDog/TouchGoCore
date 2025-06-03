package util

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"touchgocore/network/message"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

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
	fnname := proto.MessageName(pb)
	//使用proto的函数打包数据
	data, err := proto.Marshal(pb)
	if err != nil {
		vars.Error("打包数据失败:", err)
		return nil
	}

	fsmessage := &message.FSMessage{
		Head: &message.Head{
			Protocol1: proto.Int32(protocol1),
			Protocol2: proto.Int32(protocol2),
		},
		Cmd:  proto.String(string(fnname)),
		Body: data,
	}
	return fsmessage
}

func PasreFSMessage(buff interface{}) proto.Message {
	var pb *message.FSMessage = nil
	switch buff.(type) {
	case []byte:
		pb = &message.FSMessage{}
		proto.Unmarshal(buff.([]byte), pb)
	case *message.FSMessage:
		pb = buff.(*message.FSMessage)
	default:
		vars.Error("PasreFSMessage buff type error")
		return nil
	}

	//通過pb.Cmd找到对应的消息处理函数
	msgType, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(pb.GetCmd()))
	if err != nil {
		vars.Error(fmt.Sprintf("找不到消息类型 %s: %v", pb.GetCmd(), err))
		return nil
	}
	msg1 := reflect.New(reflect.TypeOf(msgType.Zero()).Elem()).Interface().(proto.Message)
	err = proto.Unmarshal(pb.GetBody(), msg1)
	if err != nil {
		vars.Error(fmt.Sprintf("proto[%v].Unmarshal error : %v. ---> msg:%+v.", msgType, err, msg1))
		return nil
	}
	return msg1
}
