package util

import (
	"encoding/binary"
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
