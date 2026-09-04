package gatepb

import (
	"testing"

	"touchgocore/util"

	"google.golang.org/protobuf/proto"
)

func TestPingPongRoundTrip(t *testing.T) {
	util.RegisterProtocolTypes(
		util.ProtocolBinding{Protocol1: ProtocolPing1, Protocol2: ProtocolPing2, Message: &GatePing{}},
		util.ProtocolBinding{Protocol1: ProtocolPong1, Protocol2: ProtocolPong2, Message: &GatePong{}},
	)
	fs := util.NewFSMessageWithID(ProtocolPing1, ProtocolPing2, 42, &GatePing{Payload: "hello"})
	if fs == nil {
		t.Fatal("NewFSMessage returned nil")
	}
	if fs.GetHead().GetRequestId() != 42 {
		t.Fatalf("request_id=%d", fs.GetHead().GetRequestId())
	}
	got := util.PasreFSMessage(fs)
	ping, ok := got.(*GatePing)
	if !ok || ping.GetPayload() != "hello" {
		t.Fatalf("got %#v", got)
	}
	data, err := proto.Marshal(fs)
	if err != nil {
		t.Fatal(err)
	}
	got = util.PasreFSMessage(data)
	ping, ok = got.(*GatePing)
	if !ok || ping.GetPayload() != "hello" {
		t.Fatalf("unmarshal got %#v", got)
	}
}
