package rpc

import (
	"sync"
)

type RpcServer struct {
	sync.RWMutex
}

func Ping(args *RpcRequest, reply *RpcResponse) error {
	return nil
}
