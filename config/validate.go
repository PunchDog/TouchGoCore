package config

import (
	"fmt"
	"strings"
)

// Validate 检查启动配置的必填项与明显冲突。
// 在 NewApp 加载配置后调用；失败应阻止启动。
func (c *Cfg) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	c.Normalize()

	ports := map[int]string{}
	addPort := func(port int, name string) error {
		if port <= 0 {
			return nil
		}
		if prev, ok := ports[port]; ok {
			return fmt.Errorf("端口冲突: %d 同时被 %s 与 %s 使用", port, prev, name)
		}
		ports[port] = name
		return nil
	}

	if c.Web != nil {
		if err := addPort(c.Web.HTTPPort, "web.httpport"); err != nil {
			return err
		}
		if err := validateTLS("web.tls", c.Web.TLS); err != nil {
			return err
		}
	}

	if c.Ws != nil {
		for i, p := range c.Ws.Port {
			if p == nil {
				continue
			}
			if err := addPort(p.Port, fmt.Sprintf("ws.port[%d]", i)); err != nil {
				return err
			}
		}
		if err := validateTLS("ws.tls", c.Ws.TLS); err != nil {
			return err
		}
		if c.Ws.CheckOrigin && len(c.Ws.AllowedOrigins) == 0 {
			return fmt.Errorf("ws.check_origin 为 true 但 allowed_origins 为空，连接会被全部拒绝")
		}
	}

	if c.Metrics != nil && c.Metrics.Enabled {
		port := c.Metrics.Port
		if port == 0 {
			port = 9090
		}
		if err := addPort(port, "metrics.port"); err != nil {
			return err
		}
	}

	rpc := c.RpcOf()
	if rpc != nil {
		for i, s := range rpc.Server {
			if s == nil {
				continue
			}
			if err := addPort(s.Port, fmt.Sprintf("rpc.server[%d]", i)); err != nil {
				return err
			}
		}
		if rpc.TLS != nil && rpc.TLS.Enable {
			if err := filesExist("rpc.tls", rpc.TLS.CertFile, rpc.TLS.KeyFile); err != nil {
				return err
			}
		}
		if err := validateRpcAuth(rpc.Auth); err != nil {
			return err
		}
	}

	return nil
}

func validateTLS(prefix string, tls *TLSConfig) error {
	if tls == nil || !tls.Enable {
		return nil
	}
	return filesExist(prefix, tls.CertFile, tls.KeyFile)
}

func filesExist(prefix, cert, key string) error {
	if strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("%s 已启用但 cert_file/key_file 为空", prefix)
	}
	if !PathExists(cert) {
		return fmt.Errorf("%s 证书不存在: %s", prefix, cert)
	}
	if !PathExists(key) {
		return fmt.Errorf("%s 私钥不存在: %s", prefix, key)
	}
	return nil
}

func validateRpcAuth(auth *RpcAuthConfig) error {
	if auth == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(auth.Mode))
	if mode == "" {
		mode = "none"
	}
	switch mode {
	case "none":
		return nil
	case "allowlist":
		if len(auth.AllowList) == 0 {
			return fmt.Errorf("rpc.auth.mode=allowlist 但 allowlist 为空")
		}
	case "token":
		if strings.TrimSpace(auth.Token) == "" {
			return fmt.Errorf("rpc.auth.mode=token 但 token 为空")
		}
	case "mtls":
		if strings.TrimSpace(auth.CAFile) == "" {
			return fmt.Errorf("rpc.auth.mode=mtls 但 ca_file 为空")
		}
		if !PathExists(auth.CAFile) {
			return fmt.Errorf("rpc.auth.ca_file 不存在: %s", auth.CAFile)
		}
	default:
		return fmt.Errorf("rpc.auth.mode 无效: %s（支持 none/allowlist/token/mtls）", auth.Mode)
	}
	return nil
}
