# 仓库布局与包依赖

本文回答两个问题：某个包该不该被另一个包引用；新增代码该落在哪个包。
数据由 `go list -json ./...` 现算，非手工整理，改包结构后请重跑生成命令（见文末）。

## 规模

- 主模块 39 个包 / 133 个非测试 .go 文件 / 28998 行；另有 3 个 `example/` 演示包。
- 全仓 259 个 .go 文件，其中测试 118 个。
- module 名 `touchgocore`，被外部业务工程直接 import，因此导出面即 API：
  见 [dead-exports.md](dead-exports.md) 的「零引用不等于可删」。

## 依赖分层

同层之间无依赖，只允许下层被上层引用。括号内是包数。

```
L0  无内部依赖      vars · syncmap · ini · network/message · swd/common
                    swd/types/category · swd/config/mappingdata
L1  配置与指标      config · metrics · random · db/dbmap · swd/config · swd/core
L2                  corectx · util · db/mysql · ranking · swd/algorithm
                    swd/types/{homophone,pinyin,similar}
L3  业务组件        ai · db · gin · list · websocket
                    swd/detector/preprocessor · swd/dictionary
L4                  localtimer · swd/detector
L5  长连接与脚本    golua · rpc · telegram · swd/filter · localtimer/internal/dupname
L6                  mapmanager · swd/swd
L7  装配层          根包 touchgocore · swd
L8  演示           example/gatewayserver · example/loginserver
```

关键点：

- `vars` 在 L0，日志器不依赖任何业务包；它同时被 21 个包引用，是全仓最底层的共用件。
  给 `vars` 加依赖等于给全仓加依赖，基本不可接受。
- `util` 在 L2，依赖 `network/message`、`random`、`syncmap`、`vars`。它不是叶子层，
  L0/L1 的包不能反向引用它。
- 根包只做装配：`app.go` / `services.go` 把 17 个内部包接进一个生命周期容器，
  自身几乎没有可复用能力。新能力应落到对应组件包，不要落到根包。
- `ai/` 是 `refactor-plan.md`（2026-09-11）依赖图里缺失的一层：它位于 L3，
  依赖 `config`、`corectx`、`vars`，被根包 `services.go` 与 `mapmanager` 引用。

## 包清单

「直接内部依赖」只列 `touchgocore/...`，不含标准库与三方库。
按非测试行数降序。

| 包 | 非测试文件 | 非测试行 | 直接内部依赖 |
|---|---:|---:|---|
| `vars` | 11 | 3121 | — |
| `localtimer` | 11 | 2803 | list, syncmap, util, vars |
| `golua` | 11 | 2619 | config, corectx, localtimer, metrics, syncmap, util, vars |
| `rpc` | 6 | 2242 | config, corectx, localtimer, metrics, network/message, syncmap, util, vars |
| `util` | 13 | 1883 | network/message, random, syncmap, vars |
| `websocket` | 4 | 1768 | config, corectx, metrics, syncmap, util, vars |
| `db/mysql` | 10 | 1511 | config, db/dbmap, vars |
| `ai` | 6 | 1262 | config, corectx, vars |
| 根包 | 5 | 999 | ai, config, corectx, db, db/dbmap, gin, golua, ini, localtimer, mapmanager, metrics, rpc, syncmap, telegram, util, vars, websocket |
| `db` | 5 | 859 | config, db/dbmap, db/mysql, metrics, util, vars |
| `config` | 3 | 731 | ini |
| `ranking` | 3 | 704 | random, syncmap |
| `swd/detector` | 2 | 695 | swd/algorithm, swd/common, swd/config, swd/core, swd/detector/preprocessor, swd/dictionary, swd/types/category |
| `mapmanager` | 2 | 688 | corectx, golua, syncmap, util, vars |
| `swd/algorithm` | 2 | 620 | swd/common, swd/core, swd/types/category |
| `swd/dictionary` | 2 | 613 | swd/config, swd/config/mappingdata, swd/core, swd/types/category, util |
| `swd/swd` | 4 | 555 | swd/config, swd/core, swd/detector, swd/dictionary, swd/filter, swd/types/category |
| `list` | 3 | 552 | util, vars |
| `syncmap` | 2 | 516 | — |
| `telegram` | 1 | 486 | config, corectx, localtimer, util, vars |
| `swd/filter` | 2 | 458 | swd/core, swd/detector, swd/types/category, util |
| `network/message` | 3 | 434 | — |
| `gin` | 2 | 387 | corectx, util, vars |
| `random` | 3 | 377 | vars |
| `swd/detector/preprocessor` | 1 | 302 | swd/common, swd/config, swd/core, swd/types/homophone, swd/types/pinyin, swd/types/similar |
| `metrics` | 1 | 256 | vars |
| `swd/config/mappingdata` | 1 | 255 | — |
| `swd/common` | 2 | 243 | — |
| `swd/core` | 2 | 211 | swd/types/category |
| `swd/config` | 1 | 201 | swd/config/mappingdata |
| `ini` | 1 | 173 | — |
| `swd/types/category` | 1 | 136 | — |
| `corectx` | 1 | 81 | config |
| `swd` | 1 | 74 | swd/config, swd/core, swd/swd, swd/types/category |
| `db/dbmap` | 1 | 59 | syncmap |
| `swd/types/homophone` | 1 | 39 | swd/config |
| `swd/types/similar` | 1 | 39 | swd/config |
| `swd/types/pinyin` | 1 | 32 | swd/config |
| `localtimer/internal/dupname` | 1 | 14 | localtimer |

## 目录里容易被误判的几处

- `example/` 参与主模块编译：`go build ./...` 与 `go vet ./...` 会连带编译这 3 个包。
  它们不是独立 module，也不是可忽略的样例目录，改导出 API 时它们的编译错误是真错误。
- `swd/`（40 文件 / 8896 行）与 `ranking/`（5 文件 / 848 行）在主模块内零 importer，
  但两者都是对外能力包，原地保留。`swd` 曾叫 `go-swd`（目录名与包名不一致），
  2026-09-11 已用 `git mv` 对齐。
- `swd/**/*.txt`（15 个）与 `docs/bench-*.txt`（11 个）是**要进版本库的数据**：
  前者经 `go:embed` 打进二进制，后者是 `perf-baseline.md` 引用的基线证据。
  `.gitignore` 里的一次性输出规则因此限定在仓库根（`/*.txt`），不用裸 `*.txt`。
- `golua/` 目录声明 `package lua`，所有引用方写 `lua "touchgocore/golua"`。
  两个方向的对齐都是破坏性变更，见 [dead-exports.md](dead-exports.md) 第 4 节。
- `.workbuddy-ai/`、`.workbuddy/` 是工具存档目录。`.` 前缀目录被 Go 工具链整体跳过
  （`go list ./...` 命中数为 0），但 `grep -r` 不跳，全仓检索要加
  `--exclude-dir=.workbuddy-ai`，否则命中的是过期副本。
- `bench/run.ps1`（全量基准，`-Short` 为快跑门控）与 `bench/race.ps1`（带 `-race` 的
  全量回归）是活的入口。后者在本机跑不了——见 [testing-conventions.md](testing-conventions.md) 的环境限制一节。

## 重跑本文的数据

```bash
# 每个包的直接内部依赖，按非测试行数降序
go list -json ./... | jq -r '.ImportPath + "\t" + (.Imports | map(select(startswith("touchgocore"))) | join(","))'

# 依赖分层（L0 为无内部依赖的叶子）：对 .Imports 做最长路径拓扑排序
```
