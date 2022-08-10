package main

import "touchgocore"

const (
	ServerName = "file_server"
	Version    = "1.0"
)

func init() {
}

func main() {
	touchgocore.Run(ServerName, Version)
}
