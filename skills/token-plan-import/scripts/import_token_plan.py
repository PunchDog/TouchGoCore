#!/usr/bin/env python3
"""Import qianwenai Token Plan models into OpenAI-compatible clients.

Targets:
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


def resolve_api_key(cli_value: str | None, existing: Iterable[dict], base_url: str) -> str:
    if cli_value:
        return cli_value.strip()
    env_value = os.environ.get("TOKEN_PLAN_API_KEY")
    if env_value:
        return env_value.strip()
    for entry in existing:
        if entry.get("url") == base_url and entry.get("apiKey"):
            return entry["apiKey"]
    raise SystemExit(
        "[error] API key not found. Pass --api-key, set TOKEN_PLAN_API_KEY, "
        "or ensure an existing token-plan entry in ~/.workbuddy/models.json."
    )


def build_workbuddy_entry(model_id: str, base_url: str, api_key: str, vision: bool) -> dict:
    return {
        "id": model_id,
        "name": model_id,
        "vendor": "Custom",
        "url": base_url,
        "apiKey": api_key,
        "supportsToolCall": True,
        "supportsImages": vision,
        "supportsReasoning": True,
        "useCustomProtocol": False,
    }


def build_qoder_entry(model_id: str, base_url: str, api_key: str, vision: bool) -> dict:
    return {
        "name": model_id,
        "displayName": model_id,
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


def build_generic_entry(model_id: str, base_url: str, api_key: str) -> dict:
    return {
        "id": model_id,
        "name": model_id,
        "baseUrl": base_url,
        "apiKey": api_key,
    }


def write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Batch-import qianwenai Token Plan models into OpenAI-compatible clients."
    )
    parser.add_argument(
        "--target",
        choices=["workbuddy", "qoder", "generic"],
        default="workbuddy",
        help="Destination format (default: workbuddy).",
    )
    parser.add_argument(
        "--models",
        default=",".join(DEFAULT_MODELS),
        help="Comma-separated model ids to import.",
    )
    parser.add_argument("--api-key", default=None, help="Token Plan API key (else TOKEN_PLAN_API_KEY or reuse existing).")
    parser.add_argument("--base-url", default=DEFAULT_BASE_URL, help="OpenAI-compatible base URL.")
    parser.add_argument(
        "--vision-models",
        default=",".join(sorted(DEFAULT_VISION)),
        help="Comma-separated model ids that support image input.",
    )
    parser.add_argument("--out", default=None, help="Output file path (qoder/generic). Defaults to stdout.")
    parser.add_argument("--workbuddy-path", default=str(WORKBUDDY_MODELS_PATH), help="Override models.json path.")
    parser.add_argument("--overwrite", action="store_true", help="Overwrite existing WorkBuddy entries with same id.")
    parser.add_argument("--dry-run", action="store_true", help="Print actions without writing files.")
    args = parser.parse_args(argv)

    model_ids = [m.strip() for m in args.models.split(",") if m.strip()]
    if not model_ids:
        raise SystemExit("[error] --models produced an empty list")
    vision_set = {m.strip() for m in args.vision_models.split(",") if m.strip()}

    wb_path = Path(args.workbuddy_path).expanduser()
    existing = load_workbuddy_models(wb_path) if args.target == "workbuddy" else []
    api_key = resolve_api_key(args.api_key, existing, args.base_url)

    if args.target == "workbuddy":
        existing_ids = {e.get("id") for e in existing}
        to_add, to_skip, to_overwrite = [], [], []
        for mid in model_ids:
            if mid in existing_ids and not args.overwrite:
                to_skip.append(mid)
            elif mid in existing_ids and args.overwrite:
                to_overwrite.append(mid)
                existing[:] = [e for e in existing if e.get("id") != mid]
                existing.append(build_workbuddy_entry(mid, args.base_url, api_key, mid in vision_set))
            else:
                to_add.append(mid)
                existing.append(build_workbuddy_entry(mid, args.base_url, api_key, mid in vision_set))

        print(f"target=workbuddy path={wb_path}")
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
        payload = [build_qoder_entry(m, args.base_url, api_key, m in vision_set) for m in model_ids]
    else:  # generic
        payload = [build_generic_entry(m, args.base_url, api_key) for m in model_ids]

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