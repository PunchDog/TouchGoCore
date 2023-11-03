package beegoweb

import (
	"touchgocore/config"
	"touchgocore/vars"

	"github.com/astaxie/beego"
)

func init() {
}

func Run() {
	if config.Cfg_.BeegoWeb == nil {
		vars.Info("不启动BeegoWeb服务")
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
	vars.Info("启动BeegoWeb服务成功")
}
