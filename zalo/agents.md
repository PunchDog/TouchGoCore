# agents.md — zalo

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

走 zcago 协议直连（非官方客户端，有封号风险，通道默认不开，zalo.enable 显式为 on 才装配）。扫码配对在本仓，也可导入浏览器 cookie；账号凭证存 zalo.sessions.dsn 指向的 Postgres zalo_sessions 表（留空复用 whatsapp 的 DSN）。发送只支持文本并严格限频。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `za_facade.go` | 读写门面（16 个） | 中 |
| `ws_zalo_qr.go` | 扫码配对 WS 端点 | 中 |
| `za_pairing.go` | 扫码配对 WS 端点 | 中 |
| `za_cache.go` | 缓存与限频 | 中 |
| `store_postgres.go` | 存储与凭证 | 中 |
| `za_credentials.go` | 存储与凭证 | 中 |
| `za_sdk.go` | SDK 与监听 | 中 |
| `za_listener.go` | SDK 与监听 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `za_manager.go` | 函数 Stop/ListAccounts/DeleteAccount/Logout/LoginWithCookie/StartClient | 中 |
| `za_pairing.go` | 类型 QRSession；函数 Subscribe/Close/ZaloStartPairing/LookupQRSession | 中 |
| `za_cache.go` | 函数 Now/Sleep/List/Refresh/Users/Groups/CachedGroup/DropGroups | 中 |
| `za_credentials.go` | 函数 Error/Unwrap | 中 |
| `za_facade.go` | 函数 ZaloLoginWithCookie/ZaloStartClient/ZaloDeleteAccount/ZaloListAccounts/ZaloSendMessage/ZaloSendGroupMessage/ZaloSendCard/ZaloSendGroupCard | 中 |
| `za_sdk.go` | 函数 Login/LoginQR/UID/SnapshotCredentials/SendText/SendCard/DeleteChat/GetAllFriends | 中 |
| `store_postgres.go` | 函数 Upsert/Get/List/Delete/Close | 中 |
| `za_listener.go` | 函数 StartListener/StopListener | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Stop`、`ListAccounts`、`DeleteAccount`、`Logout`、`LoginWithCookie`、`StartClient`、`QRSession`、`Subscribe`、`Close`、`ZaloStartPairing`、`LookupQRSession`、`Now`、`Sleep`、`List`、`Refresh`、`Users`、`Groups`、`CachedGroup` 等
- **允许依赖**：`config`、`corectx`、`gin`、`pay`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`gin`、`pay`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- 旧 zalo.login 配置与 ZaloSendCode/ZaloLogin/ZaloFriends 门面已删除（破坏性变更）。
- zalo_sessions.credentials 含 cookie/IMEI，部署侧应收紧库权限或启用落盘加密。
- 限频按 accountID 在进程内记账，多实例/重启会重置。
- 跨层依赖只向 `config`、`corectx`、`gin`、`pay`、`util`、`vars` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`（框架总说明）
- `docs/repo-layout.md`（依赖分层与包清单）
- 包内 `doc.go` 与文件头注释

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
