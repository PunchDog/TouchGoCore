# agents.md — 根目录（装配层）

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L7
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

Go 游戏服务框架 TouchGoCore（module `touchgocore`）。根包只做装配：`NewApp` 把 22 个内部组件接进一个带生命周期的依赖注入容器，`Run(serverName)` 是进程总入口。自身几乎没有可复用能力，新能力应落到对应组件包，不要落根包。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `README.md` | 要跑起来 / 看配置怎么读 | 中 |
| `example/README.md` | 要跑起来 / 看配置怎么读 | 中 |
| `app.go` | 改启动流程与模块装配 | 中 |
| `services.go` | 改启动流程与模块装配 | 中 |
| `metrics.go` | 加 Prometheus 指标定义 | 中 |
| `docs/repo-layout.md` | 查包该不该互相引用、新代码放哪 | 中 |
| `docs/testing-conventions.md` | 看测试与提交约定 | 中 |
| `agents.md` | 找现成能力在哪个包 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `app.go` | 类型 App/Service；函数 NewApp/Start/Shutdown/GetApp/Context/GetRpcClient/GetRpcServer/GetWSClient | 中 |
| `services.go` | 函数 Name/Start/Stop | 中 |
| `metrics.go` | 函数 InitMetrics/StartMetricsServer/StartMetrics/ShutdownMetrics | 中 |
| `metrics_timer.go` | 函数 Describe/Collect | 中 |
| `run.go` | 函数 Run/RunWithApp | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`App`、`Service`、`NewApp`、`Start`、`Shutdown`、`GetApp`、`Context`、`GetRpcClient`、`GetRpcServer`、`GetWSClient`、`Name`、`Stop`、`InitMetrics`、`StartMetricsServer`、`StartMetrics`、`ShutdownMetrics`、`Describe`、`Collect` 等
- **允许依赖**：`ai`、`bsc`、`config`、`corectx`、`db`、`db/dbmap`、`gin`、`golua`、`ini`、`localtimer`、`mapmanager`、`metrics`、`nft`、`rpc`、`sol`、`syncmap`、`telegram`、`ton`、`usdt`、`util`、`vars`、`websocket`、`whatsapp`、`zalo`（内部依赖，由 go list 现算）
- **禁止依赖**：不要在根包写业务逻辑；不要绕过 `App` 直接 new 组件（会脱离生命周期与关闭顺序）；不要新增对 `pay` / 各通道包的直接资金调用。
- **依赖本目录的模块**：`example/gatewayserver`、`example/loginserver`

## 5. 禁止跨模块直连约束

- `go build ./...` 会连带编译 example/ 的 3 个包（同属主模块），那里的编译错误是真错误。
- 全仓检索排除工具存档目录：`grep -rn --include='*.go' --exclude-dir=.workbuddy-ai 模式 .`；`.` 前缀目录被 Go 工具链整体跳过但 grep 不跳。
- docs/ 与文件名带 p0_~p3_ 评审前缀的测试是开发过程资产，不进版本库。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「快速开始」「配置要点」「开发」
- `docs/repo-layout.md` L0-L8 依赖分层与包清单
- `docs/dead-exports.md` 导出 API 能否删
- `docs/testing-conventions.md`

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名顶层目录或组件包？→ 必裁，并同步本文与 `docs/repo-layout.md`
- [ ] 是否改动 `services.go` 装配顺序、`App` 字段或 `Run` 关闭流程？→ 必裁
- [ ] 是否在根包新增了业务逻辑（而非装配）？→ 禁止，改落到组件包
- [ ] 是否新增了对 `pay` / 通道包的直接调用？→ 必裁（资金链路必须走 pay 契约或各包门面）
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

| 子目录 | agents.md | 职责 |
|---|---|---|
| `ai/` | `ai/agents.md` | 对上游提供统一的大模型 Chat 门面：多提供方客户端注册、模型选择策略、价格查询与成本估算、并发/RP… |
| `bsc/` | `bsc/agents.md` | BSC 链上资金实现 |
| `chainroute/` | `chainroute/agents.md` | tron / ton / bsc / sol 四条链上资金通道共用「快照客户端 → 调通道 → 广播回执… |
| `config/` | `config/agents.md` | 配置加载、结构与校验 |
| `corectx/` | `corectx/agents.md` | 把 `*config.Cfg` 与 `AppView` 挂到 context 上向下传递，替代全局变量 |
| `db/` | `db/agents.md` | 对上封装 Redis 缓存 + MySQL/Postgres/Mongo 的读写口径，并对资金/账本类列… |
| `gin/` | `gin/agents.md` | 封装 gin 的路由注册与启停，并提供「HTTP 请求 ↔ 内部 gRPC 服务」的双向代理：把 HTT… |
| `golua/` | `golua/agents.md` | 内嵌 Lua VM 与 Go 互操作层：脚本加载/热更、Go 函数注册、Go↔Lua 值转换、Lua 类… |
| `ini/` | `ini/agents.md` | 零依赖的 INI 文件解析器，配置加载链的最底层 |
| `internal/httpx/` | `internal/httpx/agents.md` | 统一的 HTTP 客户端：超时、指数退避、Retry-After、响应体上限都在这里，错误分类复用 `*… |
| `ledger/` | `ledger/agents.md` | 保证「同一单号不签第二笔」的链维度出款台账：先记账再动钱，tron/ton/bsc/sol 四条链各持一… |
| `list/` | `list/agents.md` | 通用双向链表 List（可选索引）+ 节点生命周期 + 对象池 |
| `localtimer/` | `localtimer/agents.md` | 自带状态表的分层时间轮：注册/暂停/恢复/移除定时器，支持单次与循环、异步 Tick、调度分片（多通道）… |
| `mapmanager/` | `mapmanager/agents.md` | 按地图加载场景、NPC 与巡逻路径，NPC 的对话/商店文本可用 Lua 脚本配置 |
| `metrics/` | `metrics/agents.md` | Prometheus 指标与注册表单例 |
| `network/message/` | `network/message/agents.md` | 框架内所有 protobuf 消息与 gRPC 服务的生成代码：Head/FSMessage 通用消息壳… |
| `nft/` | `nft/agents.md` | NFT 持有/详情/铸造/转账/查单的供应商适配层，与数字货币资金链路刻意不耦合：凭证登记在独立的顶层 … |
| `pay/` | `pay/agents.md` | 资金通道纯契约层：定义订单/回执形状、收款方与账户校验口径，以及「哪条通道已经起来了」的分派表 |
| `random/` | `random/agents.md` | 两套随机实现：`MersenneTwister`（可播种、可复现）与 `MonteCarlo`；对外接口… |
| `ranking/` | `ranking/agents.md` | 两种内存排行结构（SkipList 与 RankTree）加一套落库/读档封装 |
| `rpc/` | `rpc/agents.md` | 进程间通信层：服务端/客户端、服务发现（静态/DNS）、鉴权、事件协议注册与回调，外加 HTTP↔gRP… |
| `sol/` | `sol/agents.md` | Solana 链上资金实现 |
| `swd/` | `swd/agents.md` | 中文敏感词过滤框架（曾叫 go-swd）：AC 自动机/Trie 双算法、拼音/谐音/形近字变体识别、词… |
| `syncmap/` | `syncmap/agents.md` | 两种并发 map：全局单锁 `Map`/`MapAny` 与按 key 分片的 `ShardedMap`… |
| `task_engine/` | `task_engine/agents.md` | 与业务无关的任务/进度引擎：一个 Engine 绑定一个玩家，按触发事件推进任务，达标 AUTO 发奖（… |
| `telegram/` | `telegram/agents.md` | 只剩 Bot 长轮询的消息通道：启动/停止、收发文本与图片、可用性自检 |
| `ton/` | `ton/agents.md` | TON 链上资金实现（配置段 ton.native）：用 tongo 组外部消息、本地热钱包签名，经 c… |
| `tron/` | `tron/agents.md` | TRON 链上资金实现 |
| `usdt/` | `usdt/agents.md` | 只是 pay + tron 的转发壳，让外部用 `usdt.` 前缀调用 USDT-TRC20 |
| `util/` | `util/agents.md` | 高频共用工具：回调注册表（含各通道密钥注入钩子）、协议类型注册与打包、字符串/哈希/HTTP/网络/时间… |
| `vars/` | `vars/agents.md` | 全仓最底层的共用件：日志器、异步日志通道、按 channel 分文件的日志管理、批量与滚动写入、slog… |
| `websocket/` | `websocket/agents.md` | WS 升级、鉴权、按类名从对象池取回调实例派发消息 |
| `whatsapp/` | `whatsapp/agents.md` | 走 go.mau.fi/whatsmeow 多设备协议（非 HTTP 供应商） |
| `example/` | `example/agents.md` | 三个可编译运行的演示进程，是框架装配的参考实现：gatewayserver（WebSocket 网关，含… |
| `bench/` | `bench/agents.md` | 两个 PowerShell 脚本：全量基准（带 -Short 快跑门控）与带 -race 的全量回归 |
| `docker-osx/` | `docker-osx/agents.md` | 用 Docker Compose 把服务跑在 macOS 的 Docker Desktop 上（含 WS… |
| `docs/` | `docs/agents.md` | 重构与评审的工作记录：依赖分层与包清单、死导出登记、资金缺陷评审、MySQL 重写、性能基线、测试约定等 |
| `deliverables/` | `deliverables/agents.md` | 历次评审/整改/集成的交付文档汇总目录，按项目分子目录存放 |
| `skills/` | `skills/agents.md` | 项目级 Agent Skill，跟随仓库 |
| `tasks/` | `tasks/agents.md` | 按团队/专题存放的任务单与交接记录，供后续接手者提供上下文 |
| `testdata/` | `testdata/agents.md` | 只给测试用的数据与夹具目录，不参与生产构建 |
| `outputs/` | `outputs/agents.md` | 一次性问答/压测输出的落盘目录，不参与构建 |

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
