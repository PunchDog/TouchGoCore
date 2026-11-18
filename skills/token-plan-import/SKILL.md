---
name: token-plan-import
description: 把千问 AI Token Plan（platform.qianwenai.com）的 OpenAI 兼容模型（glm-5.3、deepseek-v4-pro、deepseek-v4.1-flash-1017、qwen3.8-flash、qwen4-max、auto 等）批量导入到 WorkBuddy、Qoder 等支持 OpenAI 兼容协议的客户端。触发词：token plan、千问 token plan、导入模型、批量导入、qianwenai、qwen4-max、deepseek-v4、glm-5.3。适配 WorkBuddy（写 ~/.workbuddy/models.json）与 Qoder（生成可粘贴片段）两种格式。
agent_created: true
---

# Token Plan 模型批量导入

## 概述

把千问 AI Token Plan 下的指定模型，按目标客户端的配置格式批量生成并写入，避免手工逐个填写 baseURL / apiKey / 模型 ID。当前适配两种目标：

- **WorkBuddy**：直接追加到 `~/.workbuddy/models.json`（数组结构）。
- **Qoder**：因模型存放在 `state.vscdb`（SQLite，运行期被占用），脚本只输出 JSON 片段供在设置界面「自定义模型」里粘贴导入，不直接写库。

## 端点与凭据

- Base URL：`https://token-plan.maas.qianwenaiapi.com/compatible-mode/v1`
- API Key：通过环境变量 `TOKEN_PLAN_API_KEY` 传入，或用 `--api-key` 参数显式指定；未提供时脚本从 `~/.workbuddy/models.json` 中已存在的 token-plan 条目自动复用。
- 控制台：`https://platform.qianwenai.com/home/analytics/token-plan/individual`

安全提示：API Key 属敏感凭据，不要硬编码到 skill 文件或公开仓库。

## 默认模型清单

`glm-5.3`、`deepseek-v4-pro`、`deepseek-v4.1-flash-1017`、`qwen3.8-flash`、`qwen4-max`、`auto`。

需要增删时用 `--models` 覆盖，逗号分隔。

## 用法

脚本：`scripts/import_token_plan.py`（Python 3，零第三方依赖）。

### 1. 导入到 WorkBuddy（默认写 `~/.workbuddy/models.json`）

```bash
python scripts/import_token_plan.py --target workbuddy
```

行为：

- 读取现有 `models.json`，按模型 `id` 去重，已存在则跳过（可用 `--overwrite` 强制覆盖）。
- 追加新条目并写回；每次写入前先备份原文件到 `models.json.bak`。
- WorkBuddy 字段固定填 `vendor="Custom"`、`url=<token-plan baseURL>`、`apiKey=<key>`；`supportsToolCall=true`、`supportsReasoning=true`；`supportsImages` 按模型名推断（qwen4-max / glm-5.3 视为支持，其余 false，可用 `--vision-models` 覆盖）。

### 2. 导入到 Qoder（生成片段，不直接写库）

```bash
python scripts/import_token_plan.py --target qoder --out qoder-models.json
```

输出一个 JSON 数组，每条按 Qoder「自定义模型」schema：

```
{ "name": <model_id>, "displayName": <model_id>, "format": "openai",
  "source": "custom", "baseUrl": <token-plan baseURL>, "apiKey": <key>,
  "multiModalSupported": <bool>, "reasoningSupported": true,
  "maxInputTokens": 200000, "enabled": true, "isDefault": false }
```

在 Qoder：设置 → 模型 → 自定义模型 → 批量导入/JSON 编辑，粘贴该数组即可。Qoder 运行时请勿直接改 `state.vscdb`。

### 3. 仅打印通用 OpenAI 兼容片段（适配其他客户端）

```bash
python scripts/import_token_plan.py --target generic --out models.json
```

输出最小化 OpenAI 兼容条目 `{ id, name, baseUrl, apiKey }`，便于继续改造为 Continue / Cline / LM Studio 等格式。

## 常用参数

- `--models glm-5.3,auto` 覆盖默认清单
- `--api-key sk-...` 显式指定 Key
- `--base-url <url>` 覆盖默认 baseURL
- `--vision-models qwen4-max,glm-5.3` 标记支持图像的模型
- `--overwrite` WorkBuddy 模式下覆盖同名条目
- `--dry-run` 只打印，不写文件

## 排错

- WorkBuddy 写入失败：确认 `~/.workbuddy/models.json` 存在且为合法 JSON 数组；脚本退出码非 0 时保留 `.bak` 备份。
- Qoder 导入后模型不出现在下拉：确认 Qoder 版本支持「自定义模型 JSON 导入」，否则逐条手工添加 `name/baseUrl/apiKey`。
- 401/403：Token Plan Key 过期或额度耗尽，去控制台核对。