# agents.md — util

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L2
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

高频共用工具：回调注册表（含各通道密钥注入钩子）、协议类型注册与打包、字符串/哈希/HTTP/网络/时间/随机封装。不是叶子层，L0/L1 的包不能反向引用它。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `callback.go` | 注册回调钩子（含密钥注入） | 中 |
| `const.go` | 注册回调钩子（含密钥注入） | 中 |
| `echoProtocol.go` | 协议类型与消息打包 | 中 |
| `hash.go` | 哈希与转换 | 中 |
| `convert.go` | 哈希与转换 | 中 |
| `http.go` | HTTP 客户端 | 中 |
| `net.go` | 网络与路径 | 中 |
| `path.go` | 网络与路径 | 中 |
| `random.go` | 随机封装 | 中 |
| `string.go` | 字符串/排序/反射/时间 | 中 |
| `numbersort.go` | 字符串/排序/反射/时间 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `time.go` | 类型 VirtualTimeData/VirtualTimeState；函数 HGetAll/HSet/Del/VirtualTimeRedisKey/RefreshVirtualTime/SetVirtualTimePollInterval/GetVirtualTimeData/SetVirtualTimeData | 中 |
| `callback.go` | 类型 CallFunction；函数 Register/Unregister/Do/DoWithRet/DoWithRetCtx | 中 |
| `string.go` | 函数 RandomStr/Sprintf/ParseDbData/Clip | 中 |
| `http.go` | 类型 MultipartFormField；函数 HTTPGet/HttpPost/HttpPostByContentType/PostJSONAuth/PostJSON/PostFile/PostMultipartForm | 中 |
| `echoProtocol.go` | 类型 ProtocolBinding/EchoPacket；函数 RegisterProtocolType/RegisterProtocolTypes/LookupProtocolType/InitEchoPacket/Serialize/GetLength/GetProtocol2/GetProtocol1 | 中 |
| `net.go` | 类型 IPInfo/IPData；函数 CheckPort/IsIntranetIP | 中 |
| `reflect_format.go` | 函数 GetClassName/FormatStruct | 中 |
| `numbersort.go` | 类型 Numeric/NumberSortLess/NumberSortDesc；函数 Len/Less/Swap | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`VirtualTimeData`、`VirtualTimeState`、`HGetAll`、`HSet`、`Del`、`VirtualTimeRedisKey`、`RefreshVirtualTime`、`SetVirtualTimePollInterval`、`GetVirtualTimeData`、`SetVirtualTimeData`、`CalculateVirtualTime`、`CurrentTime`、`RealTime`、`RealMS`、`ResetVirtualTime`、`SetVirtualTime`、`GetVirtualTime`、`InitVirtualTime` 等
- **允许依赖**：`network/message`、`random`、`syncmap`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`network/message`、`random`、`syncmap`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`bsc`、`chainroute`、`db`、`example/gatewayserver`、`example/loginserver`、`gin`、`golua`、`list`、`localtimer`、`mapmanager`、`nft`、`rpc`、`sol`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `network/message`、`random`、`syncmap`、`vars` 方向；禁止反向引用上层包。
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
