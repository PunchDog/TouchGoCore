# TouchGoCore 性能基线报告（重构前）

> 采集时间：2026-09-11
> 环境：Windows 11 / Go 1.25.8 / 12th Gen Intel(R) Core(TM) i9-12900H (20 核)
> Benchtime: 200ms / -benchmem

## 概览

| 包 | benchmark 数 | 备注 |
|---|---|---|
| db | 2 | safeSQLColumn、buildDSN |
| localtimer | 3 | TimerType.String、calculateType、Timer.HasNext |
| vars | 4 | hasFormatVerbs、assembleLine(两种)、consoleLevelPrefix |
| syncmap | 4 | Store/Load/Range/MapAny |
| random | 5（2026-09-22 起 8） | MersenneTwister 各方法、NextInt64、isPrime；后加三个归因基准（锁地板、两路纯算术取值） |
| ranking | 3 | SkipList 插入/查询、RankTree |
| list | 12 | Add/Range/Clear/InsertAfter/Range/Get/Remove/并发/池 |
| corectx | 2 | CfgFrom、AppViewFrom |
| metrics | 4 | 4 类指标 Inc 性能 |

## 关键观察

1. **safeSQLColumn** 当前每调用 78ns（无分配），新 Repository 的反射白名单需关注是否引入额外开销。
2. **list 池操作 PoolStress** 单线程 746μs / 1278 allocs — 是潜在热点，Repository 实现可考虑复用对象池。
3. **Mersenne Twister** 极快（< 50ns），无明显瓶颈。（已过期，见文末 2026-09-22 补测：那 43ns 里约 26ns 是一次无争用加解锁，算术只占 10ns。）
4. **vars AssembleLine** 平均 < 200ns 范围，字符串拼接效率良好。
5. **metrics Inc 系列** 接近 0ns（prometheus client 内部批处理），无明显问题。

## 详细数据

见同目录 `bench-baseline-*.txt` 文本。

## 2026-09-22 补测：本报告的绝对值不可跨时段引用，且互斥锁是 `random` 的地板

两件事只有把改动前后的代码放进同一次测量窗口才看得出来，故记在这里。

**一、同一份 HEAD 代码在本机从 29.3 ns/op 测到 44.5 ns/op。**
上表的 43.69（Uint64）/ 58.79（NextInt64）与今天的实测差了 1.3~1.5 倍，而代码一字未动；
`isPrime`（本轮完全没碰的函数）在同一台机器上测得 2219 / 3158 / 3702 ns/op。
判据是机器状态而非代码状态，所以**对比必须交错进行**：`git worktree` 拉出参照树后，
按 before→after→before→after 的顺序成对跑，同轮内取中位数比较；跨轮次、跨时段比绝对值会得出错误结论
（本轮就先用「43.69 → 41.49」误判成只提速 5%，交错重测后才知道真实差值是 44.5 → 42.0）。

**二、一次无争用 `sync.Mutex` 的 Lock+Unlock 在本机要 26~41 ns，就是 `random` 每个导出方法的成本地板。**
新增的三个归因基准（`random/random_perf_test.go`）把一次取值拆开测：

| 归因项 | 改前 | 改后 |
|---|---:|---:|
| `BenchmarkLockFloor`（一次加解锁，空循环体） | 38.9 | 39.9（同轮） |
| `BenchmarkMersenneTwister_DrawNoLock`（计时区外先持锁，纯算术：twist 摊销 + 升温） | 10.2 | 8.4 |
| `BenchmarkMonteCarlo_DrawNoLock`（纯算术，xoshiro256\*\*） | — | 4.6 |
| `BenchmarkMersenneTwister_Uint64`（含锁的对外方法） | 44.5 | 42.0 |
| `BenchmarkNextInt64`（包级全局，含锁 + 两路平均） | 59.4 | 52.9 |

拟合得上的解释是 `Uint64 ≈ 锁 + 算术`、`NextInt64 ≈ 一把锁 + MT 算术 + MC 算术`（39+8.4+4.6=52 ≈ 52.9）。
所以改后的数字已经贴地板：改前那 59.4 里除锁之外还有 15ns 花在 MonteCarlo 的 `defer`+`recover`、
对 `len(r.r)` 的运行时 64 位除法与接口分发上（`BenchmarkNextInt64 − BenchmarkMersenneTwister_Int63` = 15.7 ns），
这些现已全部消掉，剩下的只有锁。**再要快只能减少每秒锁次数**——而 `Uint64()` 每次调用只出一个值，
在导出面冻结（见 `testing-conventions.md` 的 `go doc -all` 快照门）之下做不到，
除非把包级 `NextInt64()` 改成无锁批量取号（atomic 游标 + 预生成批次，估计可到 ~17 ns）。
本轮没有做：它在 `-race` 跑不了的本机上是新的高风险面，而 `random` 的调用方只有
`ranking.randomLevel()`（整包零 importer）与已标废弃的 `util.RandomStr`。

### 同日追加：分布校验，以及 `util.RandInt` 换源的开销

**分布：两路平均之后仍然均匀的是「残余」，不均匀的是「幅度」。**
新增 `random/random_dist_test.go` 与 `util/random_dist_test.go`，用卡方拟合优度检验
（α=1e-4，Wilson–Hilferty 临界值）逐路实测：

| 检验对象 | 10 万次取值 | 结果 |
|---|---|---|
| MT19937-64 幅度 64 等宽桶 | 卡方 77.2 ≤ 103.5（df=63） | 均匀 |
| xoshiro256\*\* 幅度 64 等宽桶 | 卡方 66.8 ≤ 103.5 | 均匀 |
| 两路 `%2/%3/%8/%62/%1000/%10000` | 全部通过（如 %62：61.9 ≤ 101.0，df=61） | 均匀 |
| `NextInt64()` 幅度 64 等宽桶 | 中间桶 / 边缘桶 ≈ 57~97（理论 63；均匀则 ≈1） | **三角分布，非均匀** |
| MT / MC / 平均后序列的 lag-1 自相关 | \|r\| < 0.02 | 无跑道效应 |

即：`Random.NextInt64()` 把两路求平均，**换来的低位干净是以牺牲幅度均匀性为代价的**——
它的值集中在 2^62 附近。按低位取模的用法（`%62`、`%10000`、`%切片长度`）不受影响，
因为 `floor((a+b)/2) mod n` 只由 `(a+b) mod 2n` 决定，而 `a` 一路在 mod 2n 上已近均匀。
这一点现已写进 `Random.NextInt64` 的注释，并由 `TestNextInt64_MagnitudeIsTriangular` 钉住；
需要 `[0,2^63)` 上真正均匀的幅度请用单路的 `MersenneTwister.Int63`。

**换源代价：**`util.RandInt`/`RandRange` 的随机源改为 `random` 包后，同窗交错实测（`-count=5`）

| 基准 | ns/op（5 轮区间） | 说明 |
|---|---|---|
| `BenchmarkRandInt` | 53.7 ~ 61.9 | 新路径：全局锁 + 两路 + 取模 |
| `BenchmarkUnderlyingNextInt64` | 53.4 ~ 57.2 | 底下那次取值，说明取模与边界判断 ≈0 |
| `BenchmarkRandIntStdlibRef` | 17.5 ~ 23.2 | 被换掉的 `randv2.Int64N`，per-P 无锁缓存 |

即每次取值 +33~38 ns（约 2.9 倍），两者都是 0 allocs。绝对量级上无所谓
（唯一生产调用方是 `mapmanager/npc.go:386` 选一条对话文本），但换源的真正收益——全仓一条
发生器链路、兜底与并发保护集中在 `random`——要连着这个代价一起记。复现命令：
`go test -run '^$' -bench 'RandInt|Underlying' -benchmem -benchtime=200ms -count=5 ./util`；
分布结论复现：`go test -run 'Uniform|Triangular' -v ./random ./util`（`accept` 用 `t.Logf` 打印统计量）。

**更正上节末段**：那段写「`random` 的调用方只有 `ranking.randomLevel()`（整包零 importer）
与已标废弃的 `util.RandomStr`」，自本轮起不成立——`util.RandInt` 也走这条链路，
而且它是其中唯一有生产调用方的。因此全局默认实例那把锁现在在真实业务路径上，
若日后成为热点，优先选项是让调用方自持 `random.New(seed)`，而不是给包级实例加无锁批量取号。

### 同日再追加：三角分布已修——两路组合由求平均改为异或

上表最后那条「`NextInt64()` 幅度是三角分布」已作为缺陷处理，不再当作设计代价保留。
`random/interface.go` 的组合式从 `int64((a+b)>>1)` 换成 `r.mt.int63Locked() ^ r.mc.int63Locked()`：

- **均匀性**：均匀量和任意独立量异或，结果的边际分布跟随那个均匀量；MT 这一路已实测 64 桶均匀，
  所以组合层不再需要「调用方只取低位」这一前提。实测 `NextInt64()` 幅度 64 桶卡方 67.8 ≤ 116.5（df=63），
  中间桶/边缘桶从 57~97 变成 **0.98**（均匀应为 1），`%2/%3/%8/%62/%1000/%10000` 残余检验继续全过。
- **值域**：两路都是 `[0,2^63)`，异或仍 `<2^63` 且天然非负，不再需要右移——平均版白丢的那一位回来了，
  值域上限重新被真实取到（新增断言「最大观测值 > 0.99×2^63」，三角分布下 10 万次取值的最大值只到约 0.75×2^63）。
- **开销不变**（异或 vs 加+移，同一时间窗）：`BenchmarkNextInt64` 中位 53.4（换前 52.9~53.9）、
  `BenchmarkRandInt` 52.4（换前 53.7~61.9）、`BenchmarkMersenneTwister_Uint64` 40.9、
  `BenchmarkLockFloor` 34.6、`BenchmarkRandIntStdlibRef` 18.7，全部 0 allocs。
  成本地板仍是那把锁，与上一节结论一致。
- **测试口径**：`TestNextInt64_MagnitudeIsTriangular` 改名 `TestNextInt64_MagnitudeUniform` 并反向断言
  （比值门限从 20~200 改成 0.4~2.5），统计门连跑 10 轮全绿。上节那张表保留作为发现记录，不改写。
- **第二次变更输出流**：`NextInt64()` 的序列又换了一次（两路引擎各自逐位未动，
  `TestMersenneTwister_StreamPinned` 与 `TestMonteCarlo_Stream` 是这条保证的证据）。
  仓内消费点都不依赖可复现流，但本 module 被外部工程直接 import，若有外部「同种子复现」用法需据此重算。

原始基准输出 `docs/bench-after-random.txt`（UTF-16LE + CRLF，读前先 `iconv -f UTF-16LE -t UTF-8`）
停在**求平均阶段**，不含本节的异或后数字；那一阶段的上表数值用本节末尾的复现命令重跑即可，
参照列（`git worktree` 拉改动前的提交）按第一节的交错规则重建，不做跨时段拼接。

## 待办

- 后续：MySQL 重写后再次跑 benchmark，对比 Repository vs 旧 CRUD 的吞吐差异
- 端到端 RPC/WS benchmark 需要真实服务，CI 中跑（不在本机基线范围）
