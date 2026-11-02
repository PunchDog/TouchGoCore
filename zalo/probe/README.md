# Zalo 卡片真机探针

验证 Zalo 链接卡片发送链路（上游 `SendLink`；缩略图/标题/描述由外链页 OG 元数据决定）。

> **会真实发送一条消息。** 只对测试账号/测试会话使用，不要在客户会话上跑。

## 前置条件

1. 已有可用的 Zalo 个人号会话（扫码或 cookie 导入过），且 `zalo_sessions` 表里有该账号。
2. 能连到该会话库的 Postgres DSN。
3. 一个可控的收件人（单聊 UID 或群 ID）。

## 步骤

```bash
# 1. 复制模板
cp zalo/probe/env.example zalo/probe/probe.env

# 2. 填写 DSN / 账号 / 收件人 / 外链（probe.env 已在 .gitignore 中）

# 3. 先干跑，只校验配置
bash zalo/probe/run.sh --dry-run

# 4. 真发（单聊）
bash zalo/probe/run.sh

# 群聊
bash zalo/probe/run.sh --group
```

Windows PowerShell：

```powershell
Copy-Item zalo/probe/env.example zalo/probe/probe.env
powershell -File zalo/probe/run.ps1 -DryRun
powershell -File zalo/probe/run.ps1
powershell -File zalo/probe/run.ps1 -Group
```

> `run.ps1` 保存为 **UTF-8 with BOM**：Windows PowerShell 5.1 没有 BOM 会按本地代码页
> 解析中文，导致引号错位与语法错误。编辑脚本时不要去掉 BOM。

## 通过标准

测试输出 `--- PASS: TestZACardProbeRealDevice`，并打印 `msgID`；收件端应看到「缩略图 + 文字」卡片，点击可打开外链。缩略图/标题/描述由外链页面的 OG 元数据决定。

## 失败对照

| 现象 | 含义 / 处理 |
|---|---|
| `SKIP` 并提示缺变量 | 没有加载到 probe.env，检查路径与必填项 |
| 登录/凭证错误 | 该账号未在 `zalo_sessions` 中或凭证已失效，先扫码/cookie 导入 |
| 卡片发送报错但测试通过 | 走了拒绝降级：日志有 `zalo 卡片被服务端拒收`，收件端收到「文字 + 链接」 |
| 缩略图不显示 | 目标页缺少 `og:image`，或页面不能被 Zalo 侧访问（内网/防盗链/Cloudflare 会失败） |
| `msgID` 为空 | 服务端未受理；查看日志中的 Zalo 错误码 |

核心未知：服务端是否接受跳过 `ParseLink` 的显式 `thumb/title/desc`。若被拒且降级成为常态，回退到「目标页 OG + 现有 SendLink」方案，并把结论写回 [docs/zalo-card-design.md](../../docs/zalo-card-design.md)。
