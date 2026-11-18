---
name: token-plan-import
description: 把 OpenAI 兼容模型批量导入到 WorkBuddy / Qoder 等客户端。支持两种模型来源：opencode-go（OpenCode Go 的 32 个模型，端点 https://opencode.ai/inference/go/v1）与 token-plan（千问 Token Plan 默认 6 个模型）。触发词：token plan、千问 token plan、导入模型、批量导入、qianwenai、opencode go、opencode-go 导入、open_code_token_plan。目标格式：WorkBuddy（写 ~/.workbuddy/models.json）、Qoder（生成可粘贴片段）、generic（通用片段）。密钥从环境变量 OPEN_CODE_TOKEN_PLAN_API_KEY 读取。
agent_created: true
---

# Token Plan / OpenCode Go 模型批量导入

## 概述

把模型按目标客户端的配置格式批量生成并写入，避免手工逐个填写 baseURL / apiKey / 模型 ID。

两个维度：

- **来源 `--source`**：`token-plan`（默认，千问 Token Plan 6 个模型）或 `opencode-go`（OpenCode Go 的 32 个模型，端点 `https://opencode.ai/inference/go/v1`）。
- **目标 `--target`**：`workbuddy`（写 `~/.workbuddy/models.json`）、`qoder`（输出 JSON 片段供粘贴，不写 `state.vscdb`）、`generic`（最小 OpenAI 兼容片段）。

本技能**只写 WorkBuddy / Qoder 等客户端配置，不改 `~/.config/opencode/opencode.json`**。OpenCode 自身的 Go provider 用 `/connect` 接入。

## 端点与凭据

| 来源 | Base URL | 模型 ID |
| --- | --- | --- |
| `token-plan` | `https://token-plan.maas.qianwenaiapi.com/compatible-mode/v1` | `glm-5.3` 等 |
| `opencode-go` | `https://opencode.ai/inference/go/v1` | 官方规范 ID，如 `claude-haiku-5-5`、`hy4-flash-free` |

- API Key 读取顺序：`--api-key` → 环境变量 `OPEN_CODE_TOKEN_PLAN_API_KEY` → 兼容旧名 `TOKEN_PLAN_API_KEY` → `~/.workbuddy/models.json` 中同 URL 的已有条目。
- Key 会按目标格式明文写入（WorkBuddy 的 `apiKey`、Qoder 的 `apiKey`），因为这两个客户端需要实际凭据；不要把这些配置文件提交到公开仓库。
- 控制台：Token Plan `https://platform.qianwenai.com/home/analytics/token-plan/individual`；OpenCode Go `https://opencode.ai/console/go`。

## opencode-go 模型清单

ID 取自官方 `https://opencode.ai/inference/go/v1/models`（裁决置信度 0.99），显示名用 Go 模型表原名。共 32 个：

| 显示名 | ID | 显示名 | ID |
| --- | --- | --- | --- |
| Grok 4.7 | `grok-4.7` | GPT 6 Luna | `gpt-6-luna` |
| GPT 5.6 Luna | `gpt-5.6-luna` | Claude Haiku 5.5 | `claude-haiku-5-5` |
| Gemini 4 Argon | `gemini-4-argon` | GLM-5.3-Flash | `glm-5.3-flash` |
| GLM-5.3 | `glm-5.3` | GLM-5.2 | `glm-5.2` |
| Kimi K3.1 Pro | `kimi-k3.1-pro` | Kimi K3 | `kimi-k3` |
| Kimi K2.7 Code | `kimi-k2.7-code` | LongCat-2.0 | `longcat-2.0` |
| MiMo-V2.6-Pro | `mimo-v2.6-pro` | MiMo-V2.6-Flash | `mimo-v2.6-flash` |
| Qwen4 Max | `qwen4-max` | Qwen3.8 Max | `qwen3.8-max` |
| Qwen3.8 Flash | `qwen3.8-flash` | Qwen3.7 Plus | `qwen3.7-plus` |
| MiniMax M3.1 Flash | `minimax-m3.1-flash` | MiniMax M3 | `minimax-m3` |
| Muse Spark 1.3 Contributor | `muse-spark-1.3-contributor` | Muse Spark 1.2 Contributor | `muse-spark-1.2-contributor` |
| DeepSeek V4.1 Flash | `deepseek-v4.1-flash` | DeepSeek V4 Pro | `deepseek-v4-pro` |
| Hy4 | `hy4` | Hy4 preview | `hy4-preview` |
| Hy3 | `hy3` | Step 5 Preview | `step-5-preview` |
| Beam Free | `beam-free` | Iceman Contributor Free | `iceman-contributor-free` |
| LongCat 2.5 Free | `longcat-2.5-free` | Hy4 flash Free | `hy4-flash-free` |

易错项：`claude-haiku-5-5`（不是 `claude-haiku-5.5`）、`minimax-m3`、`hy4-flash-free`。

`supportsImages` / `multiModalSupported` 按官方 `/v1/models` 的 input modalities 自动推断（含 image 即 true），可用 `--vision-models` 覆盖。

## 用法

脚本：`scripts/import_token_plan.py`（Python 3，零第三方依赖）。

### 1. 导入 opencode-go 模型到 WorkBuddy（主用法）

```bash
python scripts/import_token_plan.py --target workbuddy --source opencode-go
```

行为：

- 读取 `~/.workbuddy/models.json`，按 `id` 去重，已存在则跳过（`--overwrite` 覆盖）；写前备份 `models.json.bak`。
- 每条：`id=<规范 ID>`、`name=<显示名>`、`vendor="Custom"`、`url=https://opencode.ai/inference/go/v1`、`apiKey=<解析出的 Key>`、`supportsToolCall=true`、`supportsReasoning=true`、`supportsImages` 按模型推断、`useCustomProtocol=false`。

### 2. 导出 opencode-go 模型给 Qoder

```bash
python scripts/import_token_plan.py --target qoder --source opencode-go --out qoder-go-models.json
```

输出 32 条 JSON 数组，每条：`name=<规范 ID>`、`displayName=<显示名>`、`format="openai"`、`source="custom"`、`baseUrl=<端点>`、`apiKey=<Key>`、`multiModalSupported=<bool>`、`reasoningSupported=true`、`maxInputTokens=200000`、`enabled=true`、`isDefault=false`。

在 Qoder：设置 → 模型 → 自定义模型 → 批量导入/JSON 编辑，粘贴该数组。不直接改 `state.vscdb`。

### 3. 导入 token-plan 模型（默认来源）

```bash
python scripts/import_token_plan.py --target workbuddy
python scripts/import_token_plan.py --target qoder --out qoder-models.json
```

默认清单 `glm-5.3`、`deepseek-v4-pro`、`deepseek-v4.1-flash-1017`、`qwen3.8-flash`、`qwen4-max`、`auto`，用 `--models` 覆盖。

### 4. 通用片段 / 演练

```bash
python scripts/import_token_plan.py --target generic --source opencode-go --models grok-4.7,hy4-flash-free
python scripts/import_token_plan.py --target workbuddy --source opencode-go --dry-run
```

`generic` 输出最小 `{ id, name, baseUrl, apiKey }`，便于继续改造为 Continue / Cline 等格式；`--dry-run` 只打印不写文件。

## 常用参数

- `--target workbuddy|qoder|generic` 目标格式，默认 workbuddy
- `--source token-plan|opencode-go` 模型来源，默认 token-plan
- `--models grok-4.7,auto` 覆盖模型清单（逗号分隔规范 ID）
- `--api-key sk-...` 显式指定 Key
- `--base-url <url>` 覆盖默认端点
- `--vision-models kimi-k3,hy4` 覆盖图像模型清单
- `--out <path>` qoder/generic 输出文件，默认 stdout
- `--workbuddy-path <path>` 覆盖 models.json 路径
- `--overwrite` WorkBuddy 覆盖同名条目
- `--dry-run` 只打印，不写文件

## 排错

- 报 API key not found：设置用户环境变量 `OPEN_CODE_TOKEN_PLAN_API_KEY`、传 `--api-key`，或确保 WorkBuddy 已有同 URL 条目。
- Qoder 导入后模型不出现在下拉：确认版本支持「自定义模型 JSON 导入」，否则逐条手工添加 `name/baseUrl/apiKey`。
- 401/403：Key 过期或额度耗尽；OpenCode Go 受限模型（DeepSeek V4.1 Flash / V4 Pro、LongCat 2.5 Free 需 Global 区域；Muse Spark 1.x Contributor 需允许训练端点）还要在 Console › Settings › Privacy 放行。
- 模型名对不上：用官方 `/v1/models` 的 `id`，不要用显示名（如 `claude-haiku-5-5` 而非 `Claude Haiku 5.5`）。
- WorkBuddy 写入失败：确认 `~/.workbuddy/models.json` 存在且为合法 JSON 数组；退出码非 0 时保留 `.bak`。
