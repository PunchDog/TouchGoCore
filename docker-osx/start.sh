#!/usr/bin/env bash
# Docker-OSX 启动脚本
#
#   ./start.sh            关闭已有实例 -> docker compose up --build -d（后台，日常使用）
#   ./start.sh -m init    关闭已有实例 -> docker compose up --build（前台，首次安装）
#
# 首次安装必须用 -m init：前台能看到下载进度，QEMU 起来后再脱离。
# 版本菜单不用管——容器内脚本会自动选最后一项（最新 macOS，当前是 Tahoe），无需输入。
# 脱离用 Ctrl-P Ctrl-Q，不要按 Ctrl-C。
set -euo pipefail

cd "$(dirname "$0")"

usage() {
    sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
}

MODE="detach"
while [[ $# -gt 0 ]]; do
    case "$1" in
        -m|--mode)
            MODE="${2:-}"
            shift 2 || { echo "错误: $1 需要一个参数" >&2; exit 1; }
            ;;
        -m=*|--mode=*)
            MODE="${1#*=}"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "错误: 未知参数 '$1'（可用: -m init | -h）" >&2
            exit 1
            ;;
    esac
done

case "$MODE" in
    init|detach) ;;
    *)
        echo "错误: 不支持的模式 '$MODE'，只支持 init（前台首次安装）" >&2
        exit 1
        ;;
esac

if ! command -v docker >/dev/null 2>&1; then
    echo "错误: 未找到 docker 命令" >&2
    exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
    echo "错误: 未找到 docker compose（需要 Compose v2，即 'docker compose' 而非 'docker-compose'）" >&2
    exit 1
fi

# CPU 配额默认真机一半：环境变量或 .env 里没写 CPU_LIMIT 时自动按 nproc/2 计算。
# 容器配额与 guest vCPU（SMP=half）对齐，避免 vCPU 数超过可用配额。
if [[ -z "${CPU_LIMIT:-}" ]] && ! grep -qE '^[[:space:]]*CPU_LIMIT[[:space:]]*=' .env 2>/dev/null; then
    HOST_NPROC="$(nproc 2>/dev/null || echo 4)"
    [[ "${HOST_NPROC}" =~ ^[0-9]+$ ]] || HOST_NPROC=4
    CPU_LIMIT=$(( HOST_NPROC / 2 ))
    if (( CPU_LIMIT < 1 )); then CPU_LIMIT=1; fi
    export CPU_LIMIT
    echo "==> CPU 配额: 真机 ${HOST_NPROC} 线程 -> ${CPU_LIMIT} 核（一半）"
else
    echo "==> CPU 配额: ${CPU_LIMIT:-由 .env 指定}"
fi

# 已有实例（运行中或已退出）先 down 掉，避免端口占用/状态残留
if docker compose ps -aq 2>/dev/null | grep -q .; then
    echo "==> 关闭已有实例: docker compose down"
    docker compose down
else
    echo "==> 没有已存在的实例，跳过 down"
fi

case "$MODE" in
    init)
        echo "==> 前台启动（首次安装）: docker compose up --build"
        echo "    版本菜单无需输入：脚本自动选最后一项（最新 macOS）"
        echo "    QEMU 起来后按 Ctrl-P Ctrl-Q 脱离，不要按 Ctrl-C"
        exec docker compose up --build
        ;;
    detach)
        echo "==> 后台启动: docker compose up --build -d"
        docker compose up --build -d
        echo "==> VNC: 127.0.0.1:5999（安装器下载 + 转换完成前不会监听）"
        echo "    进度: docker compose logs -f"
        ;;
esac
