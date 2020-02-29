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

func (this *SLoger) println(level int, format string, v ...interface{}) {
	format = this.getFile() + format
	str := fmt.Sprintf(format, v...)
	if level >= loglevelmap[log_.logLevel] {
		this.log.Println(str)
	}
	log.Println(str)
}

func Info(format string, v ...interface{}) {
	log_.println(LogLevel_Info, format, v...)
}

func Debug(format string, v ...interface{}) {
	log_.println(LogLevel_Debug, format, v...)
}

func Error(format string, v ...interface{}) {
	log_.println(LogLevel_Error, format, v...)
}
