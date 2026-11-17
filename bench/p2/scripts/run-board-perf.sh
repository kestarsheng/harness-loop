#!/usr/bin/env bash
# 场景：50 卡片大屏的前端帧率（DFX-P-08）。
#
#   门槛：单帧 ≤ 16ms；内存无持续增长
#
# 分工说明：
#   - 数据面（50 VU × 1s 轮询）由 bench/p2/k6/monitor.js 覆盖；
#   - 本脚本覆盖渲染面：真 Chrome 打开看板，页面内 rAF 自采样帧间隔，
#     再点筛选/滚列表量交互延迟。两件事口径不同，不能互相替代。
#
# 前置（本脚本不代管服务生命周期，避免把测量基线搞脏）：
#   scripts/dev-up.sh --install      # PG + harnessloop-service + gateway + vite
#
# 用法：
#   bash bench/p2/scripts/run-board-perf.sh [-n 50] [-s 120] [-i 20]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNTIME_DIR="$SCRIPT_DIR/../runtime"

CARDS=50
SECONDS_WINDOW=120
INTERACTIONS=20
BOARD_URL="${BOARD_URL:-http://localhost:5174/harness-loop/monitor}"
PG_CONTAINER="${PG_CONTAINER:-hlp-pg-test}"
PG_USER="${HLP_PG_USER:-agent}"
PG_DB="${HLP_PG_DB:-agent_platform}"

while [ $# -gt 0 ]; do
  case "$1" in
    -n) CARDS="$2"; shift 2 ;;
    -s) SECONDS_WINDOW="$2"; shift 2 ;;
    -i) INTERACTIONS="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

echo "[board] 看板 $BOARD_URL"
if ! curl -sf -o /dev/null "http://localhost:5174/"; then
  echo "[board] vite 未就绪：先执行 scripts/dev-up.sh --install" >&2
  exit 1
fi

echo "[board] 灌入 $CARDS 个看板任务（幂等）"
# seed 脚本固定写 50 行；-n 只影响断言口径，不改变灌数
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 \
  < "$SCRIPT_DIR/seed-board-tasks.sql" >/dev/null

mkdir -p "$RUNTIME_DIR"
OUT="$RUNTIME_DIR/board-frames-${CARDS}.json"

echo "[board] 采样 ${SECONDS_WINDOW}s / 交互 ${INTERACTIONS} 次/类"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/boardperf \
    -url "$BOARD_URL" \
    -cards "$CARDS" \
    -seconds "$SECONDS_WINDOW" \
    -interactions "$INTERACTIONS" \
    -out "$OUT" )

echo "[board] 结果：$OUT"
