package main

import (
	"github.com/PunchDog/TouchGoCore/example/dbserver/rpcproto"
	"github.com/PunchDog/TouchGoCore/touchgocore"
	"github.com/PunchDog/TouchGoCore/touchgocore/rpc"
)

const (
	ServerName = "DBServer"
	Version    = "1.0"
)

func init() {
	rpc.AddServerListen(new(rpcproto.RegisterFunc))
}

func main() {
	touchgocore.Run(ServerName, Version)
}
