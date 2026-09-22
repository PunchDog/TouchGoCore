# 测试约定

本文写三件事：测试文件名的前缀是什么意思、什么算「跑过」、以及这台机器上跑不了什么。

## 文件命名前缀

118 个测试文件里有 44 个带 `p0_` / `p1_` / `p2_` / `p3_` 前缀：

| 前缀 | 数量 | 含义 |
|---|---:|---|
| `p0_` | 5 | 第 0 轮评审发现的问题对应回归 |
| `p1_` | 15 | 第 1 轮 |
| `p2_` | 3 | 第 2 轮 |
| `p3_` | 21 | 第 3 轮 |
| 无前缀 | 74 | 常规用例 |

前缀是**评审轮次标记，不是可删标记**。带 `p3_` 不代表「临时验证、跑完就能扔」，
它们和别的测试一样是长期回归资产。反例即 `localtimer/p3_pool_key_test.go`：
它引用 `localtimer/internal/dupname` 这个只被它一处使用的夹具包，
按「只有测试在用」清理会直接失效该回归。

## 快跑门控 `-short`

需要真实时钟、耗时长、且在 CI 里价值不高的用例，一律挂 `testing.Short()` 门控：

| 用例 | 位置 | 代价 |
|---|---|---|
| 秒档精度 | `localtimer/p3_tier_precision_test.go:45` | 8 秒观察窗累计 5 轮续期样本 |
| 计数回归压测 | `localtimer/timer_count_regression_test.go:188` | 200 轮投递 |
| 子进程宕机回归 | `localtimer/timer_crash_test.go:49` | 起真子进程 |
| 真实端往返 | `rpc/integration_test.go:20` | 约 10 秒真实 HTTP/gRPC 往返 |

效果：`-short` 下 `localtimer` 43.7s → 15.0s，`rpc` 21.6s → 4.6s。
`bench/run.ps1 -Short` 走的即这条门控。
**门控是快跑开关，不是豁免**：合并前的完整验证必须有一次不带 `-short` 的全量。

## 「跑过」的判据

分层，从便宜到贵：

```bash
go build ./... && go vet ./...          # 1. 编译与静态检查（含 example/ 的 3 个包）
go test -count=1 ./<改动包>/             # 2. 单包，改动包必须连跑 3 次
go test -count=1 ./...                   # 3. 全量：35 个有测试的包
go test -timeout 600s -count=1 ./...     # 4. 交付前一次完整全量
```

- `-count=1` 是必需的：不加时同包重复运行会命中测试缓存，「绿」可能只是上一次的结果。
- 时序敏感包（`localtimer`）**必须连跑 3 次**，单次绿不算数。
- 全量结果与下面的基线名单比对，判据是「不比基线更差」，不是「必须绿」。

### 全量基线（commit `c84053b`，2026-09-22 实测）

在同一台机器上连续 3 轮 `go test -timeout 600s -count=1 ./...`：**3/3 全绿**，
35 个有测试的包零 FAIL。最慢的五个（取三轮里的最差值）：

| 包 | 耗时 |
|---|---:|
| `touchgocore/localtimer` | 45.6s |
| `touchgocore/rpc` | 18.0s |
| `touchgocore/swd/swd` | 15.3s |
| `touchgocore/gin` | 12.9s |
| 根包 `touchgocore` | 12.4s |

对照状态用 `git worktree add` 在目标 commit 建独立工作树跑，**不要**在主树里边跑边改：
早前一轮基线采集中，有一次「FAIL」实际是我在测试飞行途中拆 `util/net.go` 造成的
`IPInfo redeclared in this block`，与被测代码无关。

### 已知抖动面（尚未修，属测试质量问题）

以下用例断言真实时钟或机器核数，是未来出现偶发失败时的第一批嫌疑对象：

- `localtimer` 的测试里有 38 处 `time.Sleep`；
- `app_test.go:240-243` 按 `runtime.NumCPU()` 分支取目标值；
- 上表四个重型用例的时间窗在负载高时会被压缩。

真修法是把这些断言改成注入式假时钟，短期靠 `-short` 与「连跑 3 次」兜。

## 导出 API 快照门限

整理类改动（拆文件、改注释）必须证明导出面未变。工具是一份脚本：
`go doc -all` 逐包输出，取声明与成员行。

```bash
# 对每个包取「签名 + 结构体字段 + var/const 块成员」，滤掉 doc 正文
for p in $(go list ./...); do
  echo "### $p"
  go doc -all "$p" 2>/dev/null | grep -P '^(func |type |var |const |\(|\t)' | grep -vP '^\t//'
done
```

当前规模：1862 行，其中制表符缩进的成员行 934 行。判据分档：

- **纯移动**（拆文件）：快照 diff 为空，且全量 `go doc -all` diff 为空。
- **只加标注**：快照 diff 为空，全量 diff 只允许新增 `Deprecated:` 一类注释行。
- **只改注释**：快照 diff 为空，全量 diff 只允许注释文本变化。

两条踩过的坑：

1. **`grep -E` 不把 `\t` 当制表符**（GNU grep 把它当字母 `t`）。早期版本用
   `grep -E '^(func |type |\t)'`，实测制表符行命中 0 条——结构体字段和 `var`/`const`
   块成员**从来没进过快照**，即块内改名列在 `metrics.go:38-52` 那七个别名上都不报错。
   必须用 `grep -P`。这也是为什么必须用 `go doc -all` 而不是源码 `^func|^type` 正则：
   块内声明在源码里行首是缩进。
2. **`Deprecated:` 必须独占一段**。go/doc 只认段落开头的 `Deprecated:`；把它续写在
   既有一行注释之下，`go doc` 渲染时两段并成一段，下游 staticcheck 根本不报。
   用空注释行 `//` 分段，改完用 `go doc <pkg>.<Sym>` 肉眼确认渲染成独立段。

## 本机跑不了的东西

- **`go test -race` 跑不了**：需要 cgo，本机无 gcc。
  因此凡涉及并发容器与异步日志管道的改动（`syncmap`、`localtimer` 的通道/注册表、
  `vars` 的异步队列），只有非 race 覆盖。交付时必须明写这一条，不得称「已验证并发安全」。
  `bench/race.ps1` 因此在本机不可用，它写入 `docs/race-result.txt`。
- `gofmt -l` 在本仓有既有 CRLF 误报（例如 `util/callback_test.go`），
  判断格式是否真的有问题要看 `gofmt -d`，不要只看 `gofmt -l` 的文件名列表。
