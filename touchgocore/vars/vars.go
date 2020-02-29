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
)

var (
	log_ SLoger
)

const (
	LogLevel_Info = iota
	LogLevel_Debug
	LogLevel_Error
)

var loglevelmap map[string]int = map[string]int{
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
}

type SLoger struct {
	log      *log.Logger
	logLevel string
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

func (this *SLoger) println(level int, v ...interface{}) {
	var format interface{}
	//识别是不是格式化类型
	switch v[0].(type) {
	case string:
		str := v[0].(string)
		str = this.getFile() + str
		if strings.Index(str, "%") > 0 {
			format = fmt.Sprintf(str, v[1:]...)
		} else {
			format = fmt.Sprintln(v...)
		}
	default:
		format = v[:]
	}

	if level >= loglevelmap[log_.logLevel] {
	}
	log.Println(format)
}

func Info(v ...interface{}) {
	log_.println(LogLevel_Info, v...)
}

func Debug(v ...interface{}) {
	log_.println(LogLevel_Debug, v...)
}

func Error(v ...interface{}) {
	log_.println(LogLevel_Error, v...)
}
