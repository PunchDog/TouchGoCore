# agents.md — pay

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

资金通道纯契约层：定义订单/回执形状、收款方与账户校验口径，以及「哪条通道已经起来了」的分派表。不 import config、不 import 任何通道包（读了就成环，或让契约层长出「按配置构造实现」的能力）。接新通道 = 在自己包里读配置 + pay.Bind + 改 config 的 driver。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `transfer.go` | 按种类拿通道 | 中 |
| `channel.go` | 按种类拿通道 | 中 |
| `types.go` | 订单与回执 | 中 |
| `party.go` | 收款方与金额校验 | 中 |
| `secret.go` | 密钥打码类型 | 极高 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `types.go` | 类型 PayOrder/PayResult/AccountQuery/AccountInfo/ProviderError；函数 IsTestnet/IsFinal/NormalizeAccountStatus/IsActive/ParseAmountUnits/AmountBig/CanWithdraw/Error | 中 |
| `party.go` | 类型 PartyKind/BankAccount/Party/WithdrawTarget；函数 PartyKinds/KindOf/Normalize/Check/CheckTarget/CheckOrder | 中 |
| `transfer.go` | 类型 Kind/Transfer；函数 Kinds/Bind/Unbind/TransferFor/TransferTo/BoundKinds | 中 |
| `channel.go` | 类型 Channel/OnchainAccountQuery；函数 ChannelFor/QueryOrderTo/QueryAccountTo/QueryOnchainAccountTo | 中 |
| `secret.go` | 类型 Secret；函数 String/GoString/MarshalJSON/Reveal | 极高 |

## 4. 对外接口 / 依赖

- **对外暴露**：`PayOrder`、`PayResult`、`AccountQuery`、`AccountInfo`、`ProviderError`、`IsTestnet`、`IsFinal`、`NormalizeAccountStatus`、`IsActive`、`ParseAmountUnits`、`AmountBig`、`CanWithdraw`、`Error`、`IsProviderRetryable`、`ProviderErrorOf`、`NetworkTRC20`、`PartyKind`、`BankAccount` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import `config`；禁止 import 任何通道包（tron/ton/bsc/sol/zalo/whatsapp/nft…）；禁止在此实现签名、广播、HTTP 传输。
- **依赖本目录的模块**：`bsc`、`chainroute`、`internal/httpx`、`sol`、`ton`、`tron`、`usdt`、`whatsapp`、`zalo`

## 5. 禁止跨模块直连约束

- 金额一律整数最小单位、手续费万分比整数，全链路禁浮点。
- 只实现 Withdraw 的旧装配方仍可 bind，但 ChannelFor 会明确拒绝。
- To.Kind=bank 直接报错；tron/bsc/sol 支持 From 携带地址+私钥+合约+网络，TON 收到按单私钥直接拒。
- 保持叶子层（零/极少内部依赖）：新增任何内部依赖都会把本包的约束传染给上层。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「配置要点」资金通道段（四条链事实内联）
- `docs/repo-layout.md`「会动钱的出款有四条」
- `docs/fund-defect-review.md` 资金链路历史缺陷

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增对 `config` 或任何通道包的 import？→ 禁止（成环或契约层长出实现）
- [ ] 是否改动 `PayOrder` / `PayResult` / `Channel` / `Transfer` 的字段或方法集？→ 必裁（全链路契约）
- [ ] 是否改动收款方校验（`CheckOrder`）或金额单位口径（禁浮点）？→ 必裁
- [ ] 是否新增暴露明文私钥的路径（`Secret.Reveal`）？→ 必裁（资金安全）
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
