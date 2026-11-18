---
name: token-plan-import
description: 把 OpenAI 兼容模型批量导入到 WorkBuddy / Qoder 等客户端，默认自动识别当前宿主。支持两种模型来源：opencode-go（每次从本地网关 http://127.0.0.1:7863/opencode/v1 实时拉取模型清单）与 token-plan（千问 Token Plan 默认 6 个模型）。触发词：token plan、千问 token plan、导入模型、批量导入、qianwenai、opencode go、opencode-go 导入、open_code_token_plan。目标格式：auto（自动识别宿主）、WorkBuddy（写 ~/.workbuddy/models.json）、Qoder（直写 settings.json providers 段，--out 改为导出粘贴片段）、generic（通用片段）。显示名统一加来源前缀（token plan: / opencode:），导入前清理同来源旧模型。token-plan 用 TOKEN_PLAN_API_KEY，opencode-go 用 OPEN_CODE_TOKEN_PLAN_API_KEY。
agent_created: true
---

# Token Plan / OpenCode Go 模型批量导入

## 概述

把模型按目标客户端的配置格式批量生成并写入，避免手工逐个填写 baseURL / apiKey / 模型 ID。

两个维度：

- **来源 `--source`**：`token-plan`（默认，6 个模型）或 `opencode-go`（每次运行都从 `<base-url>/models` 实时拉取，不写死清单）。
- **目标 `--target`**：`auto`（默认，自动识别宿主客户端）、`workbuddy`（写 `~/.workbuddy/models.json`）、`qoder`（直写 Qoder `settings.json` 的 `providers` 段；加 `--out` 则改为导出 JSON 粘贴片段）、`generic`（最小 OpenAI 兼容片段）。

**宿主自动识别**（`--target auto`）：

1. 环境变量 `QODER_CONFIG_DIR` / `QODER_PRODUCT_ID` 存在 → Qoder；`WORKBUDDY_*` 前缀变量存在 → WorkBuddy。
2. 兜底看脚本安装路径：落在 `~/.qoder-cn/skills/` → Qoder，落在 `~/.workbuddy/` → WorkBuddy。
3. 都识别不出则报错，要求显式 `--target`。

**显示名前缀**：`token-plan` → `token plan`，`opencode-go` → `opencode`。WorkBuddy 直接把 `name` 字段赋值为前缀本身（如 `"name": "opencode"`），客户端自行拼接显示；Qoder 的 `displayName` 用 `前缀:<名>` 形态。模型 ID 保持规范不加前缀。网关返回名若自带前缀会先剥掉再统一加（Qoder 侧），保证结果确定。

**导入前清理**：默认先删除目标客户端里的同来源旧模型——同端点条目，以及带当前来源前缀但端点已变的历史残留；其他来源/其他端点的条目保留。`--keep-old` 可关闭清理。

**运行中客户端会覆写配置**：WorkBuddy / Qoder 运行时可能用内存态回写 `models.json` / `settings.json`，把刚导入的内容盖掉（实测 WorkBuddy 在写入 9 秒后覆写过一次）。导入后务必重启目标客户端；若发现前缀/清单丢失，关闭客户端后重跑导入。

本技能**只写 WorkBuddy / Qoder 等客户端配置，不改 `~/.config/opencode/opencode.json`**。

## 端点与凭据

| 来源 | Base URL | 模型 ID |
| --- | --- | --- |
| `token-plan` | `https://token-plan.maas.qianwenaiapi.com/compatible-mode/v1` | `glm-5.3` 等 |
| `opencode-go` | `http://127.0.0.1:7863/opencode/v1` | 实时 `/models` 返回的 `id` |

- `opencode-go` **严格只认** `OPEN_CODE_TOKEN_PLAN_API_KEY`（或 `--api-key`），不会回退到旧变量名 `TOKEN_PLAN_API_KEY`，也不会复用 `models.json` 里的旧 Key，避免导入旧凭据。
- `token-plan` 读取顺序（沿用原行为）：`--api-key` → `TOKEN_PLAN_API_KEY` → `models.json` 已有条目；不读 `OPEN_CODE_TOKEN_PLAN_API_KEY`。
- Windows 上若 `setx` 设置的变量还没进入当前进程树，脚本会从 `HKCU\Environment` 注册表读取同一个变量值。
- Key 会按目标格式明文写入（WorkBuddy 的 `apiKey`、Qoder 的 `apiKey`），这两个客户端需要实际凭据；不要提交到公开仓库。

`opencode-go` 端点是本地网关 `http://127.0.0.1:7863/opencode/v1`：由外部负责注入 OpenCode Go 要求的 `x-opencode-session`（直连 Go 会因缺该头返回 400 `MissingSessionID`，WorkBuddy 显示「模型服务拒绝了测试请求」）。导入只负责把该地址写进客户端。

## 模型来源

`--source opencode-go` 每次运行请求 `http://127.0.0.1:7863/opencode/v1/models`，解析 `data[]`：

- `id` → 客户端模型 ID（如 `claude-haiku-5-5`、`hy4-flash-free`）
- `name` → 显示名（如 `Claude Haiku 5.5`）
- `modalities.input` 含 `image*` → 图像能力为 true

不写死任何模型清单；官方新增/下线模型会在下次导入时自动生效。`--models` 可从实时结果里筛选子集。校验时（2026-11-18）该端点返回 32 个模型。

`--source token-plan` 用内置默认清单 `glm-5.3`、`deepseek-v4-pro`、`deepseek-v4.1-flash-1017`、`qwen3.8-flash`、`qwen4-max`、`auto`。

## 思考模式

导入的每条模型都显式开启高思考：

- WorkBuddy：`supportsReasoning=true`、`onlyReasoning=true`、`reasoning={ effort: "high", defaultEffort: "high", supportedEfforts: ["low","high"], summary: "auto", canDisableThinking: false }`。
- Qoder：`reasoningSupported=true`、`reasoningEnabled=true`、`reasoningEffort="high"`，并带同样的 `reasoning` 对象。

依据：WorkBuddy 内置 model catalog 用 `reasoning.defaultEffort` + `onlyReasoning` 控制思考模式与强度。

## 用法

脚本：`scripts/import_token_plan.py`（Python 3，零第三方依赖，仅用标准库 `urllib` 拉 `/models`）。

### 1. 自动识别宿主并导入 opencode-go 模型（主用法）

```bash
python scripts/import_token_plan.py --source opencode-go
```

在 Qoder 会话里运行会自动直写 Qoder `settings.json`；在 WorkBuddy 里运行则写 `models.json`。也可显式指定：

```bash
python scripts/import_token_plan.py --target workbuddy --source opencode-go
python scripts/import_token_plan.py --target qoder --source opencode-go
```

行为：

- 实时拉取 `http://127.0.0.1:7863/opencode/v1/models`。
- **先清理同来源旧模型**（同端点 + 带来源前缀的历史残留），再写入本次清单；其他来源条目保留。`--keep-old` 可保留旧条目。
- WorkBuddy：写入前备份 `models.json.bak`；打印 `removed_old / added / total`。
- Qoder：按 `providers` 段实测 schema 写入一个 `qoder-custom-<uuid>` provider（`type=openai-compatible`、`protocol=openai`、`authType=bearer`，models 数组含 thinking/vision 能力），备份 `settings.json.bak`；打印 `removed_old_providers / added_provider / models`。**导入后重启 Qoder** 才会出现在模型列表。

### 2. 导出 opencode-go 模型粘贴片段（Qoder UI 手工导入兜底）

```bash
python scripts/import_token_plan.py --target qoder --source opencode-go --out qoder-go-models.json
```

输出 JSON 数组，每条含 `name`（规范 ID）、`displayName`（显示名）、`format="openai"`、`source="custom"`、`baseUrl`、`apiKey`、`multiModalSupported`、`reasoningSupported` / `reasoningEnabled` / `reasoningEffort` / `reasoning`、`maxInputTokens=200000`、`enabled=true`、`isDefault=false`。

在 Qoder：设置 → 模型 → 自定义模型 → 批量导入/JSON 编辑，粘贴该数组。此为直写 `settings.json` 失败时的兜底；不直接改 `state.vscdb`。

### 3. 导入 token-plan 模型（默认来源）

```bash
python scripts/import_token_plan.py --target workbuddy
python scripts/import_token_plan.py --target qoder --out qoder-models.json
```

### 4. 筛选 / 演练

```bash
python scripts/import_token_plan.py --target workbuddy --source opencode-go --models grok-4.7,hy4-flash-free
python scripts/import_token_plan.py --target workbuddy --source opencode-go --dry-run
python scripts/import_token_plan.py --target generic --source opencode-go
```

## 常用参数

- `--target auto|workbuddy|qoder|generic` 目标客户端，默认 auto（自动识别宿主）
- `--source token-plan|opencode-go` 模型来源，默认 token-plan
- `--models grok-4.7,auto` 从来源结果里筛选子集
- `--api-key <key>` 显式指定 Key
- `--base-url <url>` 覆盖默认端点（`/models` 拼在其后）
- `--vision-models kimi-k3,hy4` 覆盖图像模型清单
- `--out <path>` qoder/generic 导出粘贴片段（qoder 加此参数则不直写 settings.json），默认 stdout
- `--workbuddy-path <path>` 覆盖 models.json 路径
- `--qoder-settings-path <path>` 覆盖 Qoder settings.json 路径（默认取 `QODER_CONFIG_DIR` 或 `~/.qoder-cn`）
- `--keep-old` 保留同来源旧条目（默认先清理）
- `--dry-run` 只打印，不写文件

## 排错

- `cannot detect host client`：auto 识别失败（无宿主环境变量且脚本不在客户端安装目录），显式传 `--target`。
- 导入后前缀/清单丢失：运行中的客户端用内存态回写了配置。关闭客户端重跑导入，再启动。
- `API key not found`：token-plan 用 `TOKEN_PLAN_API_KEY`，opencode-go 用 `OPEN_CODE_TOKEN_PLAN_API_KEY`，或传 `--api-key`。两者互不串用。
- `failed to fetch http://127.0.0.1:7863/opencode/v1/models`：本地网关没起或网络不通；偶发 403 可能是网关会话态抖动，稍后重试。
- 模型名对不上：用 `/models` 返回的 `id`，不要用显示名。
- WorkBuddy 里思考仍是关闭：确认 `reasoning.defaultEffort="high"` 与 `onlyReasoning=true` 已写入；导入后重启 WorkBuddy。
- Qoder 导入后模型不出现在下拉：先确认已重启 Qoder；再核对 `settings.json` 的 `providers` 段是否写入了 `type=openai-compatible` 的 provider。直写未生效时用 `--out` 导出片段走 UI 批量导入。
- 401/403：Key 过期或额度耗尽；OpenCode Go 受限模型（DeepSeek V4.1 Flash / V4 Pro、LongCat 2.5 Free 需 Global 区域；Muse Spark 1.x Contributor 需允许训练端点）还要在 Console › Settings › Privacy 放行。
