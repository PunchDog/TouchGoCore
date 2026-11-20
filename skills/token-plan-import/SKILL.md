---
name: token-plan-import
description: 把 OpenAI 兼容模型批量导入到 WorkBuddy / CodeBuddy / Qoder 等客户端，默认自动识别当前宿主。调用时先让用户从 providers.json 选择订阅商并输入 api-key，由 agent 把 key 写入该订阅商对应的用户级系统环境变量，再按既有流程导入。支持两种内置订阅商：token-plan（千问 Token Plan 默认 6 个模型，用 TOKEN_PLAN_API_KEY）与 opencode-go（从本地网关 http://127.0.0.1:7863/opencode/v1 实时拉取模型清单，用 OPEN_CODE_TOKEN_PLAN_API_KEY），可通过 providers.json 扩展。触发词：token plan、千问 token plan、导入模型、批量导入、qianwenai、opencode go、opencode-go 导入、open_code_token_plan、codebuddy 导入、选择订阅商、订阅商导入。目标格式：auto（自动识别宿主）、WorkBuddy（写 ~/.workbuddy/models.json）、CodeBuddy（写 ~/.codebuddy/models.json，{"models":[...]} 包装）、Qoder（直写 settings.json providers 段，--out 改为导出粘贴片段）、generic（通用片段）。显示名统一加来源前缀（token plan: / opencode:），导入前清理同来源旧模型。Qoder 导入模型默认思考强度为 high。
agent_created: true
---

# Token Plan / OpenCode Go 模型批量导入

## 概述

把模型按目标客户端的配置格式批量生成并写入，避免手工逐个填写 baseURL / apiKey / 模型 ID。

两个维度：

- **来源 `--source`**：`token-plan`（默认，6 个模型）或 `opencode-go`（每次运行都从 `<base-url>/models` 实时拉取，不写死清单）。
- **目标 `--target`**：`auto`（默认，自动识别宿主客户端）、`workbuddy`（写 `~/.workbuddy/models.json`，顶层数组）、`codebuddy`（写 `~/.codebuddy/models.json`，`{"models": [...]}` 对象包装，条目字段与 WorkBuddy 相同）、`qoder`（直写 Qoder `settings.json` 的 `providers` 段；加 `--out` 则改为导出 JSON 粘贴片段）、`generic`（最小 OpenAI 兼容片段）。

**宿主自动识别**（`--target auto`）：

1. 环境变量 `QODER_CONFIG_DIR` / `QODER_PRODUCT_ID` 存在 → Qoder；`WORKBUDDY_*` 前缀变量存在 → WorkBuddy；`CODEBUDDY_*` 前缀变量存在 → CodeBuddy。
2. 兜底看脚本安装路径：落在 `~/.qoder-cn/skills/` → Qoder，落在 `~/.workbuddy/` → WorkBuddy，落在 `~/.codebuddy/` → CodeBuddy。
3. 都识别不出则报错，要求显式 `--target`。

**显示名前缀**：`token-plan` → `token plan`，`opencode-go` → `opencode`。WorkBuddy / CodeBuddy 的 `name` 字段为 `前缀:模型ID`（如 `"name": "opencode:deepseek-v4.1-flash"`）；Qoder 的 `displayName` 为 `前缀:<显示名>`。模型 ID（`id` / `name`）保持规范不加前缀。网关返回名若自带前缀会先剥掉再统一加，保证结果确定。

**导入前清理**：默认先删除目标客户端里的同来源旧模型——同端点条目，以及带当前来源前缀但端点已变的历史残留；其他来源/其他端点的条目保留。`--keep-old` 可关闭清理。

**运行中客户端会覆写配置**：WorkBuddy / CodeBuddy / Qoder 运行时可能用内存态回写 `models.json` / `settings.json`，把刚导入的内容盖掉（实测 WorkBuddy 在写入 9 秒后覆写过一次）。导入后务必重启目标客户端；若发现前缀/清单丢失，关闭客户端后重跑导入。

本技能**只写 WorkBuddy / CodeBuddy / Qoder 等客户端配置，不改 `~/.config/opencode/opencode.json`**。

## 调用流程（必须先做，三步）

每次调用本 skill，agent 按以下顺序执行，不得跳过前两步直接跑脚本。

### 第 1 步：选择订阅商

读取同目录 `providers.json`，把 `providers[]` 的 `name` 列成选项让用户选（用 AskUserQuestion，可多列几个）。订阅商表以该文件为准，新增商家只改 JSON，不改脚本。

字段含义：

| 字段 | 作用 |
| --- | --- |
| `id` | 短名，如 `token-plan` |
| `name` | 展示给用户的选择项 |
| `source` | 传给脚本 `--source`，只能是 `token-plan` 或 `opencode-go` |
| `base_url` | 传给脚本 `--base-url` |
| `catalog` | `builtin`=用 `models` 清单；`live`=运行时拉 `<base_url>/models` |
| `env` | 该商家 api-key 对应的用户级系统变量名 |
| `models` / `vision_models` | 仅 `builtin` 需要，传给 `--models` / `--vision-models` |

### 第 2 步：输入 api-key 并写入系统变量

用户提供 key 后，agent 用 `setx` 写到用户级环境变量（与 `OPEN_CODE_TOKEN_PLAN_API_KEY` 持平，`HKCU\Environment`，当前权限即可，不需要管理员）：

```bash
setx "<该商家的 env 值>" "<用户提供的 key>"
```

- 禁止把 key 写进仓库文件或对话回显之外的落盘产物；只在命令参数里用一次。
- 脚本 `read_env()` 会先读进程环境，再兜底读 `HKCU\Environment`，所以 `setx` 后无需重开终端即可被脚本读到。
- 变量值与订阅商一一对应，不要复用另一个商家的变量（如 opencode-go 只读 `OPEN_CODE_TOKEN_PLAN_API_KEY`，不回退到 `TOKEN_PLAN_API_KEY`）。

### 第 3 步：按既有流程导入

把订阅商字段拼成脚本参数执行。脚本只内置 `TOKEN_PLAN_API_KEY` / `OPEN_CODE_TOKEN_PLAN_API_KEY` 两个变量名，新增商家的 `env` 脚本不认识。因此**统一把刚写入系统变量的 key 传给 `--api-key`**，这样任何订阅商都走同一套现有调用，不依赖脚本变量清单。

```bash
# 通用形式：source / base_url / models 都取自 providers.json 选中项
setx "<env>" "<key>"
python scripts/import_token_plan.py --target auto \
  --source "<source>" --base-url "<base_url>" --api-key "<key>" \
  [--models "<逗号分隔 models>" --vision-models "<逗号分隔 vision_models>"]

# token-plan（builtin 清单）
setx "TOKEN_PLAN_API_KEY" "<key>"
python scripts/import_token_plan.py --target auto --source token-plan \
  --base-url "https://token-plan.maas.qianwenaiapi.com/compatible-mode/v1" --api-key "<key>" \
  --models "glm-5.3,deepseek-v4-pro,deepseek-v4.1-flash-1017,qwen3.8-flash,qwen4-max,auto" \
  --vision-models "qwen4-max,glm-5.3"

# opencode-go（live 清单，无需 --models/--vision-models）
setx "OPEN_CODE_TOKEN_PLAN_API_KEY" "<key>"
python scripts/import_token_plan.py --target auto --source opencode-go \
  --base-url "http://127.0.0.1:7863/opencode/v1" --api-key "<key>"
```

`--api-key` 优先级最高，会覆盖脚本读环境变量的结果，两者值一致，所以既满足「setx 写入系统变量」又保证新商家可用。

后续导出片段（`--out`）、筛选（`--models`）、演练（`--dry-run`）等用法不变，见下文。

## 端点与凭据

内置订阅商 table 以同目录 `providers.json` 为准，下表仅作速查；扩展商家只改 JSON。

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

- WorkBuddy / CodeBuddy：`supportsReasoning=true`、`onlyReasoning=true`、`reasoning={ effort: "high", defaultEffort: "high", supportedEfforts: ["high","low"], summary: "auto", canDisableThinking: false }`。
- Qoder：provider models 的 `capabilities.thinking` 为 `{ modes: ["enabled"], supportedEffortLevels: ["high","low"], supportsEffort: true }`，另带 `reasoningSupported=true`、`reasoningEnabled=true`、`reasoningEffort="high"`（粘贴片段模式）。

依据：WorkBuddy 内置 model catalog 用 `reasoning.defaultEffort` + `onlyReasoning` 控制思考模式与强度。Qoder 应用源码（`resolveByokProtocolEffortPolicy` / `resolveChatModelReasoningEffort`）对 openai 协议默认强度为 `medium`，**不在模型强度清单里时回退取 `supportedEffortLevels[0]`**；settings.json 解析器不读任何 defaultEffort 字段，用户手选强度存在 state.vscdb（不可直写）。因此 `supportedEffortLevels` 必须 **high 放首位**（`["high","low"]`），默认思考才是 high；写成 `["low","high"]` 会默认落到 low。

## 用法

脚本：`scripts/import_token_plan.py`（Python 3，零第三方依赖，仅用标准库 `urllib` 拉 `/models`）。

### 1. 自动识别宿主并导入 opencode-go 模型（主用法）

```bash
python scripts/import_token_plan.py --source opencode-go
```

在 Qoder 会话里运行会自动直写 Qoder `settings.json`；在 WorkBuddy 里运行则写 `models.json`；在 CodeBuddy 里运行写其 `models.json`。也可显式指定：

```bash
python scripts/import_token_plan.py --target workbuddy --source opencode-go
python scripts/import_token_plan.py --target codebuddy --source opencode-go
python scripts/import_token_plan.py --target qoder --source opencode-go
```

行为：

- 实时拉取 `http://127.0.0.1:7863/opencode/v1/models`。
- **先清理同来源旧模型**（同端点 + 带来源前缀的历史残留），再写入本次清单；其他来源条目保留。`--keep-old` 可保留旧条目。
- WorkBuddy / CodeBuddy：写入前备份 `models.json.bak`；打印 `removed_old / added / total`。CodeBuddy 保持 `{"models": [...]}` 对象包装，条目字段与 WorkBuddy 一致。
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

- `--target auto|workbuddy|codebuddy|qoder|generic` 目标客户端，默认 auto（自动识别宿主）
- `--source token-plan|opencode-go` 模型来源，默认 token-plan
- `--models grok-4.7,auto` 从来源结果里筛选子集
- `--api-key <key>` 显式指定 Key
- `--base-url <url>` 覆盖默认端点（`/models` 拼在其后）
- `--vision-models kimi-k3,hy4` 覆盖图像模型清单
- `--out <path>` qoder/generic 导出粘贴片段（qoder 加此参数则不直写 settings.json），默认 stdout
- `--workbuddy-path <path>` 覆盖 models.json 路径
- `--codebuddy-path <path>` 覆盖 CodeBuddy models.json 路径
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
- Qoder 里思考默认不是 high：核对 `settings.json` providers 段模型的 `capabilities.thinking.supportedEffortLevels` 是否 high 在首位（`["high","low"]`）；Qoder 对 openai 协议默认 medium，不在清单时取列表第一个。重跑导入并重启 Qoder。
- Qoder 导入后模型不出现在下拉：先确认已重启 Qoder；再核对 `settings.json` 的 `providers` 段是否写入了 `type=openai-compatible` 的 provider。直写未生效时用 `--out` 导出片段走 UI 批量导入。
- 401/403：Key 过期或额度耗尽；OpenCode Go 受限模型（DeepSeek V4.1 Flash / V4 Pro、LongCat 2.5 Free 需 Global 区域；Muse Spark 1.x Contributor 需允许训练端点）还要在 Console › Settings › Privacy 放行。
