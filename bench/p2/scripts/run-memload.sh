#!/usr/bin/env bash
# 场景：20 并发任务的常驻内存（DFX-P-09）。
#
#   RSS ≤ 4GB；单任务增量 ≤ 256MB
#
# 口径说明：负载是 scripts/fakeagent（不接真实 LLM），因此只覆盖
# 「20 个任务状态机同时在跑」的框架开销，不含真实模型上下文。
# 报告里必须带上这句，否则数字会被误读成生产容量。
#
# 用法：
#   bash bench/p2/scripts/run-memload.sh [-n 20]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-service.sh
source "$SCRIPT_DIR/lib-service.sh"

N=20
while [ $# -gt 0 ]; do
  case "$1" in
    -n) N="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

build_binaries
ensure_fakeagent
start_harnessloop
trap 'stop_harnessloop' EXIT

PID="$(service_pid)"
if [ -z "$PID" ]; then
  log "cannot resolve harnessloop-service pid"
  exit 1
fi

log "measuring $N concurrent tasks against pid $PID"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/memload \
    -n "$N" \
    -addr "$HLP_ADDR" \
    -pid "$PID" \
    -out "$RUNTIME_DIR/memload-${N}.json" )
