# 仓内零引用导出登记（不删除清单）

本框架以 module `touchgocore` 被外部业务工程 import，因此**「仓内零引用」不构成删除依据**。
本文登记所有已核实的零引用导出与零消费包，注明处置方式，避免每轮整理都重新勘探一遍、
也避免下一轮把「登记」误读成「待删」。

判据统一为：在**本仓库**（含 `example/` 与全部测试文件）内，除自身声明与本文档之外，
`grep -rn --include='*.go' --exclude-dir=.workbuddy-ai` 命中数为 0。

- 已标 `// Deprecated:` 的条目保留至少一个大版本，且要求下游侧确认无引用才可移除。
- 未标注的条目是刻意保留的公开能力，理由逐条写在下面。

## 1. 已标 Deprecated 的导出

### 根包 `metrics.go` 的七个单例别名

`WSMetrics / RPCMetrics / HTTPMetrics / TimerMetrics / LuaMetrics / DBMetrics / LogMetrics`
（`metrics.go:38-52`）都是 `touchgocore/metrics` 包内同名单例的别名。

同时说明一处长期误读：根包 `metrics.go` 与 `metrics/` 包**不是重复代码**，只是撞名——
根包那份负责「把 /metrics 端点服务起来」（HTTP 服务、pprof、token 鉴权、时间轮积压
拉取式采集器），`metrics/` 包负责「Prometheus 指标的定义与注册」。
所以正解是注释澄清，改名反而会破坏 `go doc` 的导航。

（`metrics/metrics_test.go:20` 起的 `TestWSMetrics_BasicOps` 等 7 个是**函数名巧合**：
它们操作的是 `metrics` 包内的同名单例 `WS.SetConnections(...)`，与根包别名无关。）

### `util/` 包内零调用导出

| 符号 | 位置 | 附带事实 |
|---|---|---|
| `FormatDuration` | `util/time.go` | 无 |
| `RandomStr` | `util/string.go` | 逐位取模选字符；残余均匀性已过卡方检验（偏斜量级 ≤2n/2^63），问题在按字节截断长度与非密码学安全 |
| `ParseDbData` | `util/string.go` | 转换异常被 recover 吞掉，不返回错误 |
| `HTTPGet` | `util/http.go` | 无超时、无 context |
| `PostFile` | `util/http.go` | 依赖已废弃的 `ioutil` 语义 |
| `NewEchoPacket` | `util/echoProtocol.go` | `EchoPacket` 手写帧头的老打包路径，仓内改走 protobuf |
| `MD5` | `util/hash.go` | 仅可作内容指纹，不适用于签名/口令 |
| `IPInfo` | `util/net.go` | 只是 IP 归属地结果的载体，仓内无函数产出或消费它 |
| `CheckPort` | `util/net.go` | 实现恒返回 nil，探测失败也不报错 |
| `FormatStruct` | `util/reflect_format.go` | 反射逐字段拼接 |
| `NumberSortLess` | `util/numbersort.go` | stdlib `sort.Slice` / `slices.SortFunc` 已覆盖 |
| `NumberSortDesc` | `util/numbersort.go` | 同上 |
| `String2NumberArray` | `util/numbersort.go` | 解析失败静默返回零值 |

同文件的私有辅助 `getNumber` / `formatStruct` / `formatMapKey` 随其公共入口保留。

### `config.GetDefaultFie`

`config/config.go` 的拼写错误版本（`Fie` 应为 `File`），零调用；正确版本
`GetDefaultFile` 已存在且被使用。仅因外部可能按错名调用而保留。

### 早在本轮之前就已标 Deprecated

`util.PasreFSMessage`（`util/echoProtocol.go`）—— 正确名 `ParseFSMessage` 只是一行转发。
本轮把 `rpc/client.go`、`rpc/server.go` 两处生产调用迁到正确名；
`example/gatewayserver/gatepb/gate_test.go` **故意**继续调用废弃名，当作兼容回归保留。

## 2. 核实后刻意「不标」的条目

标注 `Deprecated` 是有代价的：下游跑 staticcheck 时会真的报出告警。以下条目按事实排除：

- `util.RandInt`、`GetClassName`、`IsIntranetIP`、`GetPathFile`、`ConvertToKind`：
  仓内有生产调用方（分别见 `mapmanager/npc.go:386`、`gin/run.go:93`、
  `rpc/client.go:424`、`mapmanager/map.go:130`、`golua/convert.go:152`），
  本就不是死导出。`RandInt` 的随机源于 2026-09-22 起由 `math/rand/v2` 改为 `touchgocore/random`
  的包级默认实例（每次取值一把全局互斥锁，同窗实测中位数 19→53 ns/op，见 `perf-baseline.md`），
  取值语义 `[0,max)` 与 `max<=0` 返回 0 均未变。
- `util.RandRange`：零生产调用，但与 `RandInt` 是一对配套 API、语义清晰、
  被 `util/p0_regress_test.go` 钉住。标 Deprecated 等于要求自己的测试使用废弃符号。
  换源顺带收掉一个宕机面：区间宽度大到 int64 溢出时，旧实现把负数宽度交给
  `randv2.Int64N` 会 panic，现已按 `RandInt` 的约定退化成返回 `min`
  （`util/random_dist_test.go` 的 `TestRandRange_HugeWidth` 钉住）。
- `util.Numeric`：被上面两个 Sort 类型引用，是它们的类型约束。
- `util.IPData`：`IPInfo` 的字段类型。两个都标会让仓内出现「已废弃符号引用已废弃符号」，
  告警打在框架自身头上，故只在 `IPInfo` 上标。
- `config.GetDefaultFile` / `GetConfDir` 等：正确版本，非登记对象。

## 3. 整包零 importer

| 包 | 规模 | 状态 |
|---|---|---|
| `swd/` | 40 文件 / 8896 行 | 包外零 importer（`touchgocore/swd...` 的引用全部来自 swd 内部）。独立内容检测组件，外部业务可直接依赖，原地保留。 |
| `ranking/` | 5 文件 / 848 行 | 全仓零 importer，连自身测试也是同包测试。排行榜能力，原地保留。 |

删除两者的前置：全量下游 import 普查 + 弃用期。

## 4. `golua/` 目录名与包名不一致

目录是 `golua/`，包声明是 `package lua`（`golua/*.go` 全部如此），因此四个引用方
（`services.go:9`、`mapmanager/map.go:9`、`mapmanager/npc.go:7`、
`mapmanager/p1_npc_test.go:15`）都写成 `lua "touchgocore/golua"`。

两个修正方向都是破坏性的：改包名破坏 `lua.Run` 这类既有调用，改目录破坏 import 路径。
属 D 档，需发布窗口与版本策略先行。

## 5. `//go:build ignore` 的三个文件

不参与编译，删除的编译风险为零，但它们是文档性资产，保留并登记：

| 文件 | 行数 | 用途 | 同步责任 |
|---|---|---|---|
| `db/mysql/models_example.go` | 301 | 仓内唯一的 mysql 新 API 用法样例，被 `docs/mysql-new-api-design.md` 记为待议项 | 改 `db/mysql` 导出签名时同步该样例 |
| `vars/example_usage.go` | 299 | 日志初始化/等级/异步通道用法存档（其中纯英文注释 15 行） | 改 `vars` 入口名时同步 |
| `network/message/gen_desc.go` | 62 | 一次性生成 protobuf descriptor 的工具 main，产物已落到 `.pb.go` | 仅当协议描述需重生成时手工运行 |

## 6. 明确**不可删**的夹具

- `localtimer/internal/dupname/plaintimer.go`（14 行）：唯一引用方是
  `localtimer/p3_pool_key_test.go:7`。它存在的意义就是「与主包内某类型同名、但不同包」，
  用来复现对象池按类型短名分组的回归（S62）。按「只被测试引用」删除会直接失效该回归。
- `random.isPrime` / `gcd` / `areCoprime`：2026-09-22 起在包内零引用——`MonteCarlo` 换成
  xoshiro256\*\* 后不再需要现搜乘数（原 `findNextPrime` 已删，它对任何 2 的幂模数恒在第一次
  尝试命中，等于常量函数）。三者仍被 `random_test.go` 的 TestIsPrime/TestGcd 与
  `random_bench_test.go` 的 BenchmarkIsPrime 引用，且是非导出符号（`go doc` 快照看不见），
  删它们等于删两个测试与一行基线，故原地保留，函数注释已写明「现仅由本包测试引用」。
- `MonteCarlo.M`（导出字段，在快照成员行里）：原语义是同余模数 `1<<k`，换 64 位发生器后模数为
  2^64、无法用正的 int64 表示，故改记为输出值域上界 `math.MaxInt64`。字段名与类型必须留着，
  注释改动的写法也有讲究：只有**独占一行**的字段注释会被快照的 `grep -vP '^\t//'` 滤掉，
  写在行尾的 `M int64 // ...` 会改掉成员行本身，破逐字节门。
- `docs/bench-*.txt`（12 个，全部已跟踪）：`docs/perf-baseline.md` 引用的性能基线证据。
- `swd/**/*.txt`（15 个，全部已跟踪）：`go:embed` 的词典与映射数据。
  `go:embed` 不读 `.gitignore`，所以历史上裸 `*.txt` 规则的风险是前瞻性的——
  将来新增词典会被静默忽略，本地与 CI 全过而下游 `go:embed` 编译失败。
  现规则已收紧为根目录限定（`.gitignore:17`）。
- `bench/race.ps1`、`bench/run.ps1`：活的压测/回归入口（`run.ps1 -Short` 是快跑门控），
  各自的输出 `docs/race-result.txt`、`docs/bench-result-*.txt` 已在 `.gitignore` 登记。
