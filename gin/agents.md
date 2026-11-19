# agents.md — gin

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L3
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

封装 gin 的路由注册与启停，并提供「HTTP 请求 ↔ 内部 gRPC 服务」的双向代理：把 HTTP 调用转成 gRPC 流式调用，也允许 gRPC 侧回调 HTTP 路由。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `run.go` | 注册并起停 HTTP 服务 | 中 |
| `routepath.go` | 按 URL 路径映射路由 | 中 |
| `dispatch.go` | 按 URL 路径映射路由 | 中 |
| `grpcproxy.go` | 起一条 gRPC 代理 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `run.go` | 类型 IRouterInterface/IRouterTimeout；函数 RegisterRouter/Run/Stop | 中 |
| `dispatch.go` | 函数 MatchedRoute | 中 |
| `grpcproxy.go` | 类型 ProxyRequest/ProxyResponse/GRPCProxyStream；函数 RegisterGRPCRoute/UnregisterGRPCRoutes | 中 |
| `routepath.go` | 类型 IRouterPath | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`IRouterInterface`、`IRouterTimeout`、`RegisterRouter`、`Run`、`Stop`、`MatchedRoute`、`ProxyRequest`、`ProxyResponse`、`GRPCProxyStream`、`RegisterGRPCRoute`、`UnregisterGRPCRoutes`、`IRouterPath` 等
- **允许依赖**：`corectx`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`corectx`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`rpc`、`whatsapp`、`zalo`

## 5. 禁止跨模块直连约束

- 配套的 gRPC 侧实现在 rpc/ginproxy_*.go。
- 跨层依赖只向 `corectx`、`util`、`vars` 方向；禁止反向引用上层包。
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
