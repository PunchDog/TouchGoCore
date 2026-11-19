# agents.md — network/message

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

框架内所有 protobuf 消息与 gRPC 服务的生成代码：Head/FSMessage 通用消息壳、内部 RPC 服务、HTTP↔gRPC 代理消息。改协议要改 .proto 后重新生成，不要手改 .pb.go。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `FSMessage.pb.go` | 通用消息壳 | 中 |
| `rpc.pb.go` | 内部 RPC 契约 | 中 |
| `rpc_grpc.pb.go` | 内部 RPC 契约 | 中 |
| `ginproxy.pb.go` | HTTP↔gRPC 代理消息 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `ginproxy.pb.go` | 类型 GinRoute/GinRouteRegistration/GinRegisterAck/GinParam/GinHeader/GinHTTPRequest；函数 Reset/String/ProtoMessage/ProtoReflect/Descriptor/GetUrlPath/GetMethod/GetRoutes | 中 |
| `FSMessage.pb.go` | 类型 Head/FSMessage；函数 Reset/String/ProtoMessage/ProtoReflect/Descriptor/GetProtocol1/GetProtocol2/GetCmd | 中 |
| `rpc_grpc.pb.go` | 类型 GrpcClient/Grpc_MsgClient/GrpcServer/UnimplementedGrpcServer/UnsafeGrpcServer/Grpc_MsgServer；函数 NewGrpcClient/Msg/RegisterGrpcServer | 中 |
| `rpc.pb.go` | 内部实现 | 中 |
| `gen_desc.go` | 构建标签 ignore，仅示例 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`GinRoute`、`GinRouteRegistration`、`GinRegisterAck`、`GinParam`、`GinHeader`、`GinHTTPRequest`、`GinHTTPResponse`、`Reset`、`String`、`ProtoMessage`、`ProtoReflect`、`Descriptor`、`GetUrlPath`、`GetMethod`、`GetRoutes`、`GetOk`、`GetMsg`、`GetKey` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import 任何 `touchgocore/*` 业务包（叶子层）；禁止反向依赖上层。
- **依赖本目录的模块**：`rpc`、`util`

## 5. 禁止跨模块直连约束

- 保持叶子层（零/极少内部依赖）：新增任何内部依赖都会把本包的约束传染给上层。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`（框架总说明）
- `docs/repo-layout.md`（依赖分层与包清单）
- 包内 `doc.go` 与文件头注释

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增内部依赖？→ 必裁（叶子层纪律）
- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
