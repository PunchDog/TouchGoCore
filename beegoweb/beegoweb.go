package beegoweb

import (
	"path"
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

	beego.BConfig.WebConfig.ViewsPath = path.Join(config.GetBasePath(), config.Cfg_.BeegoWeb.ViewsPath)
	beego.BConfig.Listen.HTTPPort = config.Cfg_.BeegoWeb.HTTPPort

	//开启静态资源,提供下载服务
	staticDir := config.Cfg_.BeegoWeb.Static
	if staticDir[0] == '/' && config.PathExists(staticDir) {
		beego.SetStaticPath("/static", staticDir)
	} else {
		beego.SetStaticPath("/static", path.Join(config.GetBasePath(), staticDir))
	}
	vars.Info("启动BeegoWeb服务成功")
}
