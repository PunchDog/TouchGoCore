package impl

import (
	"encoding/binary"
)

type EchoPacket struct {
	buff []byte
}

func (this *EchoPacket) Serialize() []byte {
	return this.buff
}

func (this *EchoPacket) GetLength() uint32 {
	return binary.BigEndian.Uint32(this.buff[0:4])
}

func (this *EchoPacket) GetType() uint32 {
	return binary.BigEndian.Uint32(this.buff[4:8])
}

func (this *EchoPacket) GetCbid() uint64 {
	return binary.BigEndian.Uint64(this.buff[8:16])
}

func (this *EchoPacket) GetBody() []byte {
	return this.buff[16:16+this.GetLength()]
}

func NewEchoPacket(buff []byte, tyc int32, cbid int64) *EchoPacket {
	p := &EchoPacket{}

	p.buff = make([]byte, 16+len(buff))
	binary.BigEndian.PutUint32(p.buff[0:4], uint32(len(buff)))
	binary.BigEndian.PutUint32(p.buff[4:8], uint32(tyc))
	binary.BigEndian.PutUint64(p.buff[8:16], uint64(cbid))
	copy(p.buff[16:], buff)

	return p
}
