# TouchGoCore

Go 游戏服务框架：WebSocket、gRPC、定时器、Lua、Telegram、Gin、Redis/MySQL/Mongo。

## 快速开始

示例网关与登录服见 [example/README.md](example/README.md)。

```bash
./gatewayserver -c example/conf
# 或
./gatewayserver --config /opt/touchgo/conf
```

`-c` / `--config` 指向 **conf 目录**（内含 `config.ini` 与各服 JSON）。未指定时回退环境变量 `CONFIG_PATH`（可为 conf 目录或其上级），再自动查找。
需在 `conf/config.ini` 中配置服务器名到 JSON 的映射。

## 配置要点

- 启动配置：`-c` / `--config` 指定 `conf/` 目录；或 `CONFIG_PATH`；或可执行文件旁自动查找 `conf/config.ini`。
- gRPC 使用 `rpc` 字段（历史 `rpc_port` 键已移除，不再兼容）。
- WebSocket 路径取自 `ws.url` / `ws.inurl`（可为 `/ws` 或完整 `wss://host/path`），缺省 `/ws`。
- 队列容量与背压：`server.read_buffer` / `write_buffer` / `backpressure`。
- Prometheus：`metrics.enabled`，可选 `metrics.token` 保护 `/metrics`。
- 资金通道：供应商接入集中在顶层 `pay_sdks` 表，一段=一套供应商（`driver` 驱动标记、`base_url`、
  凭证、`endpoints` 端点表、`accounts` 我方商户账户）。通道段 `telegram.ton`（TON）、
  `usdt.provider`（USDT/TRC20）、`tron.provider` / `bsc.provider` / `sol.provider`（原生币）、
  `whatsapp.provider` / `whatsapp.login` 与 `zalo` / `facebook` / `instagram` 的同名两段（充值/提现、登录/验证码）
  只写 `{sdk, account}` 两个名字引用它，同一供应商的几条链路因此共用一份凭证，不必抄三份。
  四条社交通道同构：登录各引一段消息网关（`supplier_wa`/`supplier_zalo`/`supplier_fb`/`supplier_ig`），
  资金一律复用支付网关段——充值提现与平台无关，凭证只登记一次；登录之所以一家一段，
  是因为 `endpoints` 的键是全局逻辑名、不带平台维度，一段服务不了 `/wa/login` 与 `/zalo/login`
  两种实际路径；代价是密钥注入钩子按段名注册（`util.CallPaySDKMsg+段名`），同一供应商登记多段时
  下游要逐段注入，别把密钥抄进版本库。
  「开不开」有两处真相：SDK 段的 `enable` 决定这套凭证能不能用，通道段的 `sdk` 决定这条通道用不用它，
  两处都到位才是开；缺段或写 `off` 都不阻断整机。出款引用必须点到一个 `enable: "on"` 的商户账户
  （只有验证码链路可以没有账户）。键名与逐项说明见 [config/example.json](config/example.json)。
  `secret_key` 建议留空，由下游按 SDK 段名注册 `util.CallPaySDKMsg+"<段名>"` 钩子在启动前注入，
  不写进版本库；金额一律整数最小单位、手续费走万分比整数，全链路禁浮点。
- 资金动作的调用面：`pay.Channel` 四件事——查商户账户 `QueryAccount`、充值 `Recharge`、
  提现 `Withdraw`、查单 `QueryOrder`。上游按包名调用门面函数即可（`WhatsappRecharge` /
  `UsdtWithdraw` / `TonAccount` 等），不必先知道配置里写的是哪家 SDK；`driver` 标记由
  `paysdk` 读表后交给 `pay.Open` 解析成实现。接一家新供应商 = 在它自己的包里
  `pay.Register("驱动名", 构造函数)` + 配置里改 `driver`，通道包与上游都不动。
- NFT 资产通道：代码在 `nft` 包，四件事——查询持有 `NftHoldings`、单币详情 `NftToken`、
  铸造 `NftMint`、转账 `NftTransfer`，外加查单 `NftQuery` 与对账 `NftReconcile`。它与数字货币资金链路刻意不耦合：
  凭证登记在独立的顶层 `nft_sdks` 表（不复用 `pay_sdks`——两张表的端点逻辑名与驱动命名空间不同域，
  混用会让「换资金供应商」与「换 NFT 供应商」互相牵连），驱动走 `nft.Register`（内置 `nft_generic_md5`），
  密钥注入钩子是 `util.CallNftSDKMsg+"<段名>"`，与 `CallPaySDKMsg` 各注各的；同一供应商两边都供时凭证仍只各自注入、不落版本库。
  通道段 `nft` 用 `{sdk, account}` 引用 `nft_sdks` 一段，`chain`（eth/bsc/tron/sol/ton）与 `network` 参与签名。
  `token_id`、总量这类链上大整数一律十进制字符串，禁 float64。落库、额度与市场撮合（上架交易）归下游业务工程，
  本包不碰 SQL——与资金侧同一分层口径。接一家新 NFT 供应商 = 在它自己的包里 `nft.Register` + 配置改 `driver`。

## 开发

```bash
go build ./... && go vet ./...
go test ./config ./corectx ./db ./rpc ./util ./websocket ./telegram ./whatsapp ./zalo ./facebook ./instagram ./nft .
```

`go build ./...` 会连带编译 `example/` 的 3 个包（同属主模块），那里的编译错误是真错误。

本仓的开发过程资产不进版本库，clone 里看不到、也不必找：`docs/` 下的重构与评审报告、
文件名带 `p0_`~`p3_` 评审轮次前缀的测试、只含 `Benchmark` 的 `*_bench_test.go`。
判定口径写在 `.gitignore` 里，新写的测试若取这些命名会被静默忽略。

两个目录级陷阱：

- 包目录下的 `.txt` 是要进版本库的数据（`swd/` 的词典与映射经 `go:embed` 打进二进制）。
  `.gitignore` 的一次性输出规则因此限定在仓库根
  （`/*.txt`），不要改回裸 `*.txt`——那会让新增词典被静默忽略，本地与 CI 全过，
  下游拉取后 `go:embed` 直接编译失败。
- 全仓检索要排除工具存档目录：`grep -rn --include='*.go' --exclude-dir=.workbuddy-ai 模式 .`。
  `.` 前缀目录被 Go 工具链整体跳过，但 `grep` 不跳，命中的往往是过期副本。

业务启动前必须 `util.RegisterProtocolType` / `RegisterProtocolTypes`，未注册协议会被拒绝解析。
