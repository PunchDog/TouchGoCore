# agents.md — whatsapp

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

走 go.mau.fi/whatsmeow 多设备协议（非 HTTP 供应商）。QR 扫码配对在本仓，也可用手机号取配对码；会话密钥材料存 Postgres。整通道有总闸 whatsapp.enable，显式为 on 才装配，默认不开（非官方客户端，账号可被封）。本包没有任何资金链路。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `whatsapp.go` | 起停与登录 | 中 |
| `wa_client.go` | 起停与登录 | 中 |
| `wa_pairing.go` | 配对（扫码 / 配对码） | 中 |
| `wa_pairing_code.go` | 配对（扫码 / 配对码） | 中 |
| `wa_media.go` | 发消息与媒体 | 中 |
| `wa_contacts.go` | 联系人同步（新账号必拉） | 中 |
| `store_postgres.go` | 会话存储 | 中 |
| `wa_delete_device.go` | 吊销配对 | 中 |
| `wa_antiban.go` | 防封与代理 | 中 |
| `wa_proxy.go` | 防封与代理 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `wa_client.go` | 类型 InboundMessage/DeliveryReceipt/ConnEvent/WaContact/WaGroup/OutboundMessage；函数 SetProxy/Login/LoggedIn/Connected/Disconnect/Logout/SendText/SendAppState | 中 |
| `wa_manager.go` | 类型 SessionInfo；函数 StartClient/StopClient/StopAll/ListClients/ReapplyProxy/SetSendRateOverride/WhatsappSendSlotWait | 中 |
| `wa_pairing.go` | 类型 QREvent/QRSession；函数 Subscribe/Cancel/Done/PairedJID/QRChannel/Connect/Disconnect/Device | 中 |
| `wa_media.go` | 类型 OutboundImage；函数 SendImage/WhatsappSendImage | 中 |
| `wa_contacts.go` | 函数 WhatsappSyncContacts | 中 |
| `wa_reachout.go` | 内部实现 | 中 |
| `wa_pairing_code.go` | 函数 Error/Unwrap/WhatsappGetPairingCode | 中 |
| `wa_proxy.go` | 函数 WhatsappSetProxy/WhatsappClearProxy/WhatsappProxy | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`InboundMessage`、`DeliveryReceipt`、`ConnEvent`、`WaContact`、`WaGroup`、`OutboundMessage`、`SendResult`、`SetProxy`、`Login`、`LoggedIn`、`Connected`、`Disconnect`、`Logout`、`SendText`、`SendAppState`、`GetLIDForPN`、`GetContacts`、`SyncContactAppState` 等
- **允许依赖**：`config`、`corectx`、`gin`、`pay`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`gin`、`pay`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`

## 5. 禁止跨模块直连约束

- 发送严格限频，号码在错误文案与日志里一律掩码。
- 读写门面自带按需登录：账号没在跑就先拉起并等连上再办事。
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
