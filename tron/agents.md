# agents.md — tron

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

TRON 链上资金实现。会动钱的只有 USDT-TRC20：本地组交易、热钱包私钥签名、经节点 gRPC 广播；账户查询读供应商账本。原生 TRX 门面返回 ErrPayoutNotWired。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `account.go` | 账户与商户账本查询 | 中 |
| `query.go` | 账户与商户账本查询 | 中 |
| `address.go` | 地址校验规则 | 中 |
| `onchain.go` | 链上出款 | 高 |
| `driver.go` | 链上出款 | 中 |
| `gateway.go` | 网关与广播（gRPC/HTTP） | 高 |
| `gateway_grpc.go` | 网关与广播（gRPC/HTTP） | 高 |
| `encoding.go` | 编码与链规则 | 中 |
| `chainrule.go` | 编码与链规则 | 极高 |
| `ledger.go` | 幂等台账 | 极高 |
| `usdt.go` | USDT 与原生币门面 | 中 |
| `tron.go` | USDT 与原生币门面 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `onchain.go` | 函数 Driver/MerchantID/Close/QueryAccount/QueryOnchainAccount/Withdraw | 高 |
| `account.go` | 函数 QueryAccount | 中 |
| `gateway_http.go` | 内部实现 | 高 |
| `gateway_grpc.go` | 内部实现 | 高 |
| `usdt.go` | 函数 UsdtWithdraw/UsdtQuery/UsdtAccount | 中 |
| `query.go` | 函数 QueryOrder | 中 |
| `encoding.go` | 内部实现 | 中 |
| `gateway.go` | 内部实现 | 高 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Driver`、`MerchantID`、`Close`、`QueryAccount`、`QueryOnchainAccount`、`Withdraw`、`UsdtWithdraw`、`UsdtQuery`、`UsdtAccount`、`QueryOrder`、`TRONRule`、`IsValidTRONAddress`、`CheckAddress`、`CheckMemo`、`TronWithdraw`、`TronQuery`、`TronAccount`、`ErrPayoutNotWired` 等
- **允许依赖**：`chainroute`、`config`、`internal/httpx`、`ledger`、`pay`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`chainroute`、`config`、`internal/httpx`、`ledger`、`pay`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`usdt`

## 5. 禁止跨模块直连约束

- 支持按单出款（From 携带地址+私钥+合约+网络）；供应商签名密钥经 util.CallPaySDKMsg+"tron.usdt" 注入。
- 跨层依赖只向 `chainroute`、`config`、`internal/httpx`、`ledger`、`pay`、`util` 方向；禁止反向引用上层包。
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
