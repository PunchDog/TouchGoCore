# SystemOne 决策模型 API 速查

决策模型 `decision-model-preview`（阿里云百炼，限时免费）通过 TypeSafe System One 协议调用，
一次前向返回结构化判定与概率，**不生成自然语言文本**。

## 端点

```
POST https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/systemone   # Token Plan（sk-sp- 开头 Key）
POST https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/systemone  # 按量付费
POST https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/systemone  # 新加坡
```

## 鉴权

```
Authorization: Bearer <API_KEY>
Content-Type: application/json
```

环境变量优先级：`SYSTEMONE_API_KEY` > `DASHSCOPE_API_KEY`。

## 请求体

```json
{
  "model": "decision-model-preview",
  "state": "待裁决的业务上下文（字符串 / 对象 / 数组）",
  "questions": {
    "问题id": {
      "type": "choice",
      "instructions": "问题说明",
      "criteria": { "选项id": "选项描述", "另一个": "描述" }
    },
    "是否问题": { "type": "noul", "instructions": "是否成立？" },
    "评分问题": { "type": "score", "instructions": "风险等级", "criteria": ["低","中","高"] }
  }
}
```

## 三种问题类型

| 类型 | 用途 | 响应字段 | 返回值 |
|------|------|----------|--------|
| `choice` | 多选一（≤255 选项） | `choice` + `confidence` + `probabilities` | 选中项 + 置信度 |
| `noul` | 是非判断 | `noul` | P(yes)，0~1 |
| `score` | 有序量表（2~255 级，建议 3~7） | `score` + `confidence` + `legend` | 概率加权等级索引 |

## 限制

- 问题数上限 **16**（延迟随问题数近线性增长）
- 上下文上限 **65536 token**，超长 `state` 直接拒绝
- `choice` 选项 ≤ 255；`score` 等级 2~255

## 响应示例

```json
{
  "model": "decision-model-preview",
  "request_id": "1234-abcd",
  "answers": {
    "plan":  { "type": "choice", "choice": "opt_a", "confidence": 0.87, "probabilities": {"opt_a":0.87,"opt_b":0.13} },
    "risk":  { "type": "score", "score": 1.3, "confidence": 0.79, "legend": {"0":"低","1":"中","2":"高"} }
  },
  "usage": { "input_tokens": 87 },
  "latency_ms": 45
}
```

## 计费与限流

- **成本与输出长度无关**，仅计 `input_tokens`
- 限时免费期：北京 / 新加坡均 0 元
- 限流：RPM 1200 / TPM 2,000,000（≈20 QPS）

## 来源

- 决策模型 API（中文）：https://www.alibabacloud.com/help/zh/model-studio/decision-model-api
- 模型信息：https://help.aliyun.com/zh/model-studio/decision-model-preview
- 协议实测：https://developer.aliyun.com/article/1770052
