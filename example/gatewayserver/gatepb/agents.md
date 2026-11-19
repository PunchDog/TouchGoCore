# agents.md — example/gatewayserver/gatepb

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：—
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

示例网关与登录服之间 gRPC 协议的生成代码（.proto 与 .pb.go）。改协议需改 .proto 后重新生成。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `gate.proto` | 协议源文件 | 中 |
| `gate.pb.go` | 生成代码 | 中 |
| `gate_test.go` | 协议自测 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `gate.pb.go` | 类型 GatePing/GatePong；函数 Reset/String/ProtoMessage/ProtoReflect/GetPayload | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`GatePing`、`GatePong`、`Reset`、`String`、`ProtoMessage`、`ProtoReflect`、`GetPayload`、`File_gate_proto` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import 任何 `touchgocore/*` 业务包（叶子层）；禁止反向依赖上层。
- **依赖本目录的模块**：`example/gatewayserver`、`example/loginserver`

## 5. 禁止跨模块直连约束

- 禁止把 `README.md` / `docs/` 的内容复制进代码形成第二事实源；只允许引用章节锚点。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `example/README.md`
- `README.md`「快速开始」

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
