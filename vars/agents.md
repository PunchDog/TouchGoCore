# agents.md — vars

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

全仓最底层的共用件：日志器、异步日志通道、按 channel 分文件的日志管理、批量与滚动写入、slog/zap handler。零内部依赖且被 18 个包直接引用——给它加依赖等于给全仓加依赖，基本不可接受。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `vars.go` | 打日志入口 | 中 |
| `log_handlers.go` | 打日志入口 | 中 |
| `async_channel.go` | 异步/按 channel 日志 | 中 |
| `channel_logger_manager.go` | 异步/按 channel 日志 | 中 |
| `log_batch.go` | 写入侧（批量、滚动、同步） | 中 |
| `rotating_writer.go` | 写入侧（批量、滚动、同步） | 中 |
| `log_config.go` | 配置与调用方信息 | 高 |
| `log_caller.go` | 配置与调用方信息 | 中 |
| `example_usage.go` | 照抄用法 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `channel_logger_manager.go` | 类型 ChannelLoggerManager；函数 NewChannelLoggerManager/LogAsync/LogAsyncSimple/GetLogger/Flush/Close/IsEnabled/IsOff | 中 |
| `rotating_writer.go` | 类型 RotatingFileWriter；函数 NewRotatingFileWriter/Write/Sync/Close | 中 |
| `async_channel.go` | 类型 AsyncChannelConfig/AsyncChannelStats/AsyncLoggerChannel；函数 DefaultAsyncChannelConfig/NewAsyncLoggerChannel/Enqueue/EnqueueSimple/Flush/FlushAndWait/Close/GetStats | 中 |
| `vars.go` | 函数 Run/Initialize/InitializeWithDefaults/Shutdown/FlushConsole/CloseConsole/Debug/Info | 中 |
| `sync_logger.go` | 类型 LoggerManager/ZapSlogHandler；函数 NewLoggerManager/Close/GetLogger/SetLevel/IsEnabled/NewZapSlogHandler/Enabled/Handle | 中 |
| `log_handlers.go` | 类型 OptimizedZapSlogHandler；函数 OptimizeCallerEncoder/NewOptimizedZapSlogHandler/WithGlobalAttrs/Enabled/Handle/WithAttrs/WithGroup | 中 |
| `example_usage.go` | 函数 ExampleUsage/ExampleAdvancedUsage/ExampleContextLogging/ExampleErrorHandling/ExampleLogRotation/ExampleAsyncLogging/ExampleFieldUsage/ExampleProductionUsage | 中 |
| `async_writer.go` | 类型 AsyncWriteSyncer；函数 NewAsyncWriteSyncer/Write/Sync/Close/Dropped | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`ChannelLoggerManager`、`NewChannelLoggerManager`、`LogAsync`、`LogAsyncSimple`、`GetLogger`、`Flush`、`Close`、`IsEnabled`、`IsOff`、`ShouldWriteFile`、`GetStats`、`IsHealthy`、`GetLoadFactor`、`SetLevel`、`InitializeChannelLogger`、`InitializeChannelLoggerWithDefaults`、`ShutdownChannelLogger`、`GetChannelLogger` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import 任何 `touchgocore/*` 业务包；禁止在日志路径做阻塞式网络/DB IO。
- **依赖本目录的模块**：`.`、`ai`、`bsc`、`chainroute`、`db`、`db/dbcache`、`db/mysql`、`db/postgres`、`example/gatewayserver`、`example/loginserver`、`gin`、`golua`、`list`、`localtimer`

## 5. 禁止跨模块直连约束

- 保持零内部依赖；禁止在日志路径做阻塞式网络/DB。
- 保持叶子层（零/极少内部依赖）：新增任何内部依赖都会把本包的约束传染给上层。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `docs/repo-layout.md` L0 vars 的锁纪律说明

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否给 `vars` 增加了内部依赖？→ 禁止（全仓循环依赖）
- [ ] 是否改变日志格式、级别判定或 channel 路由？→ 必裁
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
