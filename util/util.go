package util

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"touchgocore/jsonthr"
	"touchgocore/vars"
)

// 获取本地内网地址。
func GetLocalInternalIp() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		fmt.Println(err)
		return "", err
	}
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	fmt.Println(localAddr.String())
	ip := strings.Split(localAddr.String(), ":")[0]
	return ip, nil
}

// 获取本地外网地址。
func GetLocalExternalIp() (string, error) {
	resp, e := http.Get("http://myexternalip.com/raw")
	if e != nil {
		return "127.0.0.1", e
	}
	defer resp.Body.Close()

	result, e := ioutil.ReadAll(resp.Body)
	if e != nil {
		return "127.0.0.1", e
	}
	reg := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	return reg.FindString(string(result)), nil
}

// 判断是否是公网ip
func IsPublicIP(IP net.IP) bool {
	if IP.IsLoopback() || IP.IsLinkLocalMulticast() || IP.IsLinkLocalUnicast() {
		return false
	}
	if ip4 := IP.To4(); ip4 != nil {
		switch true {
		case ip4[0] == 10:
			return false
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return false
		case ip4[0] == 192 && ip4[1] == 168:
			return false
		default:
			return true
		}
	}
	return false
}

type IPInfo struct {
	Code int    `json:"code"`
	Data IPData `json:"data`
}
type IPData struct {
	Country   string `json:"country"`
	CountryId string `json:"country_id"`
	Area      string `json:"area"`
	AreaId    string `json:"area_id"`
	Region    string `json:"region"`
	RegionId  string `json:"region_id"`
	City      string `json:"city"`
	CityId    string `json:"city_id"`
	Isp       string `json:"isp"`
}

// 通过淘宝接口根据公网ip获取国家运营商等信息
func TabaoIpAPI(ip string) *IPInfo {
	url := "http://ip.taobao.com/service/getIpInfo.php?ip="
	url += ip

	resp, err := http.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	out, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var result IPInfo
	if err := jsonthr.Json.Unmarshal(out, &result); err != nil {
		return nil
	}

	return &result
}

// 随机64位
func RandInt(max int64) int64 {
	if max == 0 {
		return 0
	}
	rr := rand.New(rand.NewSource(time.Now().UnixNano() * rand.Int63n(9999)))
	return rr.Int63n(max)
}

// 随机范围
func RandRange(max int64, min int64) (ret int64) {
	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	if max-min == 0 {
		ret = min
	} else if max-min > 0 {
		ret = int64(random.Intn(int(max-min)) + int(min))
	} else {
		// max-min < 0
		min = min + 1
		ret = int64(random.Intn(int(min-max)) + int(max))
	}
	return
}

// 时间类型///////////////////////////////////////////////////////////////////////////////////
// 返回某个时间的时间戳
func MS(t time.Time) int64 {
	return int64(t.UnixNano() / NANO_TO_MS)
}

// 返回unix时间戳。
func CurrentMS() int64 {
	return int64(time.Now().UnixNano() / NANO_TO_MS)
}

// 返回unix时间戳。
func CurrentS() int64 {
	return int64(time.Now().UnixNano() / NANO_TO_MS / MILLISECONDS_OF_SECOND)
}

// 毫秒转时间
func Ms2Time(ms int64) time.Time {
	sec := ms / MILLISECONDS_OF_SECOND
	nsec := (ms % MILLISECONDS_OF_SECOND) * NANO_TO_MS
	return time.Unix(sec, nsec).UTC()
}

// 秒转时间
func S2Time(sec int64) time.Time {
	return time.Unix(sec, 0).UTC()
}

// 毫秒转时间字符串
func Ms2StrTime(ms int64) string {
	msTime := Ms2Time(ms)
	return msTime.Format("2006-01-02 15:04:05")
}

// 秒转时间字符串
func S2StrTime(sec int64) string {
	msTime := S2Time(sec)
	return msTime.Format("2006-01-02 15:04:05")
}

// 这个时间对应的0点
func Time2Midnight(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// 从一个毫秒时间戳获得当前时区的本日凌晨时间。
func Ms2Midnight(t int64) time.Time {
	midTime := Time2Midnight(time.Unix(t/1000, t%1000))
	return midTime
}

// 今天0点时间戳
func CurMidnight() int64 {
	return MS(Time2Midnight(time.Now()))
}

// 字符串时间转换时间戳 date format: "2006-01-02 13:04:00"
func S2UnixTime(value string) int64 {
	re := regexp.MustCompile(`([\d]+)-([\d]+)-([\d]+) ([\d]+):([\d]+):([\d]+)`)
	slices := re.FindStringSubmatch(value)
	if slices == nil || len(slices) != 7 {
		vars.Error("time[%s] format error, expect format: 2006-01-02 13:04:00...", value)
		return 0
	}
	year, _ := strconv.Atoi(slices[1])
	month, _ := strconv.Atoi(slices[2])
	day, _ := strconv.Atoi(slices[3])
	hour, _ := strconv.Atoi(slices[4])
	min, _ := strconv.Atoi(slices[5])
	sec, _ := strconv.Atoi(slices[6])
	loc, _ := time.LoadLocation("UTC") // use UTC instend of Local
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, loc)
	return int64(t.UnixNano() / NANO_TO_MS)
}

// 下一个0点
func NextMidnight(t int64) int64 {
	midTime := Time2Midnight(time.Unix(t/1000, t%1000))
	return midTime.UnixNano()/NANO_TO_MS + MILLISECONDS_OF_DAY
}

// 从一个毫秒时间戳获取下一个准点时间。
func NextHour(t int64) int64 {
	t1 := time.Unix(t/1000, t%1000)
	year, month, day := t1.Date()
	hour, _, _ := t1.Clock()
	t2 := time.Date(year, month, day, hour+1, 0, 0, 0, t1.Location())
	return t2.UnixNano() / 1e6
}

// 同一个星期
func InSameWeek(t1, t2 int64) bool {
	if t1 == 0 || t2 == 0 {
		return false
	}
	y1, w1 := time.Unix(t1, 0).ISOWeek()
	y2, w2 := time.Unix(t2, 0).ISOWeek()
	return y1 == y2 && w1 == w2
}

// 同一个月
func InSameMonth(t1, t2 int64) bool {
	if t1 == 0 || t2 == 0 {
		return false
	}
	y1, m1, _ := time.Unix(t1, 0).Date()
	y2, m2, _ := time.Unix(t2, 0).Date()
	return y1 == y2 && m1 == m2
}

// 是否在同一天
func GetDiffDay(day1 int64, day2 int64) int {
	return int((day2 - day1) / 86400)
}

///////////////////////////////////////////////////////////////////////////////////////////////////

// MD5 实现 :主要是针对 字符串的加密
func MD5(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// 获取类名
func GetClassName(p interface{}) (string, []string) {
	//神奇的获取类名
	tpy := reflect.TypeOf(p)
	rcvr := reflect.ValueOf(p)
	sname := reflect.Indirect(rcvr).Type().Name()
	methods := []string{}
	for m := 0; m < tpy.NumMethod(); m++ {
		method := tpy.Method(m)
		methods = append(methods, method.Name)
	}
	return sname, methods
}

// 检查端口占用
func CheckPort(port string) (err error) {
	tcpAddress, err := net.ResolveTCPAddr("tcp4", ":"+port)
	if err != nil {
		return err
	}

	for i := 0; i < 3; i++ {
		listener, err := net.ListenTCP("tcp", tcpAddress)
		if err != nil {
			time.Sleep(time.Duration(100) * time.Millisecond)
		} else {
			listener.Close()
			break
		}
	}

	return nil
}

// 获取路径下文件列表
func GetPathFile(path string, filter []string) []string {
	// 判断所给路径是否为文件夹
	isDir := func(path string) bool {
		s, err := os.Stat(path)
		if err != nil {
			return false
		}
		return s.IsDir()
	}

	//获取当前目录下的文件或目录名(包含路径)
	pathlen := len(path)
	if path[pathlen-1] != '/' {
		path = path + "/"
	}
	//获取当前目录下的文件或目录名(包含路径)
	filepathNames, err := filepath.Glob(path + "*")
	if err != nil {
		panic(err)
	}

	strRetList := []string{}

	//遍历路径,但是会给文件夹优先级放后
	for _, filenamepath := range filepathNames {
		if isDir(filenamepath) {
			list := GetPathFile(filenamepath, filter)
			if len(list) > 0 {
				strRetList = append(strRetList, list...)
			}
		} else {
			//过滤带关键字的
			if filter != nil {
				bContinue := false
				for _, f := range filter {
					if !strings.Contains(filenamepath, f) {
						bContinue = true
						break
					}
				}
				if !bContinue {
					strRetList = append(strRetList, filenamepath)
				}
			} else {
				strRetList = append(strRetList, filenamepath)
			}
		}
	}

	return strRetList
}

func formatMapKey(values []reflect.Value) string {
	report := ""
	v := values
	if len(values) > 64 {
		v = values[:64]
	}

	for _, v := range v {
		if v.CanInterface() {
			report += fmt.Sprintf("%v, ", v.Interface())
		} else if v.Kind() == reflect.Ptr {
			e := v.Elem()
			if e.CanInterface() {
				report += fmt.Sprintf("%v, ", e.Interface())
			} else {
				report += fmt.Sprintf("NO SUPPORT, ")
			}
		}
	}

	if len(values) > 64 {
		report += "..."
	}

	return report
}

func formatStruct(s reflect.Value, deep int16) string {
	var report string
	if s.Kind() == reflect.Interface {
		s = s.Elem()
	}
	if s.Kind() == reflect.Ptr {
		s = s.Elem()
	}

	prefix := ""
	for strdeep := deep; strdeep >= 0; strdeep-- {
		prefix += "\t"
	}

	typeOfT := s.Type()
	if s.Kind() == reflect.Struct {
		for i := 0; i < s.NumField(); i++ {
			f := s.Field(i)
			if f.Kind() == reflect.Map {
				report += fmt.Sprintf("%s%s keys: {%v}\n", prefix,
					typeOfT.Field(i).Name, formatMapKey(f.MapKeys()))
			} else if (f.Kind() == reflect.Slice) || (f.Kind() == reflect.Array) {
				report += fmt.Sprintf("%s%s len: %d\n", prefix,
					typeOfT.Field(i).Name, f.Len())
			} else if f.Kind() == reflect.Struct {
				if deep > 1 {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, f.Interface())
				} else {
					report += fmt.Sprintf("%s%s:\n", prefix, typeOfT.Field(i).Name)
					report += formatStruct(f, deep+1)
				}
			} else if f.Kind() == reflect.Interface {
				if deep > 1 {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, f.Interface())
				} else {
					report += fmt.Sprintf("%s%s:\n", prefix, typeOfT.Field(i).Name)
					report += formatStruct(f, deep+1)
				}
			} else if f.CanInterface() {
				report += fmt.Sprintf("%s%s=%v\n", prefix,
					typeOfT.Field(i).Name, f.Interface())
			} else if f.Kind() == reflect.Ptr {
				e := f.Elem()
				if f.CanInterface() {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, e.Interface())
				} else {
					report += fmt.Sprintf("%s%s=NO SUPPORT\n", prefix,
						typeOfT.Field(i).Name)
				}
			}
		}
	} else {
		report += fmt.Sprintf("%s%s=%v\n", prefix,
			typeOfT.Name(), s.Interface())
	}
	return report
}

func FormatStruct(obj interface{}) string {
	return formatStruct(reflect.ValueOf(obj), 0)
}

// 定义数值类型约束（包含所有整型和浮点型）
type Numeric interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// 数字类排序，从小到大
type NumberSortLess[T Numeric] []T

func (this NumberSortLess[T]) Len() int {
	return len(this)
}
func (this NumberSortLess[T]) Less(i, j int) bool {
	return this[i] < this[j] // 直接比较数值
}
func (this NumberSortLess[T]) Swap(i, j int) {
	this[i], this[j] = this[j], this[i]
}

// 数字类排序，从大到小
type NumberSortDesc[T Numeric] []T

func (this NumberSortDesc[T]) Len() int {
	return len(this)
}
func (this NumberSortDesc[T]) Less(i, j int) bool {
	return this[i] > this[j] // 直接比较数值
}
func (this NumberSortDesc[T]) Swap(i, j int) {
	this[i], this[j] = this[j], this[i]
}

func getNumber[T any](v string) T {
	var d T
	switch any(d).(type) {
	case uint8, uint16, uint32, uint64, int, int8, int16, int32, int64:
		num, _ := strconv.ParseInt(v, 10, 64)
		val := reflect.ValueOf(num).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val)
	case float32, float64:
		num, _ := strconv.ParseFloat(v, 10)
		val := reflect.ValueOf(num).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val)
	}
	return d
}

// 字符串转数字数组
func String2NumberArray[T any](str string, sep string) []T {
	strs := strings.Split(str, ",")
	ret := make([]T, 0)
	if len(strs) > 0 {
		for _, str := range strs {
			ret = append(ret, getNumber[T](str))
		}
	}
	return ret
}
