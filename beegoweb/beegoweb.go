package beegoweb

import (
	"touchgocore/config"

	"github.com/astaxie/beego"
)

func init() {
}

func Run() {
	if config.Cfg_.BeegoWeb == nil {
		return
	}

	beego.BConfig.WebConfig.ViewsPath = config.GetBasePath() + "/" + config.Cfg_.BeegoWeb.ViewsPath
	beego.BConfig.Listen.HTTPPort = config.Cfg_.BeegoWeb.HTTPPort

	//开启静态资源,提供下载服务
	staticDir := config.Cfg_.BeegoWeb.Static
	if staticDir[0] == '/' && config.PathExists(staticDir) {
		beego.SetStaticPath("/state", staticDir)
	} else {
		beego.SetStaticPath("/state", config.GetBasePath()+"/"+staticDir)
	}
}
