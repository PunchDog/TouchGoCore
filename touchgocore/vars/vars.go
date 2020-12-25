package vars

import (
	"fmt"
	"github.com/natefinch/lumberjack"
	"io"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
)

var (
	log_ SLoger
)

const (
	LogLevel_Off = iota
	LogLevel_Error
	LogLevel_Debug
	LogLevel_Info
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
			MaxAge:     30, //days
		},
		os.Stdout,
	}

	fileAndStdoutWriter := io.MultiWriter(writers...)
	log_.new(fileAndStdoutWriter, log.Ldate|log.Lmicroseconds|log.Lshortfile, level)
	go func() {
		//启动日志打印线程
		for {
			select {
			case fn := <-log_.printList:
				fn()
			}
		}
	}()
	Info("初始化日志模块完成！")
}

type SLoger struct {
	log       *log.Logger
	logLevel  string
	printList chan func()
}

func (this *SLoger) new(out io.Writer, flag int, logLevel string) {
	this.log = log.New(out, "", flag)
	this.logLevel = logLevel
	this.printList = make(chan func(), 10000)
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

	if level <= loglevelmap[log_.logLevel] {
		this.log.Println(format)
	} else {
		log.Println(format)
	}
}

func Info(v ...interface{}) {
	file := log_.getFile()
	log_.printList <- func() {
		log_.println("【INFO】"+file, LogLevel_Info, v...)
	}
}

func Debug(v ...interface{}) {
	file := log_.getFile()
	log_.printList <- func() {
		log_.println("【INFO】"+file, LogLevel_Debug, v...)
	}
}

func Error(v ...interface{}) {
	file := log_.getFile()
	log_.printList <- func() {
		log_.println("【INFO】"+file, LogLevel_Error, v...)
	}
}
