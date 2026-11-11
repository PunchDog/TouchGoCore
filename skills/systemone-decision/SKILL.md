---
name: systemone-decision
description: 在定稿实施计划前、plan 模式下把 plan 写成文件之前，以及修改代码前，强制调用阿里云百炼 SystemOne 决策模型（decision-model-preview）对候选方案做结构化裁决（choice/noul/score 判定+概率+置信度）。plan 模式下落盘 plan.md/TODO/方案文档前必须先经此裁决，把 choice 方案选型、score 风险评级、noul 可行性判据写进 plan 再落盘，保证计划准确性，禁止跳过裁决直接落盘。也用于实时规划阶段比较多个实施方案、修改代码前评估改动方案、需要 choice 多选一裁决、score 风险/优先级评级、noul 是否判断的场景，拿到客观结构化判定而非仅靠语言模型自述。决策模型只输出结构化 JSON 不生成文本，适合作为方案裁决器（arbiter），不适合替代对话模型。
---

# SystemOne 决策模型方案裁决器

## 概述

把「待裁决的方案 + 若干 choice / noul / score 问题」发给百炼决策模型，一次前向拿回结构化判定与概率，作为定稿计划或动代码前的客观裁决依据。模型只判定不生成文本，适合裁决，不适合对话。

## 何时调用

在以下裁决点主动调用，不要等用户点名：

- **定稿实施计划前**：已列出多个候选方案，需客观裁定用哪个。
- **修改代码前**：评估改动方案的风险等级 / 是否安全 / 优先级。
- **比较取舍时**：两个方案各有优劣，靠语言模型自述容易偏，用 choice + confidence 拿量化判定。

不要在以下情况调用：模型已给出明确单一结论、用户已拍板、或只是查资料（那是搜索的事）。

## 强制闸：plan 模式下，写 plan 文件之前必须裁决

进入 plan 模式、准备把实施计划落盘成文件（`plan.md`、`TODO.md`、`.qoder/plan*`、`deliverables/` 下的方案文档等）**之前**，先调用本 skill 做一次裁决，再写文件。这是硬约束，不是建议：

1. 落盘前若有 ≥2 个候选实施方案，先构造 choice 问题让决策模型裁定，把判定写进 plan 的「方案选型」段，注明置信度。
2. 落盘前对拟定的改动范围 / 风险，构造 score 问题评级、noul 问题确认是否可行；置信度 <0.6 的裁决不要当作定论，标注「差距不明显，结合工程判断」并保留备选。
3. 只有当裁决完成、关键判据已落进 plan 内容，才把 plan 文件写出去。

跳过裁决直接落盘属于流程违规：计划中若有未裁决的方案分歧点，先补裁决再定稿。用户已明确拍板某个方案时豁免（此时无分歧，无需裁决）。

## 调用方法

脚本零依赖（Python 3.13 标准库），路径：
`C:\Users\TouchGo\.qoder-cn\skills\systemone-decision\scripts\systemone.py`

```bash
PY="C:/Users/TouchGo/AppData/Local/Programs/Python/Python313/python.exe"

"$PY" systemone.py \
  --state "把 Redis 连接改成走连接池，对比：A 直接改现有代码；B 新建一层封装" \
  --id plan \
  --choice "opt_a=直接改现有代码" \
  --choice "opt_b=新建封装层" \
  --question "哪个方案更适合" \
  --noul is_safe \
  --score risk=低,中,高 \
  --raw
```

关键参数：

| 参数 | 说明 |
|------|------|
| `--state` | 待裁决的业务上下文/方案描述（必填） |
| `--id` | choice 问题的问题 id（默认 `choice`） |
| `--choice` | 候选选项，格式 `选项id=选项描述`，可重复累积成一个 choice 问题 |
| `--noul` | 是非问题 id，可重复 |
| `--score` | 评分问题，格式 `等级id=低,中,高`，可重复 |
| `--question` | 问题的统一说明 |
| `--raw` | 额外打印原始 JSON |

## 鉴权（前置条件）

API Key 从环境变量读取，优先级 `SYSTEMONE_API_KEY` > `DASHSCOPE_API_KEY`。若缺失，脚本退出码 2 并提示。

用 Token Plan Key（`sk-sp-` 开头）走 token-plan 端点（默认，限时免费）；用按量 Key 则设 `SYSTEMONE_BASE_URL` 为 `{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`。

配置前先确认 Key 已就绪：`echo ${DASHSCOPE_API_KEY:+已设置}`。不要把 Key 写进代码或仓库。

## 输出解读

```
=== SystemOne 裁决 ===
state: 把 Redis 连接改成走连接池...
  plan: opt_b (置信度=0.86)
  is_safe: P(yes)=0.91
  risk: 中(idx=1.3, 置信度=0.79)
输入token=87, 延迟=45ms
```

把判定当**裁决参考**：choice 给出推荐项+置信度，noul 给是/否概率，score 给加权等级。置信度低（<0.6）时说明方案差距不明显，结合自己的工程判断定稿，不要盲信单一输出。

## 完整 API 细节

需要响应字段、错误码、限流等细节时读 `references/api.md`。

## 注意

- 决策模型**不生成文本**，只返回结构化 JSON。它裁决「用哪个方案」，不产出代码——代码仍由你生成。
- 上下文上限 65536 token，`state` 太长会被拒；问题数 ≤16。
- 网络失败退出码 3、解析失败 4；脚本错误走 stderr，成功摘要走 stdout。
