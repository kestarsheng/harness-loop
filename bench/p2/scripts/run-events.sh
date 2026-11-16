#!/usr/bin/env bash
# 场景：事件从产生到出现在大屏的端到端延迟（DFX-P-07）。
#
#   P95 ≤ 1s（局域网）
#
# 链路：harnessloop-service 写 hlp_event.events
#        → gateway WatchEvents(gRPC) → MonitorStream(SSE) → 本程序
#
# 必须带 -purge：网关 SSE 在建立连接时会先 catch-up 回放历史事件，把 since
# 推到最后一条的游标值。清空本租户事件后 since 停在 0，测的才是纯推送延迟
# 与「本轮事件收集率」，不会把历史回放算进分子/分母。
#
# 事件游标在 T-P2-17 之后是租户级 global_seq（hlp_event.events.global_seq），
# 新任务的事件不再被 since 过滤；本脚本同时校验 seq 单调不跳号。
#
# 用法：
#   bash bench/p2/scripts/run-events.sh [-n 5]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-service.sh
source "$SCRIPT_DIR/lib-service.sh"

N=5
while [ $# -gt 0 ]; do
  case "$1" in
    -n) N="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

: "${HARNESS_TEST_PG_DSN:?请先 export HARNESS_TEST_PG_DSN（-purge 需要直连压测库）}"

build_binaries
ensure_fakeagent
start_harnessloop
start_gateway
trap 'stop_gateway; stop_harnessloop' EXIT

log "measuring event e2e with $N source tasks"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/events \
    -tasks "$N" \
    -purge \
    -dsn "$HARNESS_TEST_PG_DSN" \
    -base "http://${GW_ADDR:-127.0.0.1:9000}" \
    -addr "$HLP_ADDR" \
    -out "$RUNTIME_DIR/events-$N.json" )
