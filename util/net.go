package util

import (
	"net"
	"time"
)

type IPInfo struct {
	Code int    `json:"code"`
	Data IPData `json:"data"`
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
