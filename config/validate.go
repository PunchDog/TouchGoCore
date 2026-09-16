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

	if c.ModelAPI != nil && !strings.EqualFold(strings.TrimSpace(c.ModelAPI.Enable), "off") {
		if len(c.ModelAPI.Providers) == 0 {
			return fmt.Errorf("model_api 已启用但 providers 为空")
		}
		if !c.ModelAPI.StrategyValid() {
			return fmt.Errorf("model_api.strategy=%s 非法（可选 %s / %s）",
				c.ModelAPI.Strategy, ModelStrategyDefault, ModelStrategyCheapest)
		}
		if ps := c.ModelAPI.PriceSource; ps != nil {
			switch s := strings.ToLower(strings.TrimSpace(ps.Provider)); s {
			case "", PriceSourceLiteLLM, PriceSourceOpenRouter, PriceSourceOff:
			default:
				return fmt.Errorf("model_api.price_source.provider=%s 非法（可选 %s / %s / %s）",
					ps.Provider, PriceSourceLiteLLM, PriceSourceOpenRouter, PriceSourceOff)
			}
			if ps.CacheTTL < 0 {
				return fmt.Errorf("model_api.price_source.cache_ttl 不能为负")
			}
			if ps.Timeout < 0 {
				return fmt.Errorf("model_api.price_source.timeout 不能为负")
			}
		}
		for name, p := range c.ModelAPI.Providers {
			if p == nil {
				return fmt.Errorf("model_api.providers[%s] 配置为空", name)
			}
			if strings.TrimSpace(p.BaseURL) == "" {
				return fmt.Errorf("model_api.providers[%s] base_url 为空", name)
			}
			if strings.TrimSpace(p.APIKey) == "" {
				return fmt.Errorf("model_api.providers[%s] api_key 为空", name)
			}
			if p.MaxConcurrency < 0 {
				return fmt.Errorf("model_api.providers[%s].max_concurrency 不能为负", name)
			}
			if p.RPMLimit < 0 {
				return fmt.Errorf("model_api.providers[%s].rpm_limit 不能为负", name)
			}
			seen := make(map[string]struct{}, len(p.Models))
			for i, m := range p.Models {
				if m == nil {
					return fmt.Errorf("model_api.providers[%s].models[%d] 配置为空", name, i)
				}
				mn := strings.TrimSpace(m.Name)
				if mn == "" {
					return fmt.Errorf("model_api.providers[%s].models[%d].name 为空", name, i)
				}
				if m.InputPrice < 0 || m.OutputPrice < 0 {
					return fmt.Errorf("model_api.providers[%s].models[%s] 价格不能为负", name, mn)
				}
				if m.MaxConcurrency < 0 {
					return fmt.Errorf("model_api.providers[%s].models[%s].max_concurrency 不能为负", name, mn)
				}
				if m.RPMLimit < 0 {
					return fmt.Errorf("model_api.providers[%s].models[%s].rpm_limit 不能为负", name, mn)
				}
				if _, dup := seen[mn]; dup {
					return fmt.Errorf("model_api.providers[%s] 模型名重复: %s", name, mn)
				}
				seen[mn] = struct{}{}
			}
			if p.DefaultModel() == "" {
				return fmt.Errorf("model_api.providers[%s] 的 model 与 models 不能同时为空", name)
			}
			if raw := strings.TrimSpace(p.Model); raw != "" && len(p.Models) > 0 {
				if _, ok := seen[raw]; !ok {
					return fmt.Errorf("model_api.providers[%s].model=%s 不存在于 models 列表", name, raw)
				}
			}
		}
		if def := strings.TrimSpace(c.ModelAPI.Default); def != "" {
			if _, ok := c.ModelAPI.Providers[def]; !ok {
				return fmt.Errorf("model_api.default=%s 不存在于 providers", def)
			}
		} else if len(c.ModelAPI.Providers) > 1 {
			return fmt.Errorf("model_api 存在多个提供方时必须指定 default")
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
