# agents.md — swd/detector/preprocessor

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L3
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

检测前的文本归一化：全半角、数字风格、去除干扰字符等，配合拼音/谐音/形近字变体。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `preprocessor.go` | 预处理入口 | 中 |
| `NormalizeHomophone / DetectAndReplacePinyin / ProcessWithPinyin` | 变体处理 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `preprocessor.go` | 类型 Preprocessor/ProcessResult；函数 NewPreprocessor/NewPreprocessorWithConfig/SetConfig/Process/ProcessWithPositionMap/NormalizeHomophone/DetectAndReplacePinyin/ProcessWithPinyin | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Preprocessor`、`ProcessResult`、`NewPreprocessor`、`NewPreprocessorWithConfig`、`SetConfig`、`Process`、`ProcessWithPositionMap`、`NormalizeHomophone`、`DetectAndReplacePinyin`、`ProcessWithPinyin` 等
- **允许依赖**：`swd/common`、`swd/config`、`swd/core`、`swd/types/homophone`、`swd/types/pinyin`、`swd/types/similar`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`swd/common`、`swd/config`、`swd/core`、`swd/types/homophone`、`swd/types/pinyin`、`swd/types/similar`）；禁止反向依赖上层。
- **依赖本目录的模块**：`swd/detector`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `swd/common`、`swd/config`、`swd/core`、`swd/types/homophone`、`swd/types/pinyin`、`swd/types/similar` 方向；禁止反向引用上层包。
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
