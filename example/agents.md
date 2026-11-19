# agents.md — example

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：—
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

三个可编译运行的演示进程，是框架装配的参考实现：gatewayserver（WebSocket 网关，含 gatepb 协议）、loginserver（登录服）。不是独立 module，go build ./... 会连带编译，改导出 API 时这里的编译错误是真错误。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `README.md` | 看怎么装配一个服 | 中 |
| `gatewayserver/gatewayserver.go` | 起网关 | 高 |
| `websocketcall.go` | 起网关 | 中 |
| `loginserver/loginserver.go` | 起登录服 | 中 |
| `conf/config.ini` | 看配置文件长什么样 | 高 |
| `conf/gatewayserverbus.json` | 看配置文件长什么样 | 高 |
| `deploy/touchgo.service` | 部署脚本 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| README.md | 看怎么装配一个服 | 中 |
| gatewayserver/gatewayserver.go、websocketcall.go；协议 gatepb/gate.proto | 起网关 | 高 |
| loginserver/loginserver.go | 起登录服 | 中 |
| conf/config.ini、conf/gatewayserverbus.json | 看配置文件长什么样 | 高 |
| deploy/touchgo.service | 部署脚本 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：见「入口文件」表的对外门面
- **允许依赖**：—
- **禁止依赖**：禁止把过程文档/产物目录里的约定复制到代码形成第二事实源。
- **依赖本目录的模块**：—

## 5. 禁止跨模块直连约束

- 启动：./gatewayserver -c example/conf，-c 指向 conf 目录。
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
