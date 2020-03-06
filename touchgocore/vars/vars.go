package vars

import (
	"fmt"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	log_ SLoger
)

const (
	LogLevel_Off = iota
	LogLevel_Info
	LogLevel_Debug
	LogLevel_Error
)

var loglevelmap map[string]int = map[string]int{
	"off":   LogLevel_Off,
	"info":  LogLevel_Info,
	"debug": LogLevel_Debug,
	"error": LogLevel_Error,
}

func Run(servername string, level string) {
	writers := []io.Writer{
		&lumberjack.Logger{
			Filename:   servername + ".log",
			MaxSize:    500, // megabytes
			MaxBackups: 10,
			MaxAge:     28, //days
		},
		os.Stdout,
	}

	fileAndStdoutWriter := io.MultiWriter(writers...)
	log_.new(fileAndStdoutWriter, "", log.Ldate|log.Lmicroseconds|log.Lshortfile, level)
	go func() {
		//启动日志打印线程
		lock := sync.Mutex{}
		for {
			list := []func(){}
			lock.Lock()
			list = append(list, log_.printList...)
			log_.printList = nil
			lock.Unlock()
			for _, fn := range list {
				fn()
			}
			time.Sleep(time.Millisecond * 10)
		}
	}()
	Info("初始化日志模块完成！")
}

type SLoger struct {
	log       *log.Logger
	logLevel  string
	printList []func()
}

func (this *SLoger) new(out io.Writer, prefix string, flag int, logLevel string) {
	this.log = log.New(out, "", flag)
	this.logLevel = logLevel
}

func (this *SLoger) getFile() string {
	_, file, line, _ := runtime.Caller(2)
	file = file[strings.LastIndex(file, "/")+1:]
	str := file + ":" + strconv.Itoa(line) + ":"
	return str
}

func (this *SLoger) println(file string, level int, v ...interface{}) {
	var format interface{}
	//识别是不是格式化类型
	switch v[0].(type) {
	case string:
		str := v[0].(string)
		str = file + str
		if n := strings.Index(str, "%"); n > -1 {
			format = fmt.Sprintf(str, v[1:]...)
		} else {
			list := []interface{}(v)
			list[0] = str
			format = list
		}
	default:
		list := []interface{}{}
		list = append(list, file)
		list = append(list, v...)
		format = list
	}

	if level >= loglevelmap[log_.logLevel] {
		this.log.Println(format)
	} else {
		log.Println(format)
	}
}

func Info(v ...interface{}) {
	file := log_.getFile()
	log_.printList = append(log_.printList, func() {
		log_.println("【INFO】"+file, LogLevel_Info, v...)
	})
}

func Debug(v ...interface{}) {
	file := log_.getFile()
	log_.printList = append(log_.printList, func() {
		log_.println("【DEBUG】"+file, LogLevel_Debug, v...)
	})
}

func Error(v ...interface{}) {
	file := log_.getFile()
	log_.printList = append(log_.printList, func() {
		log_.println("【ERROR】"+file, LogLevel_Error, v...)
	})
}
