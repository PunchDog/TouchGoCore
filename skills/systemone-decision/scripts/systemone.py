#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""SystemOne 决策模型裁决脚本。

用途：把待裁决的方案 + 若干 choice / noul / score 问题发给阿里云百炼
决策模型（decision-model-preview），一次前向拿回结构化判定与概率，
供 Agent 在「定稿计划前 / 改代码前」做方案裁决。

输入：命令行参数（见 argparse）。
输出：stdout 打印 LLM 友好的判定摘要 + 原始 JSON（--raw 时）。
退出码：0 = 调用成功；2 = 参数/配置缺失；3 = 网络/HTTP 错误；4 = 响应解析失败。
"""

import argparse
import json
import os
import sys
import time
import urllib.request
import urllib.error

# Windows 控制台默认 GBK，强制 stdout/stderr 走 UTF-8，避免中文摘要乱码
if sys.stdout.encoding and sys.stdout.encoding.lower() != "utf-8":
    sys.stdout.reconfigure(encoding="utf-8")
if sys.stderr.encoding and sys.stderr.encoding.lower() != "utf-8":
    sys.stderr.reconfigure(encoding="utf-8")

# ---------- 常量：端点与默认模型 ----------
# 决策模型端点，{workspace} 用真实 workspace id 替换
DEFAULT_BASE = "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"
# 当前百炼决策模型编码（限时免费阶段）
DEFAULT_MODEL = "decision-model-preview"

# 裁决回执路径：PreToolUse 闸（hooks/systemone-gate.mjs）凭它判断近期是否裁决过
RECEIPT_PATH = os.path.join(os.path.expanduser("~"), ".qoder-cn", "state", "systemone-receipt.json")


def write_receipt(kind, detail):
    """裁决成功或豁免登记后写回执，供代码编辑闸校验时效。"""
    receipt = {"ts": time.time(), "kind": kind}
    receipt.update(detail)
    os.makedirs(os.path.dirname(RECEIPT_PATH), exist_ok=True)
    with open(RECEIPT_PATH, "w", encoding="utf-8") as f:
        json.dump(receipt, f, ensure_ascii=False)


def _parse_choice(raw):
    """解析单个 choice 选项片段 选项id=选项描述。

    描述里可能含 '='，所以第一个 '=' 前是选项 id，其后整段是描述。
    返回 (选项id, 选项描述)；格式非法时直接退出。
    """
    opt_id, _, opt_desc = raw.partition("=")
    if not opt_id or not opt_desc:
        die(f"choice 项格式应为 选项id=选项描述，收到：{raw}", 2)
    return opt_id.strip(), opt_desc.strip()


def build_payload(args):
    """根据命令行参数构造 SystemOne 请求体。

    一个 choice 问题由多次 --choice 累积成多个候选，问题 id 取当前 --id。
    score 问题 criteria 是等级描述列表，noul 可省略。
    """
    questions = {}
    # choice：当前 --id 下的候选选项累积为 criteria 对象 {选项id: 选项描述}
    cur_choice_id = args.id or "choice"
    for raw in args.choice or []:
        opt_id, opt_desc = _parse_choice(raw)
        q = questions.setdefault(
            cur_choice_id, {"type": "choice", "criteria": {}, "instructions": args.question}
        )
        q["criteria"][opt_id] = opt_desc

    # noul 是非判断，只要问题说明
    for qid in args.noul or []:
        questions.setdefault(qid, {"type": "noul", "instructions": args.question})

    # score 等级量表，criteria 是从低到高的等级名列表
    for raw in args.score or []:
        qid, _, levels = raw.partition("=")
        if not qid or not levels:
            die(f"score 项格式应为 等级id=低,中,高，收到：{raw}", 2)
        questions.setdefault(qid, {"type": "score", "instructions": args.question})
        questions[qid]["criteria"] = levels.split(",")

    if not questions:
        die("至少需要一个 choice/noul/score 问题", 2)

    return {
        "model": args.model,
        # state 是待裁决的业务上下文，非字符串会被服务端序列化
        "state": args.state,
        "questions": questions,
    }


def call_systemone(payload, base_url, api_key):
    """发 HTTP 请求到 SystemOne 端点，返回解析后的 dict。

    鉴权：Bearer token，来自 --api-key 或环境变量。
    """
    url = base_url.rstrip("/") + "/systemone"
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=args_timeout) as resp:
            body = resp.read().decode("utf-8")
        return json.loads(body)
    except urllib.error.HTTPError as e:
        die(f"HTTP {e.code}：{e.read().decode('utf-8', 'ignore')}", 3)
    except urllib.error.URLError as e:
        die(f"网络错误：{e.reason}", 3)
    except json.JSONDecodeError as e:
        die(f"响应不是合法 JSON：{e}", 4)


# 占位，稍后由 main 赋值，避免全局传参混乱
args_timeout = 60


def summarize(result):
    """把响应压缩成简短摘要，逐行输出「问题 -> 判定 (置信度)」。"""
    answers = result.get("answers", {})
    usage = result.get("usage", {})
    lines = []
    for qid, ans in answers.items():
        atype = ans.get("type")
        if atype == "noul":
            # noul 返回 P(yes)，四舍五入展示
            lines.append(f"  {qid}: P(yes)={ans.get('noul')}")
        elif atype == "choice":
            conf = ans.get("confidence")
            lines.append(f"  {qid}: {ans.get('choice')} (置信度={conf})")
        elif atype == "score":
            conf = ans.get("confidence")
            legend = ans.get("legend", {})
            idx = ans.get("score")
            # 把加权索引就近映射回等级名
            name = _score_name(idx, legend)
            lines.append(f"  {qid}: {name}(idx={idx}, 置信度={conf})")
    usage_line = f"输入token={usage.get('input_tokens')}, 延迟={result.get('latency_ms')}ms"
    return "\n".join(lines), usage_line


def _score_name(idx, legend):
    """legend 形如 {"0":"低","1":"中","2":"高"}，按索引就近取等级名。"""
    if not legend:
        return str(idx)
    keys = sorted(legend.keys(), key=lambda k: float(k))
    best = min(keys, key=lambda k: abs(float(k) - float(idx)))
    return legend[best]


def die(msg, code):
    """统一错误出口：打印到 stderr 并以指定退出码结束。"""
    print(f"[systemone-decision 错误] {msg}", file=sys.stderr)
    sys.exit(code)


def main():
    global args_timeout
    p = argparse.ArgumentParser(description="SystemOne 决策模型方案裁决")
    p.add_argument("--state", required=True, help="待裁决的业务上下文/方案描述")
    p.add_argument("--question", default="请裁决", help="问题的统一说明")
    p.add_argument("--id", default=None, help="choice 问题的问题 id（默认 choice）")
    p.add_argument("--choice", action="append", help='选项问题，格式 选项id=描述；可重复。例：--choice plan="改用X方案"')
    p.add_argument("--noul", action="append", help="是非问题 id，可重复")
    p.add_argument("--score", action="append", help='评分问题，格式 等级id=低,中,高，可重复')
    p.add_argument("--model", default=DEFAULT_MODEL, help="模型编码")
    p.add_argument("--base-url", default=os.environ.get("SYSTEMONE_BASE_URL", DEFAULT_BASE), help="端点基址")
    p.add_argument("--api-key", default=os.environ.get("DASHSCOPE_API_KEY") or os.environ.get("SYSTEMONE_API_KEY"))
    p.add_argument("--workspace", default=os.environ.get("SYSTEMONE_WORKSPACE", ""), help="workspace id，拼进 URL 时用")
    p.add_argument("--timeout", type=int, default=60, help="请求超时秒数")
    p.add_argument("--raw", action="store_true", help="额外打印原始 JSON")
    p.add_argument("--exempt", action="store_true", help="豁免登记：不调 API，把 --state 作为豁免理由写回执（仅限用户已拍板/单一明确方案）")
    args = p.parse_args()
    args_timeout = args.timeout

    # 豁免通道：用户已拍板或方案唯一时，登记理由即可过闸，留审计痕迹
    if args.exempt:
        write_receipt("exempt", {"reason": str(args.state)[:500]})
        print(f"[systemone-decision] 豁免已登记：{str(args.state)[:80]}")
        return

    if not args.api_key:
        die("缺少 API Key：用 --api-key 或设置 DASHSCOPE_API_KEY / SYSTEMONE_API_KEY", 2)

    # 若给了 workspace 且基址是占位形式，替换 {workspace}
    base_url = args.base_url
    if "{workspace}" in base_url:
        base_url = base_url.replace("{workspace}", args.workspace or "")

    payload = build_payload(args)
    result = call_systemone(payload, base_url, args.api_key)

    if result.get("code") or result.get("error"):
        die(f"服务端返回错误：{result}", 3)

    # 裁决成功落回执，30 分钟内的代码编辑闸凭此放行
    write_receipt("decision", {"request_id": result.get("request_id", ""), "state": str(args.state)[:500]})

    summary, usage_line = summarize(result)
    print("=== SystemOne 裁决 ===")
    print(f"state: {str(args.state)[:80]}")
    print(summary)
    print(usage_line)
    if args.raw:
        print("--- raw ---")
        print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
