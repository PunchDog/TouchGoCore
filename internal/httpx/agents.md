# agents.md — internal/httpx

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L1
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

统一的 HTTP 客户端：超时、指数退避、Retry-After、响应体上限都在这里，错误分类复用 `*pay.ProviderError`。方向只能是 httpx → pay；它取代了旧 pay 内嵌的客户端与 paysdk 装配接缝。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `client.go` | 建客户端与发请求 | 中 |
| `retry.go` | 重试哪些状态码 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `client.go` | 类型 Options/Client/CallSpec；函数 NewClient/BaseURL/Do/DefaultParse/StrictParse | 中 |
| `retry.go` | 函数 SetRetryableCodes/DeleteRetryableCode/IsRetryableCode/LenRetryableCodes | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Options`、`Client`、`CallSpec`、`NewClient`、`BaseURL`、`Do`、`DefaultParse`、`StrictParse`、`SetRetryableCodes`、`DeleteRetryableCode`、`IsRetryableCode`、`LenRetryableCodes` 等
- **允许依赖**：`pay`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`pay`）；禁止反向依赖上层。
- **依赖本目录的模块**：`tron`

## 5. 禁止跨模块直连约束

- nft/client.go 是同构的另一份实现（不同供应商域），不共用本包。
- 跨层依赖只向 `pay` 方向；禁止反向引用上层包。
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
