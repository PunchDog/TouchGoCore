# agents.md — ton

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

TON 链上资金实现（配置段 ton.native）：用 tongo 组外部消息、本地热钱包签名，经 chain.http_base（TonAPI）广播与查证；金额单位是纳吨。不支持按单私钥（地址还取决于 wallet_version / sub_wallet_id），收到明确拒单。本仓不接 lite server，原因见 gateway_tonapi.go 头部注释。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `account.go` | 账户与余额查询 | 中 |
| `address.go` | 地址校验 | 中 |
| `gateway.go` | 网关与广播 | 高 |
| `gateway_tonapi.go` | 网关与广播 | 高 |
| `onchain.go` | 链上动作 | 高 |
| `driver.go` | 链上动作 | 中 |
| `ledger.go` | 幂等台账与路由 | 极高 |
| `route.go` | 幂等台账与路由 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `onchain.go` | 函数 Driver/Close/Withdraw | 高 |
| `wallet.go` | 内部实现 | 中 |
| `gateway_tonapi.go` | 内部实现 | 高 |
| `query.go` | 函数 QueryOrder | 中 |
| `address.go` | 函数 IsValidTONAddress/CheckAddress/CheckMemo | 中 |
| `native.go` | 函数 TonWithdraw/TonQuery/TonAccount | 中 |
| `gateway.go` | 内部实现 | 高 |
| `account.go` | 函数 QueryAccount/QueryOnchainAccount | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Driver`、`Close`、`Withdraw`、`QueryOrder`、`IsValidTONAddress`、`CheckAddress`、`CheckMemo`、`TonWithdraw`、`TonQuery`、`TonAccount`、`QueryAccount`、`QueryOnchainAccount`、`TonStart`、`TonStop`、`SetJournal`、`ErrOrderAlreadySettled`、`ErrJournalUnavailable`、`DriverTonAPI` 等
- **允许依赖**：`chainroute`、`config`、`ledger`、`pay`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`chainroute`、`config`、`ledger`、`pay`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- 只接受 PayOrder.Amount 的 int64 最小单位，amount_units 会被显式拒绝。
- 跨层依赖只向 `chainroute`、`config`、`ledger`、`pay`、`util`、`vars` 方向；禁止反向引用上层包。
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
