# agents.md — websocket

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L3
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

WS 升级、鉴权、按类名从对象池取回调实例派发消息。默认单线程串行：业务回调只跑在 Tick 这一个消费协程上（除非 cfg.ws.worker_pool_size>0 开 Worker 池）。回调实例与 *Client 都是池化的，字段必须在 OnConnect 里显式复位。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `doc.go` | 并发模型与装配顺序（必读） | 中 |
| `websocket.go` | 注册回调与起停 | 中 |
| `UseClientMap / UseShardedClientMap / GetClient / GetServerStats / WorkerPoolFullCount` | 客户端表与统计 | 中 |
| `client.go` | 客户端侧 | 中 |
| `server.go` | HTTP 服务与内网 IP 判定 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `websocket.go` | 类型 AuthFunc；函数 SetAuthFunc/GetAuthFunc/WorkerPoolFullCount/OnConnect/OnMessage/OnClose/RegisterCall/UseClientMap | 中 |
| `client.go` | 类型 ClientStats/ICall/Client；函数 Do/IsClose/Connected/Close/SendMsg/NewClient/GetStats/UpdateStatsFromMessage | 中 |
| `server.go` | 函数 ListenAndServe/IsIntranetIP | 中 |
| `doc.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`AuthFunc`、`SetAuthFunc`、`GetAuthFunc`、`WorkerPoolFullCount`、`OnConnect`、`OnMessage`、`OnClose`、`RegisterCall`、`UseClientMap`、`UseShardedClientMap`、`GetClient`、`Run`、`Stop`、`Tick`、`GetServerStats`、`UpdateConnectionStats`、`UpdateMessageStats`、`UpdateErrorStats` 等
- **允许依赖**：`config`、`corectx`、`metrics`、`syncmap`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`metrics`、`syncmap`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`example/gatewayserver`、`example/loginserver`

## 5. 禁止跨模块直连约束

- OnConnect/OnClose 与 OnMessage 始终并发，跨这两处的共享状态一定要加锁。
- RegisterCall 必须在 Run 之前完成，否则连接报「未找到类名对应的 ICall 接口实现」。
- 跨层依赖只向 `config`、`corectx`、`metrics`、`syncmap`、`util`、`vars` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `websocket/doc.go` 并发模型与装配顺序
- `docs/repo-layout.md` L3 分层

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否改变默认单线程假设或 Worker 池/分片派发语义？→ 必裁
- [ ] 是否让回调实例或 `*Client` 在池化归还后被继续持有？→ 禁止（读到下一个连接的字段）
- [ ] 是否把 `RegisterCall` 挪到 `Run` 之后？→ 禁止（连接报未找到 ICall 实现）
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
