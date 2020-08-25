package util

import (
	"crypto/md5"
	"encoding/hex"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/PunchDog/TouchGoCore/touchgocore/jsonthr"
)

/*
获取IP
*/
func GetIp(r *http.Request) string {
	ip := net.ParseIP(strings.Split(r.RemoteAddr, ":")[0]).String()
	if ip == "<nil>" {
		ip = "127.0.0.1"
	}
	return ip
}

//判断是否是公网ip
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

//通过淘宝接口根据公网ip获取国家运营商等信息
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

func NextTimeZone(oldTime time.Time) time.Time {
	//往后调整8个小时//
	return time.Now().Add(time.Duration(8) * time.Hour)
}

//随机64位
func RandInt(max int64) int64 {
	if max == 0 {
		return 0
	}
	rr := rand.New(rand.NewSource(time.Now().UnixNano() * rand.Int63n(9999)))
	return rr.Int63n(max)
}

//随机范围
func RandRange(min int64, max int64) int64 {
	if max < min {
		return max
	}
	rr := rand.New(rand.NewSource(time.Now().UnixNano() * rand.Int63n(9999)))
	return rr.Int63n(max-min+1) + min
}

// 生成时间戳的函数
func UTCTime_TouchGoCore() string {
	t := time.Now()
	return strconv.FormatInt(t.UTC().UnixNano(), 10)
}

// MD5 实现 :主要是针对 字符串的加密
func MD5_TouchGoCore(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func GetTime_TouchGoCore() string {
	const shortForm = "2006-01-02 15:04:05"
	t := time.Now()
	temp := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	str := temp.Format(shortForm)
	return str
}

func GetNowtimeMD5_TouchGoCore() string {
	t := time.Now()
	timestamp := strconv.FormatInt(t.UTC().UnixNano(), 10)
	return MD5_TouchGoCore(timestamp)
}

//获取类名
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

//检查端口占用
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
