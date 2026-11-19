# agents.md — ledger

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

保证「同一单号不签第二笔」的链维度出款台账：先记账再动钱，tron/ton/bsc/sol 四条链各持一个实例、键带链维度，避免不同链的单号互相看见。纯契约 + 内存实现，零内部依赖。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `ledger.go` | 写入与读取台账 | 极高 |
| `State / Terminal / Expired / Entry` | 状态判定 | 中 |
| `WithLock / NowMS / Chain` | 并发与时间 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `ledger.go` | 类型 State/Entry/Journal/Ledger；函数 Terminal/Expired/Put/Get/New/Chain/SetJournal/WithLock | 极高 |

## 4. 对外接口 / 依赖

- **对外暴露**：`State`、`Entry`、`Journal`、`Ledger`、`Terminal`、`Expired`、`Put`、`Get`、`New`、`Chain`、`SetJournal`、`WithLock`、`NowMS`、`ErrAlreadySettled`、`ErrJournalUnavailable` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止依赖 `pay` 或任何通道包；禁止网络/DB IO（进程内台账）。
- **依赖本目录的模块**：`bsc`、`sol`、`ton`、`tron`

## 5. 禁止跨模块直连约束

- 幂等键必须带链维度；状态迁移单向；进程内实现重启即清空，跨重启幂等由业务落库保证。
- 保持叶子层（零/极少内部依赖）：新增任何内部依赖都会把本包的约束传染给上层。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「配置要点」资金通道段（四条链事实内联）
- `docs/repo-layout.md`「会动钱的出款有四条」
- `docs/fund-defect-review.md` 资金链路历史缺陷

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否改动状态机、过期口径或 `WithLock` 锁边界？→ 必裁（幂等语义）
- [ ] 是否让幂等键失去链维度（`Chain`）？→ 禁止
- [ ] 是否把「先记账再动钱」的顺序颠倒？→ 禁止
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
