# agents.md — metrics

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L1
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

Prometheus 指标与注册表单例。根包 metrics.go 只负责把 /metrics HTTP 端点服务起来（含 pprof、token 鉴权），指标本身都在这里定义与注册。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `metrics.go` | 初始化与拿注册表 | 中 |
| `IncConnection / DecConnection / IncMessages / IncErrors / IncRequests` | 连接与消息计数 | 中 |
| `ObserveLatency / SetActive / IncExecutions / SetInstances / ObserveCallLatency` | 延迟与任务观测 | 中 |
| `WS / RPC / HTTP / Timer / Lua / DB / Log / DBCache` | 按组件维度 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `metrics.go` | 函数 Init/Registry/SetConnections/IncConnection/DecConnection/IncMessages/IncErrors/IncRequests | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Init`、`Registry`、`SetConnections`、`IncConnection`、`DecConnection`、`IncMessages`、`IncErrors`、`IncRequests`、`ObserveLatency`、`SetActive`、`IncExecutions`、`SetInstances`、`IncCalls`、`ObserveCallLatency`、`SetSinkLag`、`IncTxConflict`、`IncScanBudget`、`IncSinkAbsent` 等
- **允许依赖**：`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`db`、`db/dbcache`、`golua`、`rpc`、`websocket`

## 5. 禁止跨模块直连约束

- 根包 metrics.go 里那批同名变量是历史别名，不是重复实现；新代码直接用本包。
- 跨层依赖只向 `vars` 方向；禁止反向引用上层包。
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
