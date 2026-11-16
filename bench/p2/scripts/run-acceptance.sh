#!/usr/bin/env bash
# 场景：阶段验收的确定性检查路径（DFX-P-06）。
#
#   P95 ≤ 10s（程序检查）/ ≤ 60s（含模型判断）
#
# 跑两遍：一遍含 build_pass（起真实 go build 子进程），一遍只含进程内判据。
# 两遍的差值就是「验收编排」与「工具链子进程」的归因界限——否则 P95 超标时
# 分不清该优化验收实现还是优化判据本身。
#
# 用法：
#   bash bench/p2/scripts/run-acceptance.sh [-n 20]
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

log "acceptance (full, with build_pass): $N iterations"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/acceptbench -n "$N" \
    -out "$RUNTIME_DIR/acceptance.json" )

log "acceptance (light, in-process only): $N iterations"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/acceptbench -n "$N" -light \
    -out "$RUNTIME_DIR/acceptance-light.json" )
