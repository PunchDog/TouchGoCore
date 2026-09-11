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
| random | 5 | MersenneTwister 各方法、NextInt64、isPrime |
| ranking | 3 | SkipList 插入/查询、RankTree |
| list | 12 | Add/Range/Clear/InsertAfter/Range/Get/Remove/并发/池 |
| corectx | 2 | CfgFrom、AppViewFrom |
| metrics | 4 | 4 类指标 Inc 性能 |

## 关键观察

1. **safeSQLColumn** 当前每调用 78ns（无分配），新 Repository 的反射白名单需关注是否引入额外开销。
2. **list 池操作 PoolStress** 单线程 746μs / 1278 allocs — 是潜在热点，Repository 实现可考虑复用对象池。
3. **Mersenne Twister** 极快（< 50ns），无明显瓶颈。
4. **vars AssembleLine** 平均 < 200ns 范围，字符串拼接效率良好。
5. **metrics Inc 系列** 接近 0ns（prometheus client 内部批处理），无明显问题。

## 详细数据

见同目录 `bench-baseline-*.txt` 文本。

## 待办

- 后续：MySQL 重写后再次跑 benchmark，对比 Repository vs 旧 CRUD 的吞吐差异
- 端到端 RPC/WS benchmark 需要真实服务，CI 中跑（不在本机基线范围）
