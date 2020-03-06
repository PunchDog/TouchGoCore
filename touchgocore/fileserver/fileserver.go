package fileserver

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/vars"
	"github.com/glog-master"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var mux map[string]func(http.ResponseWriter, *http.Request)

type Myhandler struct{}
type home struct {
	Title string
}

const (
	Template_Dir = "./view/"
	Upload_Dir   = "./upload/"
)

//func main() {
//	server := http.Server{
//		Addr:        ":9090",
//		Handler:     &Myhandler{},
//		ReadTimeout: 10 * time.Second,
//	}
//	mux = make(map[string]func(http.ResponseWriter, *http.Request))
//	mux["/"] = index
//	mux["/upload"] = upload
//	mux["/file"] = StaticServer
//	go server.ListenAndServe()
//
//	//启动核心代码
//	touchgocore.Run(ServerName, Version)
//	chSig := make(chan byte)
//	<-chSig
//}

func Run() {
	if config.Cfg_.File == "off" {
		glog.Info("不启动文件服务")
		return
	}

	server := http.Server{
		Addr:        ":" + config.Cfg_.File,
		Handler:     &Myhandler{},
		ReadTimeout: 10 * time.Second,
	}
	mux = make(map[string]func(http.ResponseWriter, *http.Request))
	mux["/"] = index
	mux["/upload"] = upload
	mux["/file"] = StaticServer
	go server.ListenAndServe()
	vars.Info("文件服务启动成功")
}

func (*Myhandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h, ok := mux[r.URL.String()]; ok {
		h(w, r)
		return
	}
	http.StripPrefix("/", http.FileServer(http.Dir("./upload/"))).ServeHTTP(w, r)
}

func upload(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		t, _ := template.ParseFiles(Template_Dir + "file.html")
		t.Execute(w, "上传文件")
	} else {
		r.ParseMultipartForm(32 << 20)
		file, handler, err := r.FormFile("uploadfile")
		if err != nil {
			fmt.Fprintf(w, "%v", "上传错误")
			return
		}
		fileext := filepath.Ext(handler.Filename)
		if check(fileext) == false {
			fmt.Fprintf(w, "%v", "不允许的上传类型")
			return
		}
		// filename := strconv.FormatInt(time.Now().Unix(), 10) + fileext
		filename := handler.Filename
		f, _ := os.OpenFile(Upload_Dir+filename, os.O_CREATE|os.O_WRONLY, 0660)
		_, err = io.Copy(f, file)
		if err != nil {
			fmt.Fprintf(w, "%v", "上传失败")
			return
		}
		filedir, _ := filepath.Abs(Upload_Dir + filename)
		fmt.Fprintf(w, "%v", filename+"上传完成,服务器地址:"+filedir)
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	title := home{Title: "首页"}
	t, _ := template.ParseFiles(Template_Dir + "index.html")
	t.Execute(w, title)
}

func StaticServer(w http.ResponseWriter, r *http.Request) {
	http.StripPrefix("/file", http.FileServer(http.Dir("./upload/"))).ServeHTTP(w, r)
}

func check(name string) bool {
	ext := []string{".exe", ".js", ".png"}

	for _, v := range ext {
		if v == name {
			return false
		}
	}
	return true
}
