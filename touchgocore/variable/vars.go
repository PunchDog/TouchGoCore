package variable

import (
	"fmt"
	"io"
	"log"
	"runtime"
	"strconv"
	"strings"
)

var (
	Log SLoger
)

type logcall struct {
	fn  func(v ...interface{})
	log interface{}
}

type SLoger struct {
	log  *log.Logger
	isIO bool
}

func (this *SLoger) IsIO(is bool) {
	this.isIO = is
}

func (this *SLoger) New(out io.Writer, prefix string, flag int, isio bool) {
	this.log = log.New(out, "", flag)
	this.isIO = isio
}

func (this *SLoger) GetFile() string {
	_, file, line, _ := runtime.Caller(2)
	file = file[strings.LastIndex(file, "/")+1:]
	str := file + ":" + strconv.Itoa(line) + ":"
	return str
}

func (this *SLoger) Println(v ...interface{}) {
	if !this.isIO {
		log.Println(v...)
		return
	}

	list := [](interface{}){}
	list = append(list, this.GetFile())
	list = append(list, v...)
	this.log.Println(list...)
}

func (this *SLoger) PrintlnShow(v ...interface{}) {
	this.log.Println(v...)
}

func (this *SLoger) Printf(format string, v ...interface{}) {
	if !this.isIO {
		log.Printf(format, v...)
		return
	}
	format = this.GetFile() + format
	this.log.Println(fmt.Sprintf(format, v...))
}

func (this *SLoger) Fatalf(format string, v ...interface{}) {
	if !this.isIO {
		return
	}
	format = this.GetFile() + format
	this.log.Println(fmt.Sprintf(format, v...))
}

func (this *SLoger) Fatal(v ...interface{}) {
	if !this.isIO {
		return
	}
	list := [](interface{}){}
	list = append(list, this.GetFile())
	list = append(list, v...)
	this.log.Println(list...)
}
