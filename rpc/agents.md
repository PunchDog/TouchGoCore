# agents.md — rpc

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

进程间通信层：服务端/客户端、服务发现（静态/DNS）、鉴权、事件协议注册与回调，外加 HTTP↔gRPC 双向代理（ginproxy_*）。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `run.go` | 起停与服务端 | 中 |
| `server.go` | 起停与服务端 | 中 |
| `client.go` | 调用对端 | 中 |
| `discovery.go` | 服务发现 | 中 |
| `callbacks.go` | 回调与鉴权 | 中 |
| `auth.go` | 回调与鉴权 | 高 |
| `events.go` | 事件协议白名单 | 中 |
| `ginproxy.go` | HTTP↔gRPC 代理 | 中 |
| `ginproxy_client.go` | HTTP↔gRPC 代理 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `server.go` | 类型 MessageInfo/RpcServer；函数 SetHandlerTimeout/SetShutdownBudget/Msg/Send/SendWithRequestID/Stop/StartGrpcServer/SetCallbacks | 中 |
| `client.go` | 类型 RpcClient；函数 Close/IsClosed/Tick/SendMsg/SendMsgCtx/NewRpcClient/SetCallbacks/GetCallbacks | 中 |
| `run.go` | 函数 Run/ResolveService/Stop/UseRegistry/GetRpcClient/GetRpcServer/SetClientCallbacks/SetServerCallbacks | 中 |
| `ginproxy_client.go` | 类型 GinProxyRequest/GinProxyResponse/GinProxyHandler；函数 RegisterGinProxyHandler | 中 |
| `auth.go` | 内部实现 | 高 |
| `discovery.go` | 类型 ServiceEndpoint/ServiceDiscovery/StaticDiscovery/DNSDiscovery/DiscoveryManager；函数 NewStaticDiscovery/Resolve/Watch/Close/NewDNSDiscovery/InitDiscovery/GetDiscovery/SetDiscovery | 中 |
| `ginproxy_server.go` | 函数 Do | 中 |
| `ginproxy.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`MessageInfo`、`RpcServer`、`SetHandlerTimeout`、`SetShutdownBudget`、`Msg`、`Send`、`SendWithRequestID`、`Stop`、`StartGrpcServer`、`SetCallbacks`、`GetCallbacks`、`ErrorCmd`、`RpcClient`、`Close`、`IsClosed`、`Tick`、`SendMsg`、`SendMsgCtx` 等
- **允许依赖**：`config`、`corectx`、`gin`、`localtimer`、`metrics`、`network/message`、`syncmap`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`gin`、`localtimer`、`metrics`、`network/message`、`syncmap`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `config`、`corectx`、`gin`、`localtimer`、`metrics`、`network/message` 方向；禁止反向引用上层包。
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
