package main

import (
	"github.com/PunchDog/TouchGoCore/example/fileserver/rpcptoto"
	"github.com/PunchDog/TouchGoCore/touchgocore"
	"github.com/PunchDog/TouchGoCore/touchgocore/rpc"
)

const (
	ServerName = "file_server"
	Version    = "1.0"
)

func init() {
	rpc.AddServerListen(new(rpcptoto.RegisterFunc))
}

func main() {
	touchgocore.Run(ServerName, Version)
	chSig := make(chan byte, 1)
	<-chSig
}
