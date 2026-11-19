# agents.md — swd

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L7
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

中文敏感词过滤框架（曾叫 go-swd）：AC 自动机/Trie 双算法、拼音/谐音/形近字变体识别、词典加载与持久化、替换策略、检测/过滤分离的组件化装配。主模块内零 importer，作为对外能力包保留。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `swd.go` | 一句话建好默认实例 | 中 |
| `detector/detector.go` | 检测 | 中 |
| `filter/filter.go` | 替换 | 中 |
| `dictionary/loader.go` | 加词与词典来源 | 中 |
| `algorithm/ac.go` | 选算法与变体能力 | 中 |
| `algorithm/trie.go` | 选算法与变体能力 | 中 |
| `swd/options.go` | 调选项 | 中 |
| `*.txt（见 dictionary/、config/mappingdata/）` | 词典数据文件（go:embed） | 高 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `swd.go` | 函数 New/NewWithFactory/NewWithMappingDir/NewDefaultFactory/NewDefaultFactoryWithMappingDir/GetGlobalMapping/SetGlobalMapping | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`New`、`NewWithFactory`、`NewWithMappingDir`、`NewDefaultFactory`、`NewDefaultFactoryWithMappingDir`、`GetGlobalMapping`、`SetGlobalMapping`、`All` 等
- **允许依赖**：`swd/config`、`swd/core`、`swd/swd`、`swd/types/category`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`swd/config`、`swd/core`、`swd/swd`、`swd/types/category`）；禁止反向依赖上层。
- **依赖本目录的模块**：—

## 5. 禁止跨模块直连约束

- swd/**/*.txt 是要进版本库的数据，.gitignore 的一次性输出规则限定在仓库根（/*.txt），不要改回裸 *.txt。
- 目录名与包名已对齐（原 go-swd 于 2026-09-11 用 git mv 改名）。
- 跨层依赖只向 `swd/config`、`swd/core`、`swd/swd`、`swd/types/category` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `swd/dictionary/*.txt` 经 go:embed 打进二进制
- `.gitignore` 根级 `/*.txt` 规则

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

| 子目录 | agents.md | 职责 |
|---|---|---|
| `swd/` | `swd/agents.md` | 中文敏感词过滤框架（曾叫 go-swd）：AC 自动机/Trie 双算法、拼音/谐音/形近字变体识别、词… |

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
