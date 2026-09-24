package config

import (
	"fmt"
	"sort"
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

	err := validatePayChannels(c)
	if err != nil {
		return err
	}

	if err := validateCache(c); err != nil {
		return err
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

// validatePayChannels 校验资金通道的 SDK 引用是否指得通。
//
// 这里拦的是「配置写错」而不是「供应商挂了」：sdk 名字打错一个字母、引用了
// 不存在的商户账户、SDK 段没标驱动，都是部署期就能确定的错，让它启动只会在
// 第一笔出款时暴露。至于驱动名有没有登记，由通道包在 Start 时用 pay.Open 判定
// （本包不引 pay，避免配置层依赖资金契约层）。
//
// 判定范围只有「已经开启的部分」：enable 不为 on 的段整体跳过——把一条通道关掉
// 不该同时要求它的每一项配置都仍然完整。唯一的例外是段名本身指不到：那是纯粹的
// 拼写错，跟开没开无关，趁启动报出来比留到某天打开时炸掉省事。
func validatePayChannels(c *Cfg) error {
	refs := map[string]*PaySDKRef{}
	// service 标记这条引用只做验证码下发/登录换会话，不动资金：
	// 这样的 SDK 本就没有商户账户，按资金链路口径要求它等于逼配置里填个假商户号。
	service := map[string]bool{"whatsapp.login": true}
	if c.Telegram != nil {
		refs["telegram.ton"] = c.Telegram.Ton
	}
	if c.Usdt != nil {
		refs["usdt.provider"] = c.Usdt.Provider
	}
	if c.Whatsapp != nil {
		refs["whatsapp.login"] = c.Whatsapp.Login
		refs["whatsapp.provider"] = c.Whatsapp.Provider
	}
	for _, name := range sortedMapKeys(refs) {
		ref := refs[name]
		if ref.Empty() {
			continue // 没引用即不启用，属正常缺省
		}
		section := c.PaySDks[strings.TrimSpace(ref.SDK)]
		if section == nil {
			// 段名打错一个字母是纯配置错，无论开没开都要当场报出来：
			// 它不会因为「这条通道还没启用」就变正确，只会在某天真打开时炸成启动失败。
			return fmt.Errorf("%s: pay_sdks 里没有名为 %s 的 SDK 段", name, strings.TrimSpace(ref.SDK))
		}
		if !section.Enabled() {
			continue // 段没开＝这条引用当前不参与判定，属合法的「写好待开」
		}
		_, _, err := ref.Resolve(c.PaySDks)
		if service[name] {
			_, _, err = ref.ResolveService(c.PaySDks)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, name := range sortedMapKeys(c.PaySDks) {
		sdk := c.PaySDks[name]
		if !sdk.Enabled() {
			// enable 不为 on 的段整体不参与判定：关掉一条通道不该同时要求
			// 它的每一项配置都仍然完整。
			continue
		}
		if strings.TrimSpace(sdk.Driver) == "" {
			return fmt.Errorf("pay_sdks.%s 缺 sdk 驱动标记（应为 pay 包登记过的驱动名）", name)
		}
		if strings.TrimSpace(sdk.BaseURL) == "" {
			return fmt.Errorf("pay_sdks.%s 已登记但 base_url 为空", name)
		}
		// accounts 是否为空不在这里查：只做验证码下发的 SDK 本就没有商户账户，
		// 真正需要出账主体的引用在 Resolve 里会报「未配置 accounts」。
		for _, alias := range sortedMapKeys(sdk.Accounts) {
			acc := sdk.Accounts[alias]
			if acc == nil || strings.TrimSpace(acc.MerchantID) == "" {
				return fmt.Errorf("pay_sdks.%s.accounts.%s 缺 merchant_id", name, alias)
			}
		}
	}
	return nil
}

// validateCache 校验两级缓存配置的明显错误。时长类字段 <=0 一律回落框架默认，
// 不在此报错；这里拦的是逻辑上不成立的取值。
func validateCache(c *Cfg) error {
	cc := c.Cache
	if cc == nil || !cc.Enabled {
		return nil
	}
	if cc.LogicalPct != 0 && (cc.LogicalPct < 1 || cc.LogicalPct > 99) {
		return fmt.Errorf("cache.logical_pct=%d 必须在 1..99（0 表示用默认）", cc.LogicalPct)
	}
	if cc.JitterPct != 0 && (cc.JitterPct < 0 || cc.JitterPct > 50) {
		return fmt.Errorf("cache.jitter_pct=%d 必须在 0..50（0 表示用默认）", cc.JitterPct)
	}
	if cc.Shards != 0 && (cc.Shards < 1 || cc.Shards&(cc.Shards-1) != 0) {
		return fmt.Errorf("cache.shards=%d 必须是 2 的幂", cc.Shards)
	}
	switch s := strings.ToLower(strings.TrimSpace(cc.StalePolicy)); s {
	case "", "async", "wait":
	default:
		return fmt.Errorf("cache.stale_policy=%s 非法（可选 async / wait）", cc.StalePolicy)
	}
	switch s := strings.ToLower(strings.TrimSpace(cc.Overflow)); s {
	case "", "block", "drop":
	default:
		return fmt.Errorf("cache.overflow=%s 非法（可选 block / drop）", cc.Overflow)
	}
	return nil
}

// sortedMapKeys 只为报错顺序稳定：同一份错配置两次启动报同一个首条错误，
// 便于比对与自动化断言。
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
