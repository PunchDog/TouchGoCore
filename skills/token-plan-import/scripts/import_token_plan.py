#!/usr/bin/env python3
"""Import OpenAI-compatible models into WorkBuddy / Qoder / generic clients.

Sources (model set + endpoint):
  - token-plan  : qianwenai Token Plan models (default)
  - opencode-go : 32 OpenCode Go models at https://opencode.ai/inference/go/v1

Targets (output format):
  - workbuddy : append to ~/.workbuddy/models.json
  - qoder     : emit JSON array matching Qoder custom-model schema (paste into UI)
  - generic   : emit minimal OpenAI-compatible entries

Zero third-party dependencies (Python 3.8+).
"""
from __future__ import annotations

import argparse
import json
import os
import sys
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

# OpenCode Go — canonical IDs from https://opencode.ai/inference/go/v1/models.
# Left = display name from the Go model table, right = API model id (sent as the model id to the endpoint).
OPENCODE_GO_BASE_URL = "https://opencode.ai/inference/go/v1"
OPENCODE_GO_MODELS: list[tuple[str, str]] = [
    ("Grok 4.7", "grok-4.7"),
    ("GPT 6 Luna", "gpt-6-luna"),
    ("GPT 5.6 Luna", "gpt-5.6-luna"),
    ("Claude Haiku 5.5", "claude-haiku-5-5"),
    ("Gemini 4 Argon", "gemini-4-argon"),
    ("GLM-5.3-Flash", "glm-5.3-flash"),
    ("GLM-5.3", "glm-5.3"),
    ("GLM-5.2", "glm-5.2"),
    ("Kimi K3.1 Pro", "kimi-k3.1-pro"),
    ("Kimi K3", "kimi-k3"),
    ("Kimi K2.7 Code", "kimi-k2.7-code"),
    ("LongCat-2.0", "longcat-2.0"),
    ("MiMo-V2.6-Pro", "mimo-v2.6-pro"),
    ("MiMo-V2.6-Flash", "mimo-v2.6-flash"),
    ("Qwen4 Max", "qwen4-max"),
    ("Qwen3.8 Max", "qwen3.8-max"),
    ("Qwen3.8 Flash", "qwen3.8-flash"),
    ("Qwen3.7 Plus", "qwen3.7-plus"),
    ("MiniMax M3.1 Flash", "minimax-m3.1-flash"),
    ("MiniMax M3", "minimax-m3"),
    ("Muse Spark 1.3 Contributor", "muse-spark-1.3-contributor"),
    ("Muse Spark 1.2 Contributor", "muse-spark-1.2-contributor"),
    ("DeepSeek V4.1 Flash", "deepseek-v4.1-flash"),
    ("DeepSeek V4 Pro", "deepseek-v4-pro"),
    ("Hy4", "hy4"),
    ("Hy4 preview", "hy4-preview"),
    ("Hy3", "hy3"),
    ("Step 5 Preview", "step-5-preview"),
    ("Beam Free", "beam-free"),
    ("Iceman Contributor Free", "iceman-contributor-free"),
    ("LongCat 2.5 Free", "longcat-2.5-free"),
    ("Hy4 flash Free", "hy4-flash-free"),
]

# IDs whose /v1/models metadata lists an image input modality.
OPENCODE_GO_VISION = {
    "grok-4.7", "gpt-6-luna", "gpt-5.6-luna", "claude-haiku-5-5", "gemini-4-argon",
    "glm-5.3-flash", "kimi-k3.1-pro", "kimi-k3", "kimi-k2.7-code",
    "mimo-v2.6-pro", "mimo-v2.6-flash", "qwen4-max", "qwen3.8-max", "qwen3.8-flash",
    "qwen3.7-plus", "minimax-m3.1-flash", "minimax-m3",
    "muse-spark-1.3-contributor", "muse-spark-1.2-contributor", "deepseek-v4.1-flash",
    "hy4", "step-5-preview", "longcat-2.5-free", "hy4-flash-free",
}


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


def resolve_api_key(cli_value: str | None, existing: Iterable[dict], base_url: str, env_names: tuple[str, ...] = ()) -> str:
    if cli_value:
        return cli_value.strip()
    for name in (*env_names, CREDENTIAL_ENV, "TOKEN_PLAN_API_KEY"):
        env_value = os.environ.get(name)
        if env_value:
            return env_value.strip()
    for entry in existing:
        if entry.get("url") == base_url and entry.get("apiKey"):
            return entry["apiKey"]
    raise SystemExit(
        f"[error] API key not found. Pass --api-key, set {CREDENTIAL_ENV} "
        "(or TOKEN_PLAN_API_KEY), or ensure an existing entry in ~/.workbuddy/models.json."
    )


def build_workbuddy_entry(model_id: str, display_name: str, base_url: str, api_key: str, vision: bool) -> dict:
    return {
        "id": model_id,
        "name": display_name,
        "vendor": "Custom",
        "url": base_url,
        "apiKey": api_key,
        "supportsToolCall": True,
        "supportsImages": vision,
        "supportsReasoning": True,
        "useCustomProtocol": False,
    }


def build_qoder_entry(model_id: str, display_name: str, base_url: str, api_key: str, vision: bool) -> dict:
    return {
        "name": model_id,
        "displayName": display_name,
        "format": "openai",
        "source": "custom",
        "baseUrl": base_url,
        "apiKey": api_key,
        "multiModalSupported": vision,
        "reasoningSupported": True,
        "maxInputTokens": 200000,
        "enabled": True,
        "isDefault": False,
    }


def build_generic_entry(model_id: str, display_name: str, base_url: str, api_key: str) -> dict:
    return {
        "id": model_id,
        "name": display_name,
        "baseUrl": base_url,
        "apiKey": api_key,
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
        choices=["workbuddy", "qoder", "generic"],
        default="workbuddy",
        help="Destination format (default: workbuddy).",
    )
    parser.add_argument(
        "--source",
        choices=["token-plan", "opencode-go"],
        default="token-plan",
        help="Model set/endpoint to import (default: token-plan). opencode-go = 32 Go models.",
    )
    parser.add_argument(
        "--models",
        default=None,
        help="Comma-separated model ids to import. Defaults per source (opencode-go = 32 Go IDs).",
    )
    parser.add_argument("--api-key", default=None, help=f"API key (else {CREDENTIAL_ENV} or reuse existing).")
    parser.add_argument("--base-url", default=None, help="OpenAI-compatible base URL (default per source).")
    parser.add_argument(
        "--vision-models",
        default=None,
        help="Comma-separated model ids that support image input (default per source).",
    )
    parser.add_argument("--out", default=None, help="Output file path (qoder/generic). Defaults to stdout.")
    parser.add_argument("--workbuddy-path", default=str(WORKBUDDY_MODELS_PATH), help="Override models.json path.")
    parser.add_argument("--overwrite", action="store_true", help="Overwrite existing WorkBuddy entries with same id.")
    parser.add_argument("--dry-run", action="store_true", help="Print actions without writing files.")
    args = parser.parse_args(argv)

    is_go = args.source == "opencode-go"
    base_url = args.base_url or (OPENCODE_GO_BASE_URL if is_go else DEFAULT_BASE_URL)
    vision_set = (
        set(OPENCODE_GO_VISION if is_go else DEFAULT_VISION)
        if args.vision_models is None
        else {m.strip() for m in args.vision_models.split(",") if m.strip()}
    )

    if args.models is None:
        # (display_name, model_id) pairs.
        models = list(OPENCODE_GO_MODELS) if is_go else [(m, m) for m in DEFAULT_MODELS]
    else:
        display_by_id = {mid: display for display, mid in OPENCODE_GO_MODELS} if is_go else {}
        requested = [m.strip() for m in args.models.split(",") if m.strip()]
        models = [(display_by_id.get(m, m), m) for m in requested]
    if not models:
        raise SystemExit("[error] --models produced an empty list")

    wb_path = Path(args.workbuddy_path).expanduser()
    existing = load_workbuddy_models(wb_path) if args.target == "workbuddy" else []
    api_key = resolve_api_key(args.api_key, existing, base_url)

    if args.target == "workbuddy":
        print(f"target=workbuddy source={args.source} base_url={base_url} path={wb_path}")
        print(f"  models={[mid for _, mid in models]}")
        existing_ids = {e.get("id") for e in existing}
        to_add, to_skip, to_overwrite = [], [], []
        for display, mid in models:
            entry = build_workbuddy_entry(mid, display, base_url, api_key, mid in vision_set)
            if mid in existing_ids and not args.overwrite:
                to_skip.append(mid)
            elif mid in existing_ids and args.overwrite:
                to_overwrite.append(mid)
                existing[:] = [e for e in existing if e.get("id") != mid]
                existing.append(entry)
            else:
                to_add.append(mid)
                existing.append(entry)

        print(f"  add={to_add}")
        print(f"  overwrite={to_overwrite}")
        print(f"  skip(existing)={to_skip}")
        if args.dry_run:
            print("dry-run: no changes written")
            return 0
        if not (to_add or to_overwrite):
            print("nothing to change")
            return 0
        backup = wb_path.with_suffix(wb_path.suffix + ".bak")
        if wb_path.exists():
            backup.write_bytes(wb_path.read_bytes())
            print(f"backup -> {backup}")
        write_text(wb_path, json.dumps(existing, ensure_ascii=False, indent=2) + "\n")
        print(f"wrote {wb_path} ({len(existing)} entries)")
        return 0

    if args.target == "qoder":
        payload = [build_qoder_entry(mid, display, base_url, api_key, mid in vision_set) for display, mid in models]
    else:  # generic
        payload = [build_generic_entry(mid, display, base_url, api_key) for display, mid in models]

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