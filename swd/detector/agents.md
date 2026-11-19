# agents.md — swd/detector

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L4
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

组合词库、算法与预处理器做命中检测，支持主/辅算法切换与词库热更新。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `detector.go` | 建检测器 | 中 |
| `Detect / DetectIn / Match / MatchIn / MatchAll / MatchAllIn` | 检测与匹配 | 中 |
| `EnableSecondaryAlgorithm / DisableSecondaryAlgorithm / UpdateAlgoCache / OnWordsChanged` | 运行时调参 | 中 |
| `constants.go` | 常量 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `detector.go` | 函数 NewDetector/NewDetectorWithConfig/OnWordsChanged/EnableSecondaryAlgorithm/DisableSecondaryAlgorithm/UpdateAlgoCache/Detect/DetectIn | 中 |
| `constants.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`NewDetector`、`NewDetectorWithConfig`、`OnWordsChanged`、`EnableSecondaryAlgorithm`、`DisableSecondaryAlgorithm`、`UpdateAlgoCache`、`Detect`、`DetectIn`、`Match`、`MatchIn`、`MatchAll`、`MatchAllIn` 等
- **允许依赖**：`swd/algorithm`、`swd/common`、`swd/config`、`swd/core`、`swd/detector/preprocessor`、`swd/dictionary`、`swd/types/category`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`swd/algorithm`、`swd/common`、`swd/config`、`swd/core`、`swd/detector/preprocessor`、`swd/dictionary`、`swd/types/category`）；禁止反向依赖上层。
- **依赖本目录的模块**：`swd/swd`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `swd/algorithm`、`swd/common`、`swd/config`、`swd/core`、`swd/detector/preprocessor`、`swd/dictionary` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `swd/agents.md`（框架总索引）
- `docs/repo-layout.md` 依赖分层

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
