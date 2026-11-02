#!/usr/bin/env bash
# zalo/probe/run.sh
# Zalo 卡片真机探针（Linux / macOS / Git Bash）。
#
# 用法：
#   bash zalo/probe/run.sh
#   bash zalo/probe/run.sh --group
#   bash zalo/probe/run.sh --dry-run                 # 只校验配置，不发送
#   bash zalo/probe/run.sh path/to/probe.env --group
#
# 安全：只解析 KEY=VALUE，不 source 环境文件；DSN 不会打印。
# 警告：正常模式会真实发送一条消息。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE=""
GROUP="${ZALO_PROBE_GROUP:-0}"
DRY_RUN=0

for arg in "$@"; do
  case "$arg" in
    --group) GROUP=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -*) echo "未知参数: $arg" >&2; exit 2 ;;
    *) ENV_FILE="$arg" ;;
  esac
done
ENV_FILE="${ENV_FILE:-$ROOT/zalo/probe/probe.env}"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "缺少环境文件：$ENV_FILE" >&2
  echo "先复制模板：cp zalo/probe/env.example zalo/probe/probe.env" >&2
  exit 1
fi

# 只解析 KEY=VALUE；忽略空行与 # 注释；去掉成对引号；不执行文件内容。
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line%$'\r'}"
  [[ "$line" =~ ^[[:space:]]*$ ]] && continue
  [[ "$line" =~ ^[[:space:]]*# ]] && continue
  key="${line%%=*}"
  [[ "$key" == "$line" ]] && continue
  val="${line#*=}"
  key="$(printf '%s' "$key" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  val="$(printf '%s' "$val" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
  case "$val" in
    \"*\") val="${val:1:${#val}-2}" ;;
    \'*\') val="${val:1:${#val}-2}" ;;
  esac
  export "$key=$val"
done <"$ENV_FILE"

required=(TEST_ZALO_PROBE_DSN TEST_ZALO_PROBE_ACCOUNT TEST_ZALO_PROBE_TO TEST_ZALO_PROBE_LINK)
missing=()
for k in "${required[@]}"; do
  [[ -z "${!k:-}" ]] && missing+=("$k")
done
if (( ${#missing[@]} > 0 )); then
  echo "缺少必填变量: ${missing[*]}" >&2
  exit 1
fi
[[ "$GROUP" == "1" ]] && export TEST_ZALO_PROBE_GROUP=1

echo "=== Zalo 卡片真机探针 ==="
echo "账号: ${TEST_ZALO_PROBE_ACCOUNT}  目标: ${TEST_ZALO_PROBE_TO}  群聊: ${TEST_ZALO_PROBE_GROUP:-0}"
echo "外链: ${TEST_ZALO_PROBE_LINK}"
echo "注意：会真实发送一条消息。"

if [[ "$DRY_RUN" == "1" ]]; then
  echo "DryRun：配置校验通过，未发送。"
  exit 0
fi

cd "$ROOT"
go test -count=1 -v -run TestZACardProbeRealDevice ./zalo
