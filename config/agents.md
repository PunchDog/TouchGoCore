# agents.md — config

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L1
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

配置加载、结构与校验。唯一入口是 `-c/--config` 指向的 conf 目录（内含 config.ini 与各服 JSON）；未指定时回退环境变量 CONFIG_PATH，再自动查找。加载后交给 Validate 校验，结构体集中在 struct.go。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `config.go` | 读配置入口 | 高 |
| `struct.go` | 找某个配置字段 | 中 |
| `validate.go` | 启动时校验报错文案 | 高 |
| `wa.go` | 改 WhatsApp/Zalo 默认值 | 中 |
| `zalo.go` | 改 WhatsApp/Zalo 默认值 | 中 |
| `example.json` | 看完整示例 | 中 |
| `exampleconfig.ini` | 看完整示例 | 高 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `struct.go` | 类型 MySqlDBConfig/PostgreDBConfig/MongoTableIndex/MongoDBConfig/RedisConfig/CacheConfig；函数 Enabled/Endpoint/Account/EffectiveProvider/DefaultModel/StrategyOf/StrategyValid/Empty | 中 |
| `validate.go` | 函数 Validate | 高 |
| `config.go` | 类型 Cfg；函数 TimerGoroutineLimit/ApplyFlags/Load/LoadWithError/QueueCapacity/WriteQueueCapacity/DropOnFull/GetBasePath | 高 |
| `wa.go` | 类型 WaSendRate；函数 ParseWaSendRate/EffectiveSendRate/EffectiveConnectTimeout/EffectiveReconnectMax/EffectiveReconnectMin/EffectiveMaxConcurrent/EffectiveSendMaxDelay/EffectiveSendMinDelay | 中 |
| `zalo.go` | 函数 EffectiveQRTTL/EffectiveRequestTimeout/EffectiveListenerEnabled/EffectiveFriendsCacheTTL/EffectiveGroupsCacheTTL/EffectiveInfoMinInterval/EffectiveInfoCacheTTL/EffectiveSendMaxDelay | 中 |
| `example_feature_config.go` | 类型 GameRulesConfig/RewardConfig/FeatureFlags | 高 |

## 4. 对外接口 / 依赖

- **对外暴露**：`MySqlDBConfig`、`PostgreDBConfig`、`MongoTableIndex`、`MongoDBConfig`、`RedisConfig`、`CacheConfig`、`TLSConfig`、`WebConfig`、`WebsocketPort`、`WebsocketConfig`、`RpcAddr`、`RpcTLSConfig`、`RpcAuthConfig`、`RpcConfig`、`TelegramConfig`、`ChannelAccountConfig`、`ChannelSupplierConfig`、`WhatsappConfig` 等
- **允许依赖**：`ini`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止在业务包里另建配置解析（ config 是唯一入口）；禁止把密钥写进版本库（走 `util.Call*Msg` 钩子注入）。
- **依赖本目录的模块**：`.`、`ai`、`bsc`、`chainroute`、`corectx`、`db`、`db/dbcache`、`db/mysql`、`db/postgres`、`golua`、`nft`、`rpc`、`sol`、`telegram`

## 5. 禁止跨模块直连约束

- 资金通道配置刻意内联：每链一段（tron.usdt / ton.native / bsc.usdt / sol.usdt），无集中登记表、无跨段 provider 引用。
- gRPC 用 `rpc` 字段；WebSocket 路径取 `ws.url` / `ws.inurl`。
- 跨层依赖只向 `ini` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「配置要点」
- `config/example.json`、`config/exampleconfig.ini`

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/重命名配置键或段名？→ 必裁，并同步 `config/example.json` 与各 `agents.md`
- [ ] 是否改动资金通道段的内联结构（每链一段、无集中登记表）？→ 必裁
- [ ] 是否让 `Validate` 对缺段/错值静默放行？→ 禁止（启动即失败优于运行期错配）
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
