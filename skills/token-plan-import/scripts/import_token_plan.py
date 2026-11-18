#!/usr/bin/env python3
"""Import OpenAI-compatible models into WorkBuddy / Qoder / generic clients.

Sources (model set + endpoint):
  - token-plan  : qianwenai Token Plan models (default)
  - opencode-go : models fetched live from <base-url>/models (default http://127.0.0.1:7863/opencode/v1)

Targets:
  - auto      : detect the hosting client (Qoder env markers / script path) and import into it
  - workbuddy : replace same-source entries in ~/.workbuddy/models.json
  - qoder     : write providers into Qoder settings.json (--out switches to snippet export)
  - generic   : emit minimal OpenAI-compatible entries

Display names are prefixed by source ("token plan:" / "opencode:"); model ids stay canonical.
Old same-source entries (same endpoint, or legacy prefixed names) are purged before writing.

Zero third-party dependencies (Python 3.8+).
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.request
import uuid
from pathlib import Path
from typing import Iterable

DEFAULT_BASE_URL = "https://token-plan.maas.qianwenaiapi.com/compatible-mode/v1"
DEFAULT_MODELS = [
    "glm-5.3",
    "deepseek-v4-pro",
    "deepseek-v4.1-flash-1017",
    "qwen3.8-flash",
    "qwen4-max",
    "auto",
]
DEFAULT_VISION = {"qwen4-max", "glm-5.3"}
WORKBUDDY_MODELS_PATH = Path.home() / ".workbuddy" / "models.json"
CREDENTIAL_ENV = "OPEN_CODE_TOKEN_PLAN_API_KEY"
OPENCODE_GO_BASE_URL = "http://127.0.0.1:7863/opencode/v1"
# 来源 → 显示名前缀：token-plan 对应千问（token plan），opencode-go 对应 opencode
SOURCE_PREFIX = {"token-plan": "token plan", "opencode-go": "opencode"}

# WorkBuddy built-in catalog uses reasoning.defaultEffort + onlyReasoning to enable high-effort thinking.
# Keep both `effort` and `defaultEffort` so either client spelling is honored.
REASONING_HIGH = {
    "effort": "high",
    "defaultEffort": "high",
    "supportedEfforts": ["low", "high"],
    "summary": "auto",
    "canDisableThinking": False,
}


def read_env(name: str) -> str | None:
    """Read a user env var. setx may not have propagated into this process tree,
    so fall back to the Windows user-level registry value."""
    value = os.environ.get(name)
    if value and value.strip():
        return value.strip()
    if sys.platform == "win32":
        try:
            import winreg
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, "Environment") as key:
                value, _ = winreg.QueryValueEx(key, name)
            if value and str(value).strip():
                return str(value).strip()
        except (OSError, ImportError):
            pass
    return None


def detect_target() -> str:
    """自动识别当前宿主客户端：环境变量标识优先，其次脚本所在安装目录。"""
    # Qoder 会话注入 QODER_CONFIG_DIR / QODER_PRODUCT_ID，是最强宿主信号
    if os.environ.get("QODER_CONFIG_DIR") or os.environ.get("QODER_PRODUCT_ID"):
        return "qoder"
    # WorkBuddy 会话注入 WORKBUDDY_ 前缀变量（与 QODERCN_ 同类的品牌前缀机制）
    if any(k.startswith("WORKBUDDY_") for k in os.environ):
        return "workbuddy"
    # 兜底：skill 被哪个客户端安装就落在谁的目录下（~/.qoder-cn/skills 或 ~/.workbuddy/skills）
    parts = {p.lower() for p in Path(__file__).resolve().parts}
    if ".workbuddy" in parts:
        return "workbuddy"
    if any(p.startswith(".qoder") for p in parts):
        return "qoder"
    raise SystemExit("[error] cannot detect host client; pass --target workbuddy|qoder|generic explicitly")


def qoder_settings_path() -> Path:
    """Qoder settings.json 路径：优先会话注入的 QODER_CONFIG_DIR，否则默认 ~/.qoder-cn。"""
    cfg = os.environ.get("QODER_CONFIG_DIR")
    return (Path(cfg) if cfg else Path.home() / ".qoder-cn") / "settings.json"


def prefixed_name(source: str, name: str) -> str:
    """统一显示名前缀：先剥掉网关可能自带的任一来源前缀，再加当前来源前缀，保证结果确定。"""
    for p in SOURCE_PREFIX.values():
        if name.startswith(p + ":"):
            name = name[len(p) + 1:]
            break
    return f"{SOURCE_PREFIX[source]}:{name}"


def is_legacy_name(source: str, name: str) -> bool:
    """判断显示名是否带当前来源前缀（清理换端点后的同源历史残留，不误删另一来源）。"""
    return str(name).startswith(SOURCE_PREFIX[source] + ":")


def load_workbuddy_models(path: Path) -> list:
    if not path.exists():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        raise SystemExit(f"[error] {path} is not valid JSON: {e}")
    if not isinstance(data, list):
        raise SystemExit(f"[error] {path} must be a JSON array at the top level")
    return data


def load_qoder_settings(path: Path) -> dict:
    """读取 Qoder settings.json；不存在时返回空 dict（首次导入场景）。"""
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        raise SystemExit(f"[error] {path} is not valid JSON: {e}")
    if not isinstance(data, dict):
        raise SystemExit(f"[error] {path} must be a JSON object at the top level")
    return data


def resolve_api_key(cli_value: str | None, existing: Iterable[dict], base_url: str, strict: bool = False) -> str:
    if cli_value:
        return cli_value.strip()
    if strict:
        # opencode-go: only OPEN_CODE_TOKEN_PLAN_API_KEY, no fallback.
        value = read_env(CREDENTIAL_ENV)
        if value:
            return value
        raise SystemExit(
            f"[error] API key not found. Pass --api-key or set user env var {CREDENTIAL_ENV}. "
            "Fallback to old env name / existing entries is disabled for opencode-go."
        )
    # token-plan: original TOKEN_PLAN_API_KEY behavior.
    value = read_env("TOKEN_PLAN_API_KEY")
    if value:
        return value
    for entry in existing:
        if entry.get("url") == base_url and entry.get("apiKey"):
            return entry["apiKey"]
    raise SystemExit(
        "[error] API key not found. Pass --api-key, set TOKEN_PLAN_API_KEY, "
        "or ensure an existing token-plan entry in ~/.workbuddy/models.json."
    )


def fetch_models(base_url: str) -> list[dict]:
    """Fetch the live model catalog from <base-url>/models."""
    url = base_url.rstrip("/") + "/models"
    req = urllib.request.Request(
        url,
        headers={"User-Agent": "token-plan-import/1.0", "Accept": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        raise SystemExit(f"[error] failed to fetch {url}: {e}")
    if not isinstance(payload, dict) or not isinstance(payload.get("data"), list):
        raise SystemExit(f"[error] unexpected /models response from {url}")
    models = []
    for item in payload["data"]:
        mid = item.get("id") if isinstance(item, dict) else None
        if not mid:
            continue
        inputs = (item.get("modalities") or {}).get("input") or []
        vision = any(str(m).startswith("image") for m in inputs)
        models.append({"id": mid, "name": item.get("name") or mid, "vision": vision})
    if not models:
        raise SystemExit(f"[error] no models returned from {url}")
    return models


def build_workbuddy_entry(model: dict, base_url: str, api_key: str, vision: bool, name_label: str) -> dict:
    return {
        "id": model["id"],            # 规范模型 ID，不带前缀
        "name": name_label,           # WorkBuddy 的 name 字段即前缀标签（opencode / token plan），客户端自行拼接显示
        "vendor": "Custom",
        "url": base_url,
        "apiKey": api_key,
        "supportsToolCall": True,
        "supportsImages": vision,
        "supportsReasoning": True,
        "onlyReasoning": True,
        "reasoning": dict(REASONING_HIGH),
        "useCustomProtocol": False,
    }


def build_qoder_entry(model: dict, base_url: str, api_key: str, vision: bool, display: str) -> dict:
    return {
        "name": model["id"],          # 规范模型 ID，不带前缀
        "displayName": display,       # 显示名带来源前缀
        "format": "openai",
        "source": "custom",
        "baseUrl": base_url,
        "apiKey": api_key,
        "multiModalSupported": vision,
        "reasoningSupported": True,
        "reasoningEnabled": True,
        "reasoningEffort": "high",
        "reasoning": dict(REASONING_HIGH),
        "maxInputTokens": 200000,
        "enabled": True,
        "isDefault": False,
    }


def build_generic_entry(model: dict, base_url: str, api_key: str, display: str) -> dict:
    return {
        "id": model["id"],
        "name": display,
        "baseUrl": base_url,
        "apiKey": api_key,
    }


def build_qoder_provider(models: list[dict], base_url: str, api_key: str, source: str) -> dict:
    """按 Qoder settings.json providers 段实测 schema 组装 openai-compatible provider。"""
    return {
        "apiKey": api_key,                # 网关凭据，Qoder 需要实际 Key
        "authType": "bearer",             # OpenAI 兼容 Bearer 鉴权
        "baseUrl": base_url,              # 模型端点
        "model": models[0]["id"],         # provider 默认模型取清单第一个
        "models": [
            {
                "capabilities": {
                    "thinking": {         # 显式高思考，与 WorkBuddy 口径一致
                        "modes": ["enabled"],
                        "supportedEffortLevels": ["low", "high"],
                        "supportsEffort": True,
                    },
                    "vision": m["vision"],            # 图像能力来自 /models modalities
                },
                "contextWindow": 200000,              # 输入上下文窗口
                "displayName": prefixed_name(source, m["name"]),  # 显示名带来源前缀
                "maxOutputTokens": 8192,              # 输出上限沿用实测条目口径
                "model": m["id"],                     # 规范模型 ID
            }
            for m in models
        ],
        "protocol": "openai",             # OpenAI 兼容协议
        "type": "openai-compatible",      # 自定义模型类型
    }


def write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Batch-import OpenAI-compatible models into WorkBuddy / Qoder / generic clients."
    )
    parser.add_argument(
        "--target",
        choices=["auto", "workbuddy", "qoder", "generic"],
        default="auto",
        help="Destination client (default: auto-detect Qoder/WorkBuddy from env or install path).",
    )
    parser.add_argument(
        "--source",
        choices=["token-plan", "opencode-go"],
        default="token-plan",
        help="Model set/endpoint (default: token-plan). opencode-go fetches live /models.",
    )
    parser.add_argument("--models", default=None, help="Comma-separated model ids; default = all from source.")
    parser.add_argument("--api-key", default=None, help=f"API key (else {CREDENTIAL_ENV} only for opencode-go).")
    parser.add_argument("--base-url", default=None, help="OpenAI-compatible base URL (default per source).")
    parser.add_argument("--vision-models", default=None, help="Comma-separated ids that support image input.")
    parser.add_argument("--out", default=None, help="qoder/generic: export snippet to file instead of writing client config.")
    parser.add_argument("--workbuddy-path", default=str(WORKBUDDY_MODELS_PATH), help="Override models.json path.")
    parser.add_argument("--qoder-settings-path", default=None, help="Override Qoder settings.json path.")
    parser.add_argument("--keep-old", action="store_true", help="Keep existing same-source entries instead of removing them.")
    parser.add_argument("--dry-run", action="store_true", help="Print actions without writing files.")
    args = parser.parse_args(argv)

    # auto 目标先解析宿主，后续分支都走具体目标
    target = detect_target() if args.target == "auto" else args.target

    is_go = args.source == "opencode-go"
    base_url = args.base_url or (OPENCODE_GO_BASE_URL if is_go else DEFAULT_BASE_URL)
    vision_override = (
        {m.strip() for m in args.vision_models.split(",") if m.strip()}
        if args.vision_models is not None
        else None
    )

    if is_go:
        catalog = fetch_models(base_url)
        by_id = {m["id"]: m for m in catalog}
        if args.models is None:
            models = catalog
        else:
            requested = [m.strip() for m in args.models.split(",") if m.strip()]
            models = [by_id.get(mid, {"id": mid, "name": mid, "vision": False}) for mid in requested]
    else:
        ids = [m.strip() for m in args.models.split(",") if m.strip()] if args.models else DEFAULT_MODELS
        models = [{"id": mid, "name": mid, "vision": mid in DEFAULT_VISION} for mid in ids]

    if vision_override is not None:
        for model in models:
            model["vision"] = model["id"] in vision_override
    if not models:
        raise SystemExit("[error] --models produced an empty list")

    wb_path = Path(args.workbuddy_path).expanduser()
    qs_path = Path(args.qoder_settings_path).expanduser() if args.qoder_settings_path else qoder_settings_path()
    # 按目标加载既有配置：WorkBuddy 读 models.json，Qoder 直写模式读 settings.json
    existing = load_workbuddy_models(wb_path) if target == "workbuddy" else []
    settings = load_qoder_settings(qs_path) if target == "qoder" and not args.out else {}
    api_key = resolve_api_key(args.api_key, existing, base_url, strict=is_go)

    if target == "workbuddy":
        # 清理口径：同端点旧条目 + 同源前缀残留（name 等于前缀，或旧版 "前缀:显示名" 形态）；其他来源条目保留
        prefix = SOURCE_PREFIX[args.source]

        def stale(e: dict) -> bool:
            if e.get("url") == base_url:
                return True
            n = str(e.get("name", ""))
            return n == prefix or n.startswith(prefix + ":")

        old = [e for e in existing if stale(e)]
        kept = existing if args.keep_old else [e for e in existing if not stale(e)]
        for model in models:
            kept.append(build_workbuddy_entry(model, base_url, api_key, model["vision"], prefix))

        print(f"target=workbuddy source={args.source} base_url={base_url} path={wb_path}")
        print(f"  removed_old={len(old)} added={len(models)} total={len(kept)}")
        print(f"  models={[m['id'] for m in models]}")
        if args.dry_run:
            print("dry-run: no changes written")
            return 0
        backup = wb_path.with_suffix(wb_path.suffix + ".bak")
        if wb_path.exists():
            backup.write_bytes(wb_path.read_bytes())
            print(f"backup -> {backup}")
        write_text(wb_path, json.dumps(kept, ensure_ascii=False, indent=2) + "\n")
        print(f"wrote {wb_path} ({len(kept)} entries)")
        return 0

    if target == "qoder" and not args.out:
        # 直写 settings.json providers 段：先清理同端点/带前缀残留的旧 provider，再写入新 provider
        providers = settings.setdefault("providers", {})

        def stale_provider(p: dict) -> bool:
            if p.get("baseUrl") == base_url:
                return True
            return any(is_legacy_name(args.source, m.get("displayName", "")) for m in p.get("models", []))

        old_keys = [k for k, p in providers.items() if isinstance(p, dict) and stale_provider(p)]
        if not args.keep_old:
            for k in old_keys:
                del providers[k]
        pid = "qoder-custom-" + str(uuid.uuid4())  # 与 Qoder 自建条目同款 key 形态
        providers[pid] = build_qoder_provider(models, base_url, api_key, args.source)

        print(f"target=qoder source={args.source} base_url={base_url} path={qs_path}")
        print(f"  removed_old_providers={len(old_keys)} added_provider={pid} models={len(models)}")
        print(f"  models={[m['id'] for m in models]}")
        if args.dry_run:
            print("dry-run: no changes written")
            return 0
        if qs_path.exists():
            backup = qs_path.with_suffix(qs_path.suffix + ".bak")
            backup.write_bytes(qs_path.read_bytes())
            print(f"backup -> {backup}")
        write_text(qs_path, json.dumps(settings, ensure_ascii=False, indent=2) + "\n")
        print(f"wrote {qs_path} (provider {pid}, {len(models)} models)")
        print("note: restart Qoder to reload custom models")
        return 0

    # snippet 导出模式（qoder --out / generic）：只产 JSON 片段，不改客户端配置
    if target == "qoder":
        payload = [build_qoder_entry(m, base_url, api_key, m["vision"], prefixed_name(args.source, m["name"])) for m in models]
    else:  # generic
        payload = [build_generic_entry(m, base_url, api_key, prefixed_name(args.source, m["name"])) for m in models]

    content = json.dumps(payload, ensure_ascii=False, indent=2) + "\n"
    if args.out:
        out_path = Path(args.out).expanduser()
        if args.dry_run:
            print(f"dry-run: would write {len(payload)} entries to {out_path}")
            print(content)
        else:
            write_text(out_path, content)
            print(f"wrote {out_path} ({len(payload)} entries)")
    else:
        print(content)
    return 0


if __name__ == "__main__":
    sys.exit(main())