package mapmanager

import (
	"touchgocore/config"
	"touchgocore/util"
)

func Run() {
	if config.Cfg_.MapPath == "off" || config.Cfg_.MapPath == "" {
		return
	}

	util.DefaultCallFunc.Do("RunMap")
}
