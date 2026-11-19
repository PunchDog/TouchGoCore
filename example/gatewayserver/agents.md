# agents.md — example/gatewayserver

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：—
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

WebSocket 网关的参考实现，把框架各组件接成一个可运行进程，并通过 gatepb 协议与登录服通信。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `gatewayserver.go` | 进程入口 | 高 |
| `websocketcall.go` | WS 侧调用封装 | 中 |
| `gatepb/gate.proto` | 协议定义 | 中 |
| `gatepb/gate.pb.go` | 协议定义 | 中 |
| `logs/` | 运行日志样例 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `gatewayserver.go` | 内部实现 | 高 |
| `websocketcall.go` | 类型 GateMsg；函数 OnConnect/OnMessage/OnClose | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`GateMsg`、`OnConnect`、`OnMessage`、`OnClose` 等
- **允许依赖**：`.`、`example/gatewayserver/gatepb`、`util`、`vars`、`websocket`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`.`、`example/gatewayserver/gatepb`、`util`、`vars`、`websocket`）；禁止反向依赖上层。
- **依赖本目录的模块**：—

## 5. 禁止跨模块直连约束

- 编译产物 *.exe 已被 .gitignore 排除。
- 跨层依赖只向 `.`、`example/gatewayserver/gatepb`、`util`、`vars`、`websocket` 方向；禁止反向引用上层包。
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
