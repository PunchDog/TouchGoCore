package mapmanager

import (
	"touchgocore/config"
	"touchgocore/util"
)

func Run() {
	if config.Cfg_.MapPath == "off" || config.Cfg_.MapPath == "" {
		return
	}

	_, _ = util.DefaultCallFunc.Do("RunMap")
}
