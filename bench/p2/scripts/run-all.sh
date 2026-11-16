#!/usr/bin/env bash
# T-P2-T1 全量压测入口：七类场景按顺序跑一遍，结果落到 runtime/。
#
# 设计原则：
#   1. 每个场景独立脚本，可单独重跑；本脚本只负责串起来，不吞错误（set -e）。
#   2. 索引场景复用 bench/repo-500（只读取它的夹具，输出写到 runtime/），
#      不覆盖只读目录里的 last-run.json。
#   3. 跑之前先校验夹具快照 sha256——夹具变了，历史数字就不可比。
#
# 用法：
#   export HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable'
#   bash bench/p2/scripts/run-all.sh
#
# 单项：
#   bash bench/p2/scripts/run-events.sh -n 5
#   bash bench/p2/scripts/run-search.sh
#   bash bench/p2/scripts/run-acceptance.sh
#   bash bench/p2/scripts/run-memload.sh -n 20
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-service.sh
source "$SCRIPT_DIR/lib-service.sh"

: "${HARNESS_TEST_PG_DSN:?请先 export HARNESS_TEST_PG_DSN（专用压测库，勿用共享库）}"

log "== 0. fixture snapshot check =="
( cd "$REPO_ROOT" && go run ./bench/repo-500/cmd -mode check -dir bench/repo-500/testdata/repo )

log "== 1/7 单文件 + 批量索引（DFX-P-01/02）=="
export GOMAXPROCS=8
( cd "$REPO_ROOT" && go run ./bench/repo-500/cmd -mode run \
    -dir bench/repo-500/testdata/repo \
    -dsn "$HARNESS_TEST_PG_DSN" \
    -tenant tenant-bench-repo500 \
    -project proj-bench-repo500 \
    -concurrency 4 -batch 32 -insert-batch 200 \
    -probe-interval 500ms -probe-query handler -sample 20 \
    -out "$RUNTIME_DIR/index-baseline.json" )

log "== 2/7 代码检索（DFX-P-03）=="
bash "$SCRIPT_DIR/run-search.sh" -q 50 -r 10

log "== 3/7 验收（DFX-P-06）=="
bash "$SCRIPT_DIR/run-acceptance.sh" -n 20

log "== 4/7 事件端到端（DFX-P-07）: 5 源任务 =="
bash "$SCRIPT_DIR/run-events.sh" -n 5

log "== 5/7 50 卡片数据面（DFX-P-08 服务端侧）: 50 源任务 =="
bash "$SCRIPT_DIR/run-events.sh" -n 50

log "== 6/7 20 并发内存（DFX-P-09）=="
bash "$SCRIPT_DIR/run-memload.sh" -n 20

log "== 7/7 记忆检索（DFX-P-05）=="
log "未自动化：需真实 embedding 服务（向量路）或 Qdrant+种子数据，见 perf-suite.md 偏差项 2"

log "all scenarios finished; results under $RUNTIME_DIR"
