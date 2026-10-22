# pay 重构计划：配置归位 + 纯转账接口

- 项目：TouchGoCore（Go）
- 日期：2026-10-21
- 状态：计划（未实施）
- 范围：`config/`、`pay/`、`tron/`、`telegram/`、`usdt/`、`bsc/`、`sol/`、`facebook/`、`instagram/`、`services.go`、`app.go`、`util/`

---

## 0. 一句话目标

把 `pay_sdks` 集中表拆掉、配置回归各通道段；`pay` 收敛为「转账契约 + 按 kind 分派」，不持有配置、不实现 HTTP、不定义驱动注册表；转账实现全部落在各自的逻辑包（`tron.usdt` / `bsc.usdt` / `sol.usdt` / `telegram.ton`）。

> 本文档含两大部分：§1–§10 是「配置归位 + pay 瘦身」的存量重构；§11 是「bsc/sol 补齐 USDT 链上出款」的增量扩展。§11 依赖 §7 的 P0–P6 完成后再动手（P1.5 除外，它与 P1 同批做）。

---

## 1. 现状与问题

| # | 现状                                                                                                     | 问题                                                    |
| - | ------------------------------------------------------------------------------------------------------ | ----------------------------------------------------- |
| 1 | 供应商凭证、端点、链上接入点集中在顶层 `pay_sdks`（`config/example.json:193-331`，4 个子段）                                    | 配置与通道段分离，看 `tron.usdt` 段看不到它实际用哪套凭证                          |
| 2 | `usdt.provider` / `telegram.ton.provider` / `facebook.login` / `instagram.login` 用 `{sdk, account}` 引用 | 引用链路需要二次跳转，且删段会触发 `config/validate.go:262-266` 的启动期报错 |
| 3 | `pay/assemble.go` 是全包唯一 `import config` 的文件                                                            | 契约层反向依赖配置层                                            |
| 4 | `pay.Channel` 定义 Withdraw/QueryOrder/QueryAccount 三方法                                                  | 一个接口承担三件事，链上驱动被迫实现用不到的账户查询                            |
| 5 | `pay/client.go` + `pay/provider.go`（合计 1048 行）承载通用 HTTP 与 MD5 实现                                       | pay 同时是契约层与实现层，职责混杂                                   |
| 6 | `facebook` / `instagram` 两个包只剩登录链路                                                                     | 与「pay 只做转账」的目标不一致                                     |
| 7 | `PayOrder.Address` 是扁平字符串                                                                              | 无法表达银行账户，也无法显式指定出款方                                   |

---

## 2. 终局架构

```
config（纯数据，不依赖任何业务包）
├─ tron.usdt        TRC20 USDT（链参数 + 供应商接入内联）
├─ bsc.usdt         BEP-20 USDT（链参数 + RPC 接入）
├─ sol.usdt         SPL USDT（链参数 + RPC 接入）
├─ telegram.ton     TON（链参数 + 供应商接入内联）
└─ nft              本次不动（nft_sdks 独立表，后续单独立项）

pay（零 config 依赖、零 HTTP、零签名算法）
├─ types.go         PayOrder / PayResult / 状态与币种常量 / AccountQuery+AccountInfo
├─ party.go         Party / BankAccount / PartyKind / Normalize / Check
└─ transfer.go      Transfer 接口 / Kind 常量 / Bind / TransferTo

tron（tron.usdt 的实现）
├─ 链上出款、链上查单、供应商 account 查询、地址规则
bsc（bsc.usdt 的实现）
├─ 链上出款、链上查单、链上余额 balanceOf、地址规则
sol（sol.usdt 的实现）
├─ 链上出款、链上查单、链上余额、地址规则
telegram（telegram.ton 的实现）
├─ 链上出款、链上查单、供应商 account 查询、地址规则

usdt             纯转调 tron 的门面（签名不变）
whatsapp / zalo  消息通道（资金门面保持哨兵）
```

> `bsc` / `sol` 两条链路在 P7 / P8 完成后才是上图形态；在此之前它们只有地址规则与
> `ErrPayoutNotWired` 哨兵（见 §11）。顶层 `usdt` 段在 P1（含 P1.5 三块化）删除；`pay_sdks` 在 P4 删除。

依赖方向（硬约束）：

```
config  ←  tron / bsc / sol / telegram / usdt / whatsapp / zalo
pay     ←  tron / bsc / sol / telegram / usdt / whatsapp / zalo
pay  ↛  config
pay  ↛  tron / bsc / sol / telegram
```

`pay` 不得 import `config`，也不得 import 任何通道包。分派靠运行期注册，不靠编译期引用。

---

## 3. 契约定义

### 3.1 Party（新增 `pay/party.go`）

```go
type PartyKind string

const (
    PartyWallet   PartyKind = "wallet"   // 链上货币钱包
    PartyBank     PartyKind = "bank"     // 银行账户
    PartyMerchant PartyKind = "merchant" // 供应商侧商户账户
)

type Party struct {
    Kind PartyKind `json:"kind,omitempty"` // 空则按字段推断

    Name    string `json:"name,omitempty"`    // 户名 / 本地账户别名
    Account string `json:"account,omitempty"` // 商户号 / 内部账号

    // 链上钱包
    Address string `json:"address,omitempty"`
    Chain   string `json:"chain,omitempty"`   // tron / ton / eth / bsc / sol
    Network string `json:"network,omitempty"` // mainnet / testnet
    Memo    string `json:"memo,omitempty"`    // tag / comment

    // 银行账户
    Bank *BankAccount `json:"bank,omitempty"`
}

type BankAccount struct {
    BankName    string `json:"bank_name,omitempty"`
    AccountNo   string `json:"account_no,omitempty"`
    AccountName string `json:"account_name,omitempty"`
    Branch      string `json:"branch,omitempty"`
    SWIFT       string `json:"swift,omitempty"`
    IBAN        string `json:"iban,omitempty"`
    RoutingNo   string `json:"routing_no,omitempty"`
    Country     string `json:"country,omitempty"`
    Currency    string `json:"currency,omitempty"`
}
```

### 3.2 PayOrder（改造 `pay/types.go`）

```go
type PayOrder struct {
    OrderNo  string `json:"order_no"`
    Amount   int64  `json:"amount"`
    Currency string `json:"currency,omitempty"`

    From *Party `json:"from,omitempty"` // 空＝用通道配置的默认出款主体
    To   *Party `json:"to"`             // 必填

    Phone string            `json:"phone,omitempty"`
    Code  string            `json:"code,omitempty"`
    Extra map[string]string `json:"extra,omitempty"`

    // Deprecated: 兼容既有调用方；Normalize 时合成 To，实现层不再读
    Address string `json:"address,omitempty"`
    Network string `json:"network,omitempty"`
    Memo    string `json:"memo,omitempty"`
}

func (o *PayOrder) Normalize()  // 旧字段 → To；推断 Kind
func (o *PayOrder) Check() error // To 必填；From/To 不同；按 Kind 校验必需字段
```

归一规则：

| 场景                                                 | 处理                                                                |
| -------------------------------------------------- | ----------------------------------------------------------------- |
| `To == nil` 且 `Address != ""`                      | 合成 `Party{Kind: wallet, Address, Network, Memo}`                  |
| `To != nil` 且 `To.Address == ""` 且 `Address != ""` | 用旧字段补 `To.Address`                                                |
| `To.Kind == ""`                                    | 有 `Bank` → `bank`；有 `Address` → `wallet`；有 `Account` → `merchant` |
| `To` 与 `Address` 都空                                | 拒单：「收款方 to 必填」                                                    |

`Normalize` 必须幂等：只补空位，不覆盖已显式填写的字段。

校验矩阵：

| Kind       | 必需字段                                  | 本仓支持                    |
| ---------- | ------------------------------------- | ----------------------- |
| `wallet`   | `Address`                             | ✅ tron / telegram       |
| `merchant` | `Account`                             | ✅ 供应商驱动（HTTP）           |
| `bank`     | `Bank.AccountNo` + `Bank.AccountName` | ❌ 结构预留，通道返回「本通道不支持银行转账」 |

### 3.3 Transfer（新增 `pay/transfer.go`）

```go
type Transfer interface {
    Withdraw(ctx context.Context, o *PayOrder) (*PayResult, error)
}

type Kind string

const (
    KindTronUSDT Kind = "tron.usdt"
    KindBscUSDT  Kind = "bsc.usdt"
    KindSolUSDT  Kind = "sol.usdt"
    KindTON      Kind = "telegram.ton"
)

func Bind(kind Kind, t Transfer) error
func TransferTo(ctx context.Context, kind Kind, o *PayOrder) (*PayResult, error)
```

`Kind` 按「链.币种」命名，而不是只写币种：同一种 USDT 在三条链上是三个互不相通的通道，
用 `KindUSDT` 一个值会让 `TransferTo` 无法表达「发哪条链」，只能由调用方再传一个链参数——
那等于把分派逻辑又推回上游。P0–P6 阶段先落地前两个常量，`bsc.usdt` / `sol.usdt` 在 P7 / P8 补齐。

`Bind` 重复注册返回错误（不覆盖）：两处实现抢同一个 kind 是资损级风险。

### 3.4 保留但不再作为接口

- `PayResult`：转账回执，`Transfer.Withdraw` 的返回类型。
- `AccountQuery` / `AccountInfo`：纯数据，被 6 个包的门面签名引用（`tron/tron.go:54`、`tron/usdt.go:134`、`telegram/payout.go:217`、`bsc/bsc.go:41`、`sol/sol.go:54`、`whatsapp/whatsapp.go:192`、`zalo/zalo.go:184`），删了是连锁破坏。
- `Currency*` / 状态常量：转账语义的一部分。

---

## 4. 配置迁移

### 4.1 `pay_sdks` → 各通道段

| 原 `pay_sdks.<段>` 字段                                                                  | 新位置                                             |
| ------------------------------------------------------------------------------------ | ----------------------------------------------- |
| `enable`                                                                             | `tron.usdt.enable` / `telegram.ton.enable`      |
| `driver`                                                                             | 同名内联                                            |
| `base_url` / `app_id` / `secret_key` / `auth_header` / `timeout_sec` / `max_retries` | 同名内联                                            |
| `endpoints` / `accounts`                                                             | 同名内联；`tron.usdt` 与 `telegram.ton` 只保留 `account` |
| `chain.*`                                                                            | `tron.usdt.chain` / `telegram.ton.chain`        |

原通道段已有的 `network` / `contract` / `jetton` 原地不动（与 chain 正交，无键名冲突）。

### 4.2 段名映射

| 原段             | 新归属                    |
| -------------- | ---------------------- |
| `supplier_a`   | `tron.usdt`            |
| `supplier_ton` | `telegram.ton`         |
| `supplier_fb`  | 删除（login 链路移除）         |
| `supplier_ig`  | 删除（login 链路移除）         |

### 4.3 默认出款方

| 驱动              | 默认 `From` 等价形态                                                                                                      |
| --------------- | ----------------------------------------------------------------------------------------------------------------- |
| `tron_trongrid` | `Party{Kind: wallet, Address: tron.usdt.chain.wallet_address, Chain: "tron", Network: tron.usdt.network}`          |
| `ton_tonapi`    | `Party{Kind: wallet, Address: telegram.ton.chain.wallet_address, Chain: "ton", Network: telegram.ton.network}`     |
| `generic_md5`   | `Party{Kind: merchant, Account: tron.usdt.accounts.default.merchant_id}`                                          |

链上硬约束：`From.Address` 只能为空或等于 `chain.wallet_address`，否则拒单。私钥只有一把，不存在第二个可签名的出款地址。

### 4.4 密钥注入钩子

| 原                                       | 新                                             |
| --------------------------------------- | --------------------------------------------- |
| `util.CallPaySDKMsg + "supplier_a"`     | `util.CallPaySDKMsg + "tron.usdt"`            |
| `util.CallPayChainMsg + "supplier_a"`   | `util.CallPayChainMsg + "tron.usdt"`          |
| `util.CallPaySDKMsg + "supplier_ton"`   | `util.CallPaySDKMsg + "telegram.ton"`         |
| `util.CallPayChainMsg + "supplier_ton"` | `util.CallPayChainMsg + "telegram.ton"`       |

P7 / P8 追加两组：`CallPayChainMsg + "bsc.usdt"`、`CallPayChainMsg + "sol.usdt"`。

**部署侧注入脚本必须同步**，否则 secret 为空导致通道不启动。

---

## 5. 删除清单

### 5.1 整包删除

| 目标           | 文件数          | 前置条件                                               |
| ------------ | ------------ | -------------------------------------------------- |
| `facebook/`  | 3 源文件 + 6 测试 | 外部业务工程不再调用 `FacebookSendCode/Login/Logout/Friends` |
| `instagram/` | 3 源文件 + 5 测试 | 同上                                                 |

### 5.2 pay 包

| 文件                    | 行数  | 理由                      |
| --------------------- | --- | ----------------------- |
| `pay/assemble.go`     | 194 | 配置装配归各通道包               |
| `pay/channel.go`      | 118 | `Channel` 三方法接口 + 驱动注册表 |
| `pay/client.go`       | 274 | HTTP 传输，下沉              |
| `pay/provider.go`     | 774 | generic_md5 实现，下沉       |
| `pay/chainrule.go`    | 118 | 地址规则，下沉为包内函数            |
| `pay/chainoptions.go` | 60  | 链上参数，tron/telegram 各自定义 |


删除后 `pay` 约 250 行（现 1590 行）。

### 5.3 config 包

| 位置                                                                                                    | 动作         |
| ----------------------------------------------------------------------------------------------------- | ---------- |
| `config/config.go:39` `PaySDKs`                                                                       | 删          |
| `config/config.go:30-31` `Facebook` / `Instagram`                                                     | 删          |
| `config/struct.go:229-470` `PaySDKConfig` / `PayChainConfig` / `PayMerchantAccount` / `PaySDKRef` 及方法 | 删          |
| `config/struct.go:598-617` `FacebookConfig` / `InstagramConfig`                                       | 删          |
| `config/validate.go:223-318` `validatePayChannels`                                                    | 删，改为按通道段校验 |
| `config/validate.go:238-256` login 引用与 service 白名单                                                    | 删          |
| `config/config.go:32` `Usdt *UsdtConfig`                                                              | 删（内容并入 `config.Tron.Usdt`，P1.5） |
| `config/struct.go:619-630` `UsdtConfig`                                                               | 删（由 `TronUsdtConfig` 取代） |

### 5.4 顶层 `usdt` 段的引用点

顶层 `usdt` 配置段删除后，`cfg.Usdt` 的引用点必须同步（非测试共 3 处）：

| 位置 | 改造 |
|---|---|
| `config/validate.go:242-243` | 随 `validatePayChannels` 一并删除 |
| `tron/usdt.go:44/47/54/72` | 改读 `cfg.Tron.Usdt`（P2 双路径时改） |
| `config/example.json:471-482` | 删 `usdt说明` 与 `usdt` 段（P3） |

测试侧 4 个文件各有一处夹具：`config/chain_addr_example_test.go`、`config/p1_example_json_binding_test.go`、`config/pay_sdk_test.go`、`tron/usdt_test.go`，随 P3 / P6 的配置形态改写。

### 5.5 其他

| 位置                                       | 动作                                          |
| ---------------------------------------- | ------------------------------------------- |
| `app.go:270-271`                         | 删 `facebookService` / `instagramService` 注册 |
| `services.go:122-146`                    | 删两个 service 适配器                             |
| `util/callback.go:136-137`               | 删 `CallFacebookMsg` / `CallInstagramMsg`    |
| `bsc/address.go:115`、`sol/address.go:98` | 删 `pay.RegisterChainRule` 调用（已核实：只注册、无消费者）  |

---

## 6. 下沉清单

| 原 pay 能力                                                   | 新位置                                                                    | 说明                                                                                  |
| ---------------------------------------------------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `Provider.QueryAccount`（MD5 + `/pay/account`）              | `tron/account.go`、`telegram/ton_account.go`                            | 各写精简版（签名 + 单端点，约 80–100 行）                                                          |
| `ChainRule` / `CheckChainRules`                            | `tron/chainrule.go`（已有 TRONRule）、`telegram/ton_address.go`（已有 tonRule） | 改为包内 `checkOrderAddress(o)`；调用点 `tron/onchain.go:261`、`telegram/ton_onchain.go:166` |
| `ChainOptions`                                             | `tron/onchain.go`、`telegram/ton_onchain.go` 各自结构                       | 字段相同但不再跨包共享                                                                         |
| `Open` / `OpenService` / `OpenChain` / `Resolved`          | 各通道包 `fromInline(cfg)`                                                 | 自己读自己的配置段                                                                           |
| `Provider.SetToken` / `TokenHeader` / `DefaultTokenHeader` | 删除                                                                     | 唯一使用者是 facebook/instagram 的 `auth.go:72`                                            |

---

## 7. 阶段计划

每个阶段独立可编译、可回滚。

### P0 删除 login 链路

**改动**

1. 删 `facebook/`、`instagram/` 整目录。
2. `app.go:270-271`、`services.go:122-146` 删服务注册与适配器。
3. `config/config.go:30-31`、`config/struct.go:598-617` 删配置类型与字段。
4. `config/validate.go:238-256` 删 login 引用与 service 白名单。
5. `config/example.json:451-470` 删 facebook/instagram 两段；`:293-330` 删 supplier_fb/supplier_ig。
6. `util/callback.go:136-137` 删两个回调常量。
7. `pay/provider.go` 删 `SetToken` / `TokenHeader` / `DefaultTokenHeader` / token 头校验。
8. `config/p1_example_json_binding_test.go:309-328` 删 fb/ig 引用断言。

**验证**：`go build ./...`；`go test ./config/ ./util/ ./pay/`

**回滚**：单 commit，`git revert`

---

### P1 配置内联

**改动**

1. 新增内联结构（`config/struct.go`）：`ChannelSupplierConfig`，字段与 `PaySDKConfig` 对齐（driver/base_url/app_id/secret_key/auth_header/timeout_sec/max_retries/endpoints/accounts/chain）。
2. 新增三块链配置：`TronConfig{ Usdt *TronUsdtConfig }`、`BscConfig{ Usdt *BscUsdtConfig }`、`SolConfig{ Usdt *SolUsdtConfig }`；三者的 `Usdt` 均内嵌 `ChannelSupplierConfig`，另加 `network` / `contract` / `decimals` / `chain`。
3. `TonChannelConfig`（`struct.go:216`）内嵌 `ChannelSupplierConfig`；旧 `Provider *PaySDKRef` 标 Deprecated 保留。
4. **删顶层 `UsdtConfig`（`struct.go:619-630`）与 `Cfg.Usdt`（`config.go:32`）**：原字段全部并入 `TronConfig.Usdt`，不留双源。
5. `config/validate.go` 加「内联与旧引用同时存在即报错」；`c.Usdt` 的两处引用（`validate.go:242-243`）改读 `c.Tron.Usdt`。
6. `config/example.json`：新增 `tron` / `bsc` / `sol` 三段，删顶层 `usdt` 段；`telegram.ton` 补内联字段（旧 provider 暂时保留）。

**验证**：`go build ./...`；`go test ./config/`

---

### P2 装配双路径

**改动**

1. `tron/usdt.go:72` 新增 `newUSDTClientInline(cfg)`：从 `cfg.Tron.Usdt` 直接构造 `pay.ProviderOptions` + `pay.ChainOptions`，不经 `pay.OpenChain`。
2. `telegram/payout.go:117` 新增同构 `newTonClientInline(cfg)`。
3. 补等价性测试：同一份配置用旧引用与新内联分别装配，逐字段比对 Options。

**验证**：`go test ./tron/ ./telegram/ ./usdt/`

---

### P2.5 转账契约扩展（Party / From / To）

**改动**

1. 新增 `pay/party.go`：`Party` / `BankAccount` / `PartyKind` / `Normalize` / `Check`。
2. `pay/types.go`：`From`/`To` 改 `*Party`；`Address`/`Network`/`Memo` 标 Deprecated。
3. 约 20 处取值替换（`o.Address` → `o.To.Address`）：
   - `tron/address.go:82`、`tron/onchain.go:330,359`、`tron/query.go:170,178`
   - `telegram/ton_address.go:153`、`telegram/ton_onchain.go:234,238,245,284`、`telegram/ton_query.go:224,235`
   - `bsc/address.go:92`、`sol/address.go:74`
   - `pay/provider.go:508,533,761`（供应商报文构造，字段名仍为 `address`）
4. `tron/onchain.go` 与 `telegram/ton_onchain.go` 的 `Withdraw`：先按 `Kind` 分支（wallet 走原逻辑，bank 拒绝），再解析 `From` 默认值与链上地址一致性。
5. 实现层加「同一单号二次提交时 from/to 必须一致」校验。

**验证**：`go test ./pay/ ./tron/ ./telegram/ ./bsc/ ./sol/`

**风险**：`from`/`to` 从 string 变 object，外部调用方需同步；旧 `address` 字段是唯一平滑迁移路径。

---

### P3 示例切换

**改动**

1. `config/example.json`：`tron.usdt` 与 `telegram.ton` 改为内联形态；删 `pay_sdks说明`（`:192`）与 `pay_sdks`（`:193-331`）；删两段的 `provider` 引用；**删顶层 `usdt说明` 与 `usdt` 段（`:471-482`）**——其内容已全部迁入 `tron.usdt`。
2. `config/p1_example_json_binding_test.go:278` 的「表必须非空」硬断言改为「表可缺省」；`:309-360` 的引用解析循环改为空表跳过。

**验证**：`go test ./config/`；启动进程确认日志出现 2 条「不启动」而非配置校验失败。

---

### P4 删装配层与旧表

**改动**

1. 删 `pay/assemble.go`。
2. 删 `config.PaySDKs` / `PaySDKConfig` / `PayChainConfig` / `PayMerchantAccount` / `PaySDKRef`。
3. 删 `validatePayChannels`，改为 `validateUsdtChannel` / `validateTonChannel`（按内联字段校验）。
4. 删各通道的旧装配路径（`newUSDTClient` / `newTonClient` 的旧分支）。

**验证**：`go build ./... && go vet ./...`；`go test ./config/ ./tron/ ./telegram/`

**顺序要求**：必须在 P3 之后——先让示例与默认路径切到内联，再删旧表。

---

### P5 pay 瘦身

**改动**

1. 删 `pay/channel.go` / `client.go` / `provider.go` / `chainrule.go` / `chainoptions.go`。
2. 新增 `pay/transfer.go`：`Transfer` / `Kind` / `Bind` / `TransferTo`。
3. 下沉：
   - `tron/account.go`：MD5 签名 + `/pay/account` 调用（从 `pay/client.go` + `provider.go` 精简）。
   - `telegram/ton_account.go`：同构。
   - `tron/onchain.go:261`、`telegram/ton_onchain.go:166`：`pay.CheckChainRules` → 包内 `checkOrderAddress`。
   - `ChainOptions` → 各自包内结构。
4. 删 `tron/driver.go:16`、`telegram/ton_driver.go:16` 的 `pay.Register` init。
5. `tron/onchain.go:59`、`telegram/ton_onchain.go:48` 的返回类型从 `pay.Channel` 改为各包内部接口。

**验证**：`go test ./pay/ ./tron/ ./telegram/ ./bsc/ ./sol/ ./usdt/`

**这是唯一有实质编码量的阶段**：两份 account 客户端 + 两处地址校验改包内调用，其余为删除。

---

### P6 分派与收尾

**改动**

1. `tron/usdt.go`、`telegram/payout.go` 的 `Start` 成功后调用 `pay.Bind(pay.KindTronUSDT, ch)` / `pay.Bind(pay.KindTON, ch)`。
2. 密钥钩子键名改为 kind（见 §4.4）。
3. 清理测试夹具：`config/pay_sdk_test.go`、`pay/assemble_test.go`、`config/p1_example_json_binding_test.go`、`usdt/*_test.go`、`tron/usdt_test.go`、`telegram/ton_nilcfg_test.go`。
4. 文档：`README.md:25-28,91,102`、`docs/repo-layout.md`、`config/example.json` 各段说明。

**验证**：`go test ./...`；`go test ./bsc/ ./sol/ ./nft/`（确认未误伤）

---

## 8. 验收标准

```bash
go build ./...
go vet ./...
go test ./... -count=1
```

外加三条结构性断言：

1. `pay` 不依赖 `config`：  
   `go list -f '{{join .Imports "\n"}}' ./pay | grep touchgocore/config` 必须无输出。
2. `pay` 不依赖任何通道包：  
   `go list -f '{{join .Imports "\n"}}' ./pay` 中不得出现 `touchgocore/tron` / `touchgocore/telegram`。
3. `pay` 的导出符号只剩契约：  
   `go doc ./pay` 中不得出现 `Client` / `Provider` / `ProviderOptions` / `ChainOptions` / `Register` / `OpenDriver` / `ChainRule`。

功能验收：

- `pay.TransferTo(ctx, pay.KindTronUSDT, order)` 能落到 `tron` 的实现；`pay.KindTON` 落到 `telegram`。
- `From` 为空时使用配置默认出款方；链上 `From` 指定非热钱包地址时拒单。
- `To.Kind == "bank"` 时通道返回明确错误，不进入地址解析。
- 旧调用方只填 `address` 仍可正常出款。

---

## 9. 风险与待确认

| # | 风险                            | 影响                         | 规避                                            |
| - | ----------------------------- | -------------------------- | --------------------------------------------- |
| 1 | `from`/`to` 从 string 变 object | 外部调用方 JSON 反序列化失败          | 保留 `address` 回落；发布前与下游确认                      |
| 2 | 删除 facebook/instagram 包       | 外部直接调用登录门面的工程编译失败          | 本仓唯一引用是 `services.go`，外部需单独确认                 |
| 3 | 钩子键名变更                        | 部署侧未同步 → secret 为空 → 通道不启动 | 启动日志把「未注入密钥」打成 Warning                        |
| 4 | 链上 `from` 只能是热钱包              | 无法多钱包出款                    | 明确边界；多钱包需把私钥下沉到 `accounts.<alias>.chain`，单独立项 |
| 5 | 两份 account 客户端重复约 80 行        | 维护成本                       | 「pay 不留实现」的必然代价，不再抽公共包                        |
| 6 | 银行能力是空壳                       | 误用后报出误导性地址错误               | 各通道 `Withdraw` 显式拒绝 `bank`                    |
| 7 | `bsc`/`sol` 删 ChainRule 注册    | 地址校验口径可能分散                 | 已核实只注册无消费者；两包保留自身地址函数                         |
| 8 | `nft_sdks` 同构问题               | 未来一致性债                     | 本次不动，单独立项                                     |

**需用户确认**

1. 外部业务工程是否直接调用 `facebook.FacebookLogin/SendCode/Logout/Friends` 或 `instagram.*`？
2. 部署侧注入脚本是否可同步改键名（`supplier_a` → `tron.usdt`、`supplier_ton` → `telegram.ton`）？
3. `from`/`to` 结构变更的下游改造窗口。

---

## 10. 附：各阶段工作量

| 阶段         | 性质            | 相对工作量 |
| ---------- | ------------- | ----- |
| P0 删 login | 删除            | 小     |
| P1 配置内联（含 P1.5 三块化） | 新增结构          | 中     |
| P2 装配双路径   | 新增函数 + 等价性测试  | 中     |
| P2.5 契约扩展  | 契约改造 + 20 处替换 | 中     |
| P3 示例切换    | 文档 + 测试       | 小     |
| P4 删装配层    | 删除            | 小     |
| P5 pay 瘦身  | 删除 + 两份客户端下沉  | **大** |
| P6 分派收尾    | 新增分派 + 清理     | 中     |

> 多链扩展阶段 P1.5 / P7 / P8 的工作量见 §11.7。

---

## 11. 多链 USDT 扩展（tron / bsc / sol）

### 11.1 现状

| 包      | 现有能力                                                                 | 缺口    |
| ------ | -------------------------------------------------------------------- | ----- |
| `tron` | 完整 USDT-TRC20 链上出款：地址校验、交易构造、本地签名、gRPC/HTTP 广播、查单、幂等台账、余额查询（12 个源文件） | 无     |
| `bsc`  | 仅 `address.go`（EIP-55 校验）+ 三个 `ErrPayoutNotWired` 哨兵                 | 无出款实现 |
| `sol`  | 仅 `address.go`（Base58 32 字节校验）+ 三个 `ErrPayoutNotWired` 哨兵            | 无出款实现 |

`bsc/bsc.go:22` 与 `sol/sol.go:36` 的哨兵文案明确写着「本仓链上出款只交付 USDT-TRC20 与原生 TON 两条通道」——本次要把这两条补上。

### 11.2 配置三块化

顶层 `usdt` 段**整段删除**（不是保留后新增三块）：原字段全部并入 `tron.usdt`，`UsdtConfig` 类型一并删除。

`tron.usdt` 另含 `base_url` / `app_id` / `secret_key` / `auth_header` / `timeout_sec` / `max_retries` / `endpoints` / `accounts`（即原 `supplier_a` 的内联字段），下面示例省略这些以突出链参数。


```json
{
  "tron": {
    "usdt": {
      "enable": "off",
      "driver": "tron_trongrid",
      "network": "mainnet",
      "contract": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
      "decimals": 6,
      "chain": { "node": "grpc.trongrid.io:50051", "tls": "off",
                 "http_base": "https://api.trongrid.io", "api_key": "CHANGE-ME",
                 "wallet_address": "T...", "private_key": "",
                 "fee_limit": 0, "timeout_sec": 30, "max_retries": 2 }
    }
  },
  "bsc": {
    "usdt": {
      "enable": "off",
      "driver": "bsc_evm",
      "network": "mainnet",
      "contract": "0x55d398326f99059fF775485246999027B3197955",
      "decimals": 18,
      "chain": { "rpc": "https://bsc-dataseed.binance.org", "chain_id": 56,
                 "wallet_address": "0x...", "private_key": "",
                 "gas_limit": 0, "gas_price_gwei": 0,
                 "timeout_sec": 30, "max_retries": 2 }
    }
  },
  "sol": {
    "usdt": {
      "enable": "off",
      "driver": "sol_spl",
      "network": "mainnet-beta",
      "contract": "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
      "decimals": 6,
      "chain": { "rpc": "https://api.mainnet-beta.solana.com", "api_key": "",
                 "wallet_address": "...", "private_key": "",
                 "commitment": "confirmed", "priority_fee_microlamports": 0,
                 "timeout_sec": 30, "max_retries": 2 }
    }
  }
}
```

`tron.usdt` 保留供应商 HTTP 字段（`base_url`/`app_id`/`endpoints`/`accounts`）用于 `QueryAccount`；  
`bsc.usdt` / `sol.usdt` 不需要——账户查询直接走链上 `balanceOf`。

### 11.3 模板对齐

以 `tron` 的文件划分为模板，`bsc` / `sol` 逐文件对齐（结构复制，实现按各自链重写）：

| tron 文件                          | bsc 对应                                                                  | sol 对应                                                          |
| -------------------------------- | ----------------------------------------------------------------------- | --------------------------------------------------------------- |
| `address.go`                     | 复用现有 `bsc/address.go`                                                   | 复用现有 `sol/address.go`                                           |
| `chainrule.go`                   | 新增 `bscRule`（币种 `BSC-USDT`）                                             | 新增 `solRule`（币种 `SOL-USDT`）                                     |
| `encoding.go`                    | RLP + EIP-155 签名域                                                       | 消息序列化 + ed25519 签名                                              |
| `gateway.go` / `gateway_http.go` | JSON-RPC（eth_call / eth_sendRawTransaction / eth_getTransactionReceipt） | JSON-RPC（getLatestBlockhash / sendTransaction / getTransaction） |
| `gateway_grpc.go`                | 无（BSC 无 gRPC 公共端点）                                                      | 无                                                               |
| `onchain.go`                     | 构造 ERC-20 `transfer(address,uint256)`                                   | 构造 SPL `Transfer` instruction                                   |
| `query.go`                       | receipt + `Transfer` 事件日志解析                                             | transaction meta 的 balance changes 解析                           |
| `ledger.go`                      | 幂等台账（先记账后广播）                                                            | 同构                                                              |
| `route.go`                       | 生命周期 + 回执广播（`util.CallBscMsg`）                                          | 同构（`util.CallSolMsg`）                                           |
| `usdt.go`                        | 门面 + 装配                                                                 | 同构                                                              |

### 11.4 三链技术差异

| 维度           | TRON                   | BSC                          | SOL                          |
| ------------ | ---------------------- | ---------------------------- | ---------------------------- |
| 签名算法         | secp256k1              | secp256k1 + RLP/EIP-155      | **ed25519**                  |
| 交易格式         | protobuf               | RLP                          | 二进制消息                        |
| 广播           | gRPC / HTTP            | JSON-RPC                     | JSON-RPC                     |
| USDT 合约      | TRC-20                 | BEP-20                       | SPL Token                    |
| **USDT 小数位** | **6**                  | **18**                       | **6**                        |
| 手续费          | TRX（`fee_limit`，sun）   | BNB（`gas_price × gas_limit`） | SOL（base fee + priority fee） |
| 查单           | 交易回执                   | receipt + logs               | transaction meta             |
| 余额           | `balanceOf`（HTTP/gRPC） | `balanceOf`（eth_call）        | `getTokenAccountBalance`     |

**BSC USDT 是 18 位小数**，TRON/SOL 是 6 位。`PayOrder.Amount` 的现有语义是「该合约最小单位整数」，跨链同币种差 10^12 —— 这是本次最高优先级的资损风险点，见 §11.6。

### 11.5 新增阶段

| 阶段              | 内容                                                                                                                                                                        | 验证                                   |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------ |
| **P1.5 配置三块化**（与 §7 的 P1 合并执行，不单独成阶段）  | 顶层 `usdt` → `tron.usdt` 并**删除顶层 `usdt` 段与 `UsdtConfig` 类型**；新增 `bsc` / `sol` 段（含 `usdt` 子段）；`config/struct.go` 加 `TronConfig` / `BscConfig` / `SolConfig` 与各自的 `UsdtChannelConfig`；`config/example.json` 三块示例 | `go build ./...`；`go test ./config/` |
| **P7 BSC USDT** | `bsc` 包按 §11.3 补齐 8 个文件；`go-ethereum` 从 indirect 转 direct；`bsc.BscWithdraw/Query/Account` 从哨兵改为真实实现；`pay.Bind(KindBscUSDT, ...)`                                          | `go test ./bsc/`；`go vet ./...`      |
| **P8 SOL USDT** | `sol` 包按 §11.3 补齐 8 个文件；新增 `solana-go` 依赖；`sol.SolWithdraw/Query/Account` 改为真实实现；`pay.Bind(KindSolUSDT, ...)`                                                             | `go test ./sol/`；`go vet ./...`      |

`Kind` 常量已在 §3.3 定义为四值（`tron.usdt` / `bsc.usdt` / `sol.usdt` / `telegram.ton`），本阶段只需补 `Bind` 的两处调用。

`usdt` 包保留为 `tron.usdt` 的薄转发（`usdt.UsdtWithdraw` 签名不变），不新增 `bsc`/`sol` 的转发包。

### 11.6 风险（本节最高优先级）

| # | 风险                                  | 后果                              | 规避                                                                   |
| - | ----------------------------------- | ------------------------------- | -------------------------------------------------------------------- |
| 1 | **BSC USDT 18 位小数 vs TRON/SOL 6 位** | 上游按 6 位传 BSC 单 → 实发金额差 10^12，资损 | 配置显式 `decimals`；通道启动时校验；`PayOrder` 增可选 `Decimals` 字段做交叉校验，不一致直接拒单    |
| 2 | BSC 交易被抢跑/替换                        | 同一 nonce 重复广播                   | 按 `nonce` 串行化 + 台账先记账后广播（口径同 tron 的 `ledger.go`）                     |
| 3 | SOL 交易过期（blockhash 失效）              | 广播失败                            | 每次签名前取新 `blockhash`；失败重取，不缓存                                         |
| 4 | SOL 无 memo 机制                       | 归集账户认款困难                        | 沿用 `sol/address.go` 现有规则：填了 memo 直接拒单，不做静默忽略                         |
| 5 | 新增 `solana-go` 依赖                   | 依赖体积与供应链风险                      | 优先只用其 `transaction` / `rpc` 子包；若体积不可接受，自实现 Base58 + ed25519 + 最小 RPC |
| 6 | BSC `gas_price` 波动                  | 交易长时间不上链                        | 配置 `gas_price_gwei` 为下限，运行时按节点 `eth_gasPrice` 取更高值；超过配置上限则拒单         |
| 7 | 三份链上实现并行维护                          | 长期成本                            | 三包结构对齐（§11.3），共享 `pay` 的 `PayOrder`/`Party` 契约；不抽公共链抽象层（各链差异太大）      |

### 11.7 工作量

| 阶段          | 预估                    |
| ----------- | --------------------- |
| P1.5 配置三块化  | 0.5 天                 |
| P7 BSC USDT | 2–3 天（含测试）            |
| P8 SOL USDT | 3–4 天（含测试，SPL 序列化最耗时） |

### 11.8 需用户确认

1. BSC 与 SOL 的 USDT 是否都要支持**出款 + 查单 + 余额**三件，还是先只做出款？
2. SOL 是否接受引入 `solana-go` 依赖？若不接受，需自实现 RPC 与交易序列化，工作量翻倍。
3. 三条链私钥是否各自独立注入（`CallPayChainMsg+"tron.usdt"` / `"bsc.usdt"` / `"sol.usdt"`）？
4. BSC 的 USDT 合约是否固定用 `0x55d398326f99059fF775485246999027B3197955`（BSC 主网 USDT，18 位）？测试网 chapel 需要另一个地址。
