package util

import (
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestParseFSMessageRejectsUnregistered(t *testing.T) {
	fs := NewFSMessageWithID(91, 92, 1, wrapperspb.String("nope"))
	if fs == nil {
		t.Fatal("pack failed")
	}
	if got := ParseFSMessage(fs); got != nil {
		t.Fatalf("unregistered protocol should be rejected, got %T", got)
	}
}

func TestParseFSMessageRegisteredRoundTrip(t *testing.T) {
	RegisterProtocolType(81, 82, wrapperspb.String(""))
	fs := NewFSMessageWithID(81, 82, 7, wrapperspb.String("hi"))
	got := ParseFSMessage(fs)
	sv, ok := got.(*wrapperspb.StringValue)
	if !ok || sv.GetValue() != "hi" {
		t.Fatalf("got %#v", got)
	}
	if fs.GetHead().GetRequestId() != 7 {
		t.Fatalf("request_id=%d", fs.GetHead().GetRequestId())
	}
}
