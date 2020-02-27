package util

import (
	"database/sql"
	"encoding/json"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
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
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}

	return &result
}

/*
把查询数据库的结果集转换成map
*/
func ResToMap(rows *sql.Rows) map[string]string {
	data := make(map[string]string)
	columns, err := rows.Columns()
	if err != nil {
		log.Println("获取结果集中列名数组错误:", err)
	}
	values := make([]sql.RawBytes, len(columns))
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}
	for rows.Next() {
		err = rows.Scan(scanArgs...)
		if err != nil {
			log.Println("扫描结果集中参数值错误:", err)
		}
		var value string
		for i, col := range values {
			if col == nil {
				value = "NULL"
			} else {
				value = string(col)
			}
			data[columns[i]] = value
		}

	}
	return data
}

func NextTimeZone(oldTime time.Time) time.Time {
	//往后调整8个小时//
	return time.Now().Add(time.Duration(8) * time.Hour)
}

//随机64位
func RandInt(max int64) int64 {
	if max == 0 {
		return 1
	}
	rr := rand.New(rand.NewSource(time.Now().UnixNano() * rand.Int63n(9999)))
	return rr.Int63n(max) + 1
}

//随机范围
func RandRange(min int64, max int64) int64 {
	if max < min {
		return max
	}
	rr := rand.New(rand.NewSource(time.Now().UnixNano() * rand.Int63n(9999)))
	return rr.Int63n(max-min+1) + min
}
