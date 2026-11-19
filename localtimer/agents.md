# agents.md — localtimer

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L4
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

自带状态表的分层时间轮：注册/暂停/恢复/移除定时器，支持单次与循环、异步 Tick、调度分片（多通道）与积压统计。RPC、websocket、telegram 等都架在它上面。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `timer_facade.go` | 对外主入口 | 中 |
| `timer.go` | 定时器对象与状态 | 中 |
| `timer_manager.go` | 管理器与注册表 | 中 |
| `timer_registry.go` | 管理器与注册表 | 中 |
| `timer_wheel.go` | 时间轮与槽位 | 中 |
| `wheel_slots.go` | 时间轮与槽位 | 中 |
| `schedule_shards.go` | 调度分片与调度项状态表 | 中 |
| `state_table.go` | 调度分片与调度项状态表 | 中 |
| `timer_close.go` | 关闭、通道与统计 | 中 |
| `timer_channel.go` | 关闭、通道与统计 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `timer.go` | 类型 TimerType/TimerInterface/TimerPool/TimerPoolStats/Timer；函数 String/GetTimerPoolStats/Get/Put/Init/SetSelf/RemoveFromManager/Remove | 中 |
| `timer_wheel.go` | 类型 TimerWheel | 中 |
| `timer_registry.go` | 类型 AsyncWorkerStats；函数 SetAsyncWorkerLimit/GetAsyncWorkerStats | 中 |
| `timer_facade.go` | 函数 Run/TimeStop/AddTimer/TimeTick/GetDefaultManager/IsSystemRunning/GetSystemStats/GetQueueStats | 中 |
| `wheel_slots.go` | 函数 Add/Length/Clear/Range/RangeWindow | 中 |
| `state_table.go` | 内部实现 | 中 |
| `timer_add.go` | 函数 AddTimer | 中 |
| `timer_stats.go` | 类型 TimerStats/TimerQueueStats；函数 GetStats/GetTimerCount/GetWheelTimerCount/GetQueueStats | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`TimerType`、`TimerInterface`、`TimerPool`、`TimerPoolStats`、`Timer`、`String`、`GetTimerPoolStats`、`Get`、`Put`、`Init`、`SetSelf`、`RemoveFromManager`、`Remove`、`Pause`、`TryAdvance`、`HasNext`、`GetUID`、`GetParent` 等
- **允许依赖**：`list`、`syncmap`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`list`、`syncmap`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`db/dbcache`、`golua`、`localtimer/internal/dupname`、`rpc`、`telegram`

## 5. 禁止跨模块直连约束

- 积压深度采集见根包 metrics_timer.go（拉取式 Collector）。
- worker 协程上限由 cfg.TimerGoroutineLimit() 决定。
- 跨层依赖只向 `list`、`syncmap`、`util`、`vars` 方向；禁止反向引用上层包。
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
