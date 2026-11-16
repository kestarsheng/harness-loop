#!/usr/bin/env bash
# 场景：代码混合检索（DFX-P-03）。
#
#   50 query × 10 轮，top_k=20，三路召回经 RRF 合并，不含 Reranker，P95 ≤ 500ms
#
# 前置：仓库必须已经被 bench/repo-500 索引过（同一 tenant/project）。
# 没索引过就先跑：
#   bash bench/repo-500/run.sh           # 注意它会覆盖 last-run.json
# 或（不改只读目录）：
#   go run ./bench/repo-500/cmd -mode run -dir bench/repo-500/testdata/repo \
#     -dsn "$HARNESS_TEST_PG_DSN" -tenant tenant-bench-repo500 \
#     -project proj-bench-repo500 -out /tmp/repo500.json
#
# 用法：
#   bash bench/p2/scripts/run-search.sh [-q 50] [-r 10]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-service.sh
source "$SCRIPT_DIR/lib-service.sh"

Q=50
R=10
while [ $# -gt 0 ]; do
  case "$1" in
    -q) Q="$2"; shift 2 ;;
    -r) R="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

: "${HARNESS_TEST_PG_DSN:?请先 export HARNESS_TEST_PG_DSN}"

log "code search: $Q queries x $R rounds, top_k=20"
( cd "$REPO_ROOT" && go run ./bench/p2/scripts/cmd/searchbench \
    -dsn "$HARNESS_TEST_PG_DSN" \
    -queries "$Q" \
    -rounds "$R" \
    -out "$RUNTIME_DIR/search.json" )
