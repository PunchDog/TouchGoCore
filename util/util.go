package util

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"touchgocore/random"
)

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

// 全局线程安全随机数生成器（避免每次调用创建新实例）
var (
	globalRand     *random.Random
	globalRandOnce sync.Once
)

// initGlobalRand 初始化全局随机数生成器
func initGlobalRand() {
	globalRand = random.New(time.Now().UnixNano())
}

// 随机64位（优化：使用全局随机数生成器，避免每次创建 rand.Rand）
func RandInt(max int64) int64 {
	if max == 0 {
		return 0
	}
	globalRandOnce.Do(initGlobalRand)
	return globalRand.NextInt64()
}

// 随机范围（优化：使用全局随机数生成器）
func RandRange(max int64, min int64) (ret int64) {
	globalRandOnce.Do(initGlobalRand)
	if max-min == 0 {
		ret = min
	} else if max-min > 0 {
		ret = int64(globalRand.NextInt64()%(max-min) + int64(min))
	} else {
		// max-min < 0
		min = min + 1
		ret = int64(globalRand.NextInt64()%(min-max) + int64(max))
	}
	return
}

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

// getNumber 将字符串转换为数值类型（优化：使用类型断言替代反射，避免 reflect 开销）
func getNumber[T any](v string) T {
	var d T
	switch any(&d).(type) {
	case *uint:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint) = uint(num)
	case *uint8:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint8) = uint8(num)
	case *uint16:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint16) = uint16(num)
	case *uint32:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint32) = uint32(num)
	case *uint64:
		num, _ := strconv.ParseUint(v, 10, 64)
		*any(&d).(*uint64) = num
	case *int:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int) = int(num)
	case *int8:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int8) = int8(num)
	case *int16:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int16) = int16(num)
	case *int32:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int32) = int32(num)
	case *int64:
		num, _ := strconv.ParseInt(v, 10, 64)
		*any(&d).(*int64) = num
	case *float32:
		num, _ := strconv.ParseFloat(v, 32)
		*any(&d).(*float32) = float32(num)
	case *float64:
		num, _ := strconv.ParseFloat(v, 64)
		*any(&d).(*float64) = num
	}
	return d
}

// 字符串转数字数组
func String2NumberArray[T any](str string, sep string) []T {
	strs := strings.Split(str, sep)
	ret := make([]T, 0)
	if len(strs) > 0 {
		for _, str := range strs {
			ret = append(ret, getNumber[T](str))
		}
	}
	return ret
}

// IsIntranetIP 检测IP是否为内网IP（支持 IPv4 和 IPv6）
//
// IPv4 内网范围：
//   - 127.0.0.0/8 (localhost)
//   - 10.0.0.0/8
//   - 172.16.0.0/12
//   - 192.168.0.0/16
//
// IPv6 内网范围：
//   - ::1/128 (localhost)
//   - fc00::/7 (ULA, 唯一本地地址，等同 IPv4 私有地址)
//   - fe80::/10 (链路本地地址)
//   - ::ffff:0:0/96 (IPv4 映射地址，递归检查内嵌的 IPv4)
func IsIntranetIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	// 区分 IPv4 和 IPv6
	if ip4 := ip.To4(); ip4 != nil {
		return isIntranetIPv4(ip4)
	}

	// IPv6 地址（ip.To4() 返回 nil 时为纯 IPv6）
	return isIntranetIPv6(ip)
}

// isIntranetIPv4 检测 IPv4 地址是否为内网地址
func isIntranetIPv4(ip net.IP) bool {
	// 127.0.0.0/8 (localhost)
	if ip[0] == 127 {
		return true
	}

	// 10.0.0.0/8
	if ip[0] == 10 {
		return true
	}

	// 172.16.0.0/12
	if ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31 {
		return true
	}

	// 192.168.0.0/16
	if ip[0] == 192 && ip[1] == 168 {
		return true
	}

	return false
}

// isIntranetIPv6 检测 IPv6 地址是否为内网地址
func isIntranetIPv6(ip net.IP) bool {
	// 确保使用 16 字节表示
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}

	// ::1/128 — 本地回环
	if ip16.IsLoopback() {
		return true
	}

	// fc00::/7 — 唯一本地地址（ULA）
	// 前缀 fc00::/7 意味着第一个字节的最高 7 位为 1111110，即 0xfc 或 0xfd
	if ip16[0]&0xfe == 0xfc {
		return true
	}

	// fe80::/10 — 链路本地地址
	// 前缀 fe80::/10 意味着前 10 位为 1111111010，即第一字节 0xfe，第二字节高两位 10
	if ip16[0] == 0xfe && ip16[1]&0xc0 == 0x80 {
		return true
	}

	// ::ffff:0:0/96 — IPv4 映射地址，递归检查内嵌的 IPv4
	// 格式：前 80 位为 0，接下来 16 位为 0xffff，最后 32 位为 IPv4 地址
	if ip16[0] == 0 && ip16[1] == 0 && ip16[2] == 0 && ip16[3] == 0 &&
		ip16[4] == 0 && ip16[5] == 0 && ip16[6] == 0 && ip16[7] == 0 &&
		ip16[8] == 0 && ip16[9] == 0 && ip16[10] == 0xff && ip16[11] == 0xff {
		return isIntranetIPv4(ip16[12:16])
	}

	return false
}
