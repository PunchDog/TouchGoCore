package main

import (
	"touchgocore"
	"touchgocore/example/dbserver/rpcproto"
	"touchgocore/rpc"
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
