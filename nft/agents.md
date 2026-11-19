# agents.md — nft

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L3
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

NFT 持有/详情/铸造/转账/查单的供应商适配层，与数字货币资金链路刻意不耦合：凭证登记在独立的顶层 nft_sdks 表，驱动走 nft.Register，密钥钩子是 util.CallNftSDKMsg+段名。包内不碰 SQL（落库/额度/撮合归下游业务）。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `nft.go` | 六个门面 | 中 |
| `NftStart / NftStop` | 起停 | 中 |
| `channel.go` | 接一家新供应商 | 中 |
| `provider.go` | 接一家新供应商 | 中 |
| `reconcile.go` | UNKNOWN 单处理 | 极高 |
| `types.go` | 订单/回执/错误口径 | 中 |
| `client.go` | HTTP 客户端与传输装配 | 中 |
| `wire.go` | HTTP 客户端与传输装配 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `provider.go` | 类型 ExtraField/ProviderOptions/Provider/MintRequest/TransferRequest/QueryRequest；函数 SignMD5/SetRetryableCodes/DeleteRetryableCode/IsRetryableCode/NewProvider/BaseURL/Driver/MerchantID | 中 |
| `client.go` | 类型 Options/Client/CallSpec；函数 NewClient/BaseURL/Do/DefaultParse | 中 |
| `types.go` | 类型 NftOrder/NftResult/HoldingsQuery/HoldingItem/HoldingsResult/TokenQuery；函数 IsTestnet/IsFinal/Error/IsProviderRetryable/ProviderErrorOf | 中 |
| `nft.go` | 函数 NftStart/NftStop/NftMint/NftTransfer/NftQuery/NftHoldings/NftToken | 中 |
| `wire.go` | 内部实现 | 中 |
| `channel.go` | 类型 NftChannel/Factory；函数 Register/Open/Drivers | 中 |
| `reconcile.go` | 函数 NftReconcile/NeedsReconcile/InSubmitDoubt | 极高 |

## 4. 对外接口 / 依赖

- **对外暴露**：`ExtraField`、`ProviderOptions`、`Provider`、`MintRequest`、`TransferRequest`、`QueryRequest`、`HoldingsRequest`、`TokenRequest`、`OrderData`、`HoldingsData`、`TokenData`、`SignMD5`、`SetRetryableCodes`、`DeleteRetryableCode`、`IsRetryableCode`、`NewProvider`、`BaseURL`、`Driver` 等
- **允许依赖**：`config`、`corectx`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- nft_sdks.<段>.endpoints 只有 holdings/token/mint/transfer/query 五个合法键，写错启动即报错。
- nft.provider 留空即通道不启用，nft_sdks.<段>.enable 不为 on 即凭证不可用。
- token_id 等链上大整数一律十进制字符串，禁 float64。
- 跨层依赖只向 `config`、`corectx`、`util`、`vars` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「配置要点」资金通道段（四条链事实内联）
- `docs/repo-layout.md`「会动钱的出款有四条」
- `docs/fund-defect-review.md` 资金链路历史缺陷

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否触及幂等键、金额精度（禁浮点）、对账口径或状态机？→ 必裁
- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
