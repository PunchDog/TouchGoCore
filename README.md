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
- 资金通道：每条通道的全部事实内联在它自己的链段里——`tron.usdt`（USDT-TRC20）、
  `ton.native`（原生 TON）、`bsc.usdt`（USDT-BEP20）与 `sol.usdt`（USDT-SPL）。
  一段=一套事实：`enable` 总闸、`driver` 驱动标记、币种事实（`network` / `contract` /
  `decimals`）与链上接入点 `chain`；`tron.usdt` 还带供应商接入
  （`base_url`、凭证、`endpoints`、`accounts`，只用于查商户账户）。没有集中登记表，
  也没有跨段的 `provider` 引用：写一段就够，读配置的人不必在两处拼一个人。
  **出款一律本地签名后广播**：USDT-TRC20 本地组交易、热钱包私钥签名、经 `chain.node` 的
  gRPC 广播；原生 TON 用 `tongo` 组外部消息、经 `chain.http_base`（TonAPI）广播（本仓不接
  lite server，原因见 `ton/gateway_tonapi.go` 头部注释），金额单位是纳吨；
  USDT-BEP20 用 EIP-155 本地签名、经 `chain.rpc` 的 JSON-RPC 广播；USDT-SPL 用
  `solana-go` 组包签名、经 `chain.rpc` 广播（寿命由 blockhash 而非 nonce 决定）。
  四条都用 `touchgocore/ledger` 的同一套出款台账保证「同一单号不签第二笔」（各持一个实例，
  键带链维度）；账户查询分两种口径：tron 读供应商账本，bsc/sol/ton 直接读链上余额；
  另有一个 `QueryOnchainAccount` 统一读热钱包链上余额（水位门禁用），四条链都实现
  （TON 只支持原生 TON）。`tron.usdt` / `bsc.usdt` / `sol.usdt` 支持按单出款：
  `PayOrder.From` 可携带地址+私钥+合约+网络，配置里的 `chain.wallet_address` /
  `private_key` / `contract` 只是缺省值——多线路热钱包因此不需要为每个钱包配一条通道。
  TON 不支持按单私钥（地址还取决于 wallet_version / sub_wallet_id），收到明确拒单。
  密钥分两个钩子、按段名各注一次：`util.CallPaySDKMsg+"<段名>"` 注供应商签名密钥
  （只有 tron.usdt 需要），`util.CallPayChainMsg+"<段名>"` 注**配置里的**
  热钱包私钥（段名即 `tron.usdt` / `ton.native` / `bsc.usdt` / `sol.usdt`），且只认
  这一个键名——同一段有两个名字都能塞密钥，等于让「密钥从哪来」变成猜的事。
  按单私钥不走钩子：它随 `From.PrivateKey`（`json:"-"` + `pay.Secret` 打码）进内存，
  只在签名与派生地址时 Reveal。当前由 tron/bsc/sol 消费；TON 收到明确拒单。
  TON 只接受 `PayOrder.Amount` 的 int64 最小单位，`amount_units` 会被显式拒绝。
  其余原生币/代币通道（`tron` 的 TRX、`bsc` 的 BNB、`sol` 的 SOL）的资金实现已在 2026-10 的
  资金面收口里删除：提现/查单/查账户三个门面的函数签名保留，一律返回各包的
  `ErrPayoutNotWired` 哨兵（`errors.Is` 可判）；充值/收款接口则整条删除（全仓只留提现）。
  各链的地址校验与 `pay` 的收款方规则**刻意不随提现实现删除**——删掉校验会让该币种的单
  走「没规则即放行」，那是资金安全退化而不是简化。
  Zalo 已改走 **zcago 协议直连**（非官方客户端，不再有 `supplier_zalo`）：扫码配对在本仓
  （`ZaloStartPairing` + WebSocket 端点 `GET /zalo/qr/{session_id}`），也可用
  `ZaloLoginWithCookie` 导入浏览器 cookie；账号凭证存 `zalo.sessions.dsn` 指向的 Postgres
  `zalo_sessions` 表，该 DSN 留空时复用 `whatsapp.sessions.dsn`（两者都空则不启动），
  `zalo.enable` 显式为 `on` 才装配。发送只支持文本，并严格限频：好友/群列表两次拉取间隔
  至少 5 分钟且走缓存，用户/群资料请求间隔至少 1.5 秒、缓存 10 分钟，连续发信随机 3–5 秒。
  zcago 是非官方客户端，**有封号风险**，通道默认不开；旧 `zalo.login` 配置与
  `ZaloSendCode/ZaloLogin/ZaloFriends` 门面已删除（破坏性变更），旧配置会由 `Validate` 报错指引迁移。
  `zalo_sessions.credentials` 含 cookie/IMEI，部署侧应收紧库权限或启用落盘加密；限频按
  accountID 在进程内记账，多实例/重启会重置，同一 Zalo 账号不要用多个 accountID 导入同一份 cookie。
  whatsapp 不在这三条之内：它的**消息侧已改走 `go.mau.fi/whatsmeow` 协议客户端**（非 HTTP 供应商）——
  QR 扫码配对在本仓（`WhatsappStartPairing` + WebSocket 端点 `GET /whatsapp/qr/{session_id}`），
  不扫码改用手机号取配对码走 `WhatsappGetPairingCode(ctx, phone)`（同步回 `XXXX-XXXX` 8 位码 +
  会话 id，配对码与终局事件复用同一条 `CallWhatsappMsg+"QR"` 广播与上面那个 WS 端点；
  号码在错误文案与日志里一律掩码，在飞会话另有包内总量上限），
  多账号会话密钥材料存 `whatsapp.sessions.dsn` 指向的 Postgres（sqlstore 管表），发消息/清空会话消息/
  吊销配对（`WhatsappDeleteDevice`：在跑先向服务端解绑再删设备行，没在跑只删行）
  直连 WhatsApp 多设备协议；本包不再有任何资金链路（提现/查单/查账户门面只剩哨兵，
  充值接口已整条删除）。整通道另有一支总闸
  `whatsapp.enable`：**显式为 `on` 才装配**，值不为 on（或压根没有 whatsapp 段）时消息侧与
  资金侧一起不启动，本包所有门面调用就地拒绝——默认不开是刻意的，见下条封号风险。
  读写门面（发消息/发群消息/清空消息/列好友/列群组/批量发信）**自带按需登录**：账号没在跑就先拉起并
  等到连上再办事，库里没有设备行时报错仍含「没有在跑」并带出「未配对」；「配对成功不自动上线」
  这条裁决管的是配对那一刻不批量拉起整库账号，两者不冲突（要长期在跑仍显式 `WhatsappStartClient`）。
  联系人另有 `WhatsappSyncContacts(ctx, fromJID, full)`：whatsmeow 的通讯录只来自
  `critical_unblock_low` 这份 app state 快照且**连接时不会自动拉**，所以新配对账号（扫码与配对码
  都一样）不拉一次就永远是空列表；`WhatsappListContacts` 已内置「本地为空才拉」的两段式，
  同账号并发由包内单飞收住、`full=true` 的全量重拉按 10 分钟窗口限流（它会先擦 app state 版本）。
  whatsmeow 是非官方客户端，用它收发违反 WhatsApp 服务条款、**账号可被封**，
  框架不为此担保，风控与责任归业务方。
  「开不开」有两处真相：SDK 段的 `enable` 决定这套凭证能不能用，通道段的 `sdk` 决定这条通道用不用它，
  两处都到位才是开；缺段或写 `off` 都不阻断整机。出款引用必须点到一个 `enable: "on"` 的商户账户
  （只有验证码链路可以没有账户）；whatsapp 通道在这两处之前还要先过 `whatsapp.enable` 总闸。
  键名与逐项说明见 [config/example.json](config/example.json)。
  `secret_key` 建议留空，由下游按段名注册 `util.CallPaySDKMsg+"<段名>"` 钩子在启动前注入，
  不写进版本库；金额一律整数最小单位、手续费走万分比整数，全链路禁浮点。
- 资金动作的调用面：上游按种类分派 `pay.ChannelFor(pay.KindTronUSDT)` 拿统一接口
  （`Withdraw` / `QueryOrder` / `QueryAccount` / `QueryOnchainAccount`），或直接调各包门面
  （`tron.UsdtWithdraw` / `ton.TonWithdraw` …）。分派表保留两条面：`pay.Transfer`
  是只含 `Withdraw` 的最小契约，`pay.Channel` 在它之上补查询；只实现 `Withdraw` 的旧装配方
  仍可 `pay.Bind`，但 `ChannelFor` 会明确拒绝而不是返回空实现。
  装配归各通道包，`Start` 成功即 `pay.Bind(kind, ch)`、`Stop` 先 `pay.Unbind` 再关连接——
  「这条通道起来了」与「分派找得到它」必须是同一件事。
  `pay` 包因此只剩契约：不 import `config`、不 import 任何通道包；传输实现（超时、指数退避、
  `Retry-After`、响应体上限）已下沉到 `internal/httpx`。接一家新供应商 = 在它自己的包里读
  自己那段配置、装配、`pay.Bind` + 配置里改 `driver`，`pay` 与上游都不动。
  前置校验口径：`To.Kind` 为 `bank` 时通道直接给明确错误、不进地址解析；`From` 留空即用配置
  的热钱包；tron/bsc/sol 支持 `From` 携带地址+私钥+合约+网络，TON 收到按单私钥直接拒；
  同一单号二次提交时 from/to/contract 必须一致。
  其余通道的同名门面仍在（`TronWithdraw` / `BscWithdraw` / `SolWithdraw` 等），但它们一律
  返回 `ErrPayoutNotWired`：接口保留是为了不破坏调用面，不代表这条链路可用。
- NFT 资产通道：代码在 `nft` 包，六个门面——查询持有 `NftHoldings`、单币详情 `NftToken`、
  铸造 `NftMint`、转账 `NftTransfer`、查单 `NftQuery`，外加 UNKNOWN 单的唯一出路
  `NftReconcile`（读回供应商事实，不改写 UNKNOWN，也不自动重下）。它与数字货币资金链路刻意不耦合：
  凭证登记在独立的顶层 `nft_sdks` 表（不复用 `pay_sdks`——两张表的端点逻辑名与驱动命名空间不同域，
  混用会让「换资金供应商」与「换 NFT 供应商」互相牵连），驱动走 `nft.Register`（内置 `nft_generic_md5`），
  密钥注入钩子是 `util.CallNftSDKMsg+"<段名>"`，与 `CallPaySDKMsg` 各注各的；同一供应商两边都供时凭证仍只各自注入、不落版本库。
  通道段 `nft` 的 `provider` 字段（即 `nft.provider`，形态 `{sdk, account}`）引用 `nft_sdks` 一段，
  `chain`（eth/bsc/tron/sol/ton）与 `network` 参与签名。`nft_sdks.<段>.endpoints` 的键只有
  `holdings/token/mint/transfer/query` 五个合法逻辑名（写错键名启动即报错），`NftReconcile` 复用 `query`、
  不占独立端点；`<段>.accounts.<别名>` 必须带 `merchant_id`。开不开有两处真相、都得开：
  `nft.provider` 留空即本通道不启用，`nft_sdks.<段>.enable` 不为 `"on"` 即凭证不可用。
  `token_id`、总量这类链上大整数一律十进制字符串，禁 float64。落库、额度与市场撮合（上架交易）归下游业务工程，
  本包不碰 SQL——与资金侧同一分层口径。接一家新 NFT 供应商 = 在它自己的包里 `nft.Register` + 配置改 `driver`。

## 开发

```bash
go build ./... && go vet ./...
go test ./... -count=1
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
