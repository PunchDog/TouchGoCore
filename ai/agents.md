# agents.md — ai

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L3
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

对上游提供统一的大模型 Chat 门面：多提供方客户端注册、模型选择策略、价格查询与成本估算、并发/RPM 压力跟踪与过载保护。依赖 config、corectx、vars，被根包 services.go 与 mapmanager 引用。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `facade.go` | 发一次对话请求 | 中 |
| `client.go` | 看支持的模型与限流 | 中 |
| `pressure.go` | 判断是否过载 | 中 |
| `strategy.go` | 算 token 成本 / 选模型策略 | 中 |
| `price.go` | 算 token 成本 / 选模型策略 | 中 |
| `types.go` | 改请求响应结构 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `client.go` | 类型 Client；函数 Name/DefaultModel/Models/HasModel/NewClient/LimitsFor/Chat/Error | 中 |
| `price.go` | 类型 PriceQuote/PriceSource；函数 NewPriceSource/Lookup/Prefetch | 中 |
| `facade.go` | 函数 Run/Stop/GetClient/Chat/SelectModel/CurrentStrategy/ListProviders | 中 |
| `strategy.go` | 类型 Strategy/Selection；函数 ParseStrategy/String/EstimatePromptTokens/EstimateCost | 中 |
| `pressure.go` | 类型 PressureTracker；函数 NewPressureTracker/Reset/Overloaded/Acquire | 中 |
| `types.go` | 类型 Role/Message/FunctionCall/ToolCall/FunctionDef/Tool；函数 Priced/Error | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Client`、`Name`、`DefaultModel`、`Models`、`HasModel`、`NewClient`、`LimitsFor`、`Chat`、`Error`、`Unwrap`、`PriceQuote`、`PriceSource`、`NewPriceSource`、`Lookup`、`Prefetch`、`Run`、`Stop`、`GetClient` 等
- **允许依赖**：`config`、`corectx`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- facade 是全局单例，状态由 facadeMu 保护；读路径 RLock、写路径（Run/Stop）Lock。
- 跨层依赖只向 `config`、`corectx`、`vars` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`（框架总说明）
- `docs/repo-layout.md`（依赖分层与包清单）
- 包内 `doc.go` 与文件头注释

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
