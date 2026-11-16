#!/usr/bin/env bash
# P2 索引线全量压测入口（Linux/macOS + Git Bash）。
#
# 两道闸门先过，才有数字可看：
#   1. -mode check：500 个夹具文件必须与 snapshot.json 的 sha256 逐条一致；
#   2. GOMAXPROCS 与 hardware.json 对齐（不匹配会照跑，但结果标 hardware_match=false）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FIXTURE="$REPO_ROOT/bench/repo-500/testdata/repo"

: "${HARNESS_TEST_PG_DSN:?请先 export HARNESS_TEST_PG_DSN（专用 PG 实例，勿用共享库）}"

export GOMAXPROCS=8
cd "$REPO_ROOT"

echo "== snapshot check =="
go run ./bench/repo-500/cmd -mode check -dir "$FIXTURE"

echo "== bench run =="
go run ./bench/repo-500/cmd -mode run \
  -dir "$FIXTURE" \
  -dsn "$HARNESS_TEST_PG_DSN" \
  -tenant tenant-bench-repo500 \
  -project proj-bench-repo500 \
  -concurrency 4 \
  -batch 32 \
  -insert-batch 200 \
  -probe-interval 500ms \
  -probe-query handler \
  -sample 20 \
  -out "$REPO_ROOT/bench/repo-500/last-run.json" \
  "$@"
