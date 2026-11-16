#!/usr/bin/env bash
# T-P2-T2 · 混沌与故障注入驱动脚本（design 9.8）
#
# 分工：
#   - 本脚本只做 docker 层面的注入（pause / stop / kill -9 / tmpfs 配额）与断言编排；
#   - 所有"平台看到了什么"的探测走 bench/chaos/probe（真实 pgx / mongo-driver /
#     go-redis 客户端），避免"容器停了但代码静默成功"这种假通过。
#
# 用法：
#   bench/chaos/inject.sh preflight          # 检查 docker / 镜像 / go
#   bench/chaos/inject.sh up                 # 起独立混沌容器（不碰共享开发库）
#   bench/chaos/inject.sh case pg-outage     # 跑单个用例
#   bench/chaos/inject.sh suite              # 跑全部用例并输出汇总
#   bench/chaos/inject.sh status / down
#
# 环境变量：
#   CHAOS_KEEP=1        用例结束后保留容器（默认保留，便于复跑）
#   CHAOS_ARTIFACT_DIR  证据目录，默认 bench/chaos/out
#   CHAOS_*_PORT        端口，默认 55440 / 27018 / 56379
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ART_DIR="${CHAOS_ARTIFACT_DIR:-$ROOT/bench/chaos/out}"
mkdir -p "$ART_DIR"

PG_CONTAINER="${CHAOS_PG_CONTAINER:-hlp-chaos-pg}"
PG_PORT="${CHAOS_PG_PORT:-55440}"
PG_IMAGE="${CHAOS_PG_IMAGE:-pgvector/pgvector:pg16}"
MONGO_CONTAINER="${CHAOS_MONGO_CONTAINER:-hlp-chaos-mongo}"
MONGO_PORT="${CHAOS_MONGO_PORT:-27018}"
MONGO_IMAGE="${CHAOS_MONGO_IMAGE:-mongo:7}"
REDIS_CONTAINER="${CHAOS_REDIS_CONTAINER:-hlp-chaos-redis}"
REDIS_PORT="${CHAOS_REDIS_PORT:-56379}"
REDIS_IMAGE="${CHAOS_REDIS_IMAGE:-redis:7-alpine}"

export CHAOS_PG_DSN="postgres://agent:agent_dev_password@127.0.0.1:${PG_PORT}/agent_platform?sslmode=disable"
export CHAOS_MONGO_URI="mongodb://127.0.0.1:${MONGO_PORT}"
export CHAOS_REDIS_URL="redis://127.0.0.1:${REDIS_PORT}/0"

WAIT_SECONDS="${CHAOS_WAIT_SECONDS:-60}"
PROBE_BIN="$ART_DIR/probe-linux"
PROBE_LOCAL="$ART_DIR/probe$(go env GOEXE 2>/dev/null || true)"
RESULTS="$ART_DIR/results.jsonl"
touch "$RESULTS"

PASS=0
FAIL=0

log() { printf '\033[36m[chaos]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m  PASS\033[0m %s\n' "$*"; PASS=$((PASS + 1)); }
bad() { printf '\033[31m  FAIL\033[0m %s\n' "$*"; FAIL=$((FAIL + 1)); }

record() { # record <case> <verdict> <detail> — detail 必须是合法 JSON 字符串
  local detail
  detail="$(printf '%s' "${3:-}" | tr '\n' ' ' | sed "s/\"/'/g")"
  printf '{"case":"%s","verdict":"%s","detail":"%s"}\n' "$1" "$2" "$detail" >>"$RESULTS"
}

winpath() { # docker -v 需要 Windows 风格路径（Git Bash 下）
  if command -v cygpath >/dev/null 2>&1; then
    cygpath -w "$1"
  else
    printf '%s' "$1"
  fi
}

probe() { # probe <op> [extra args...] — 优先用编译好的本地探针（每个用例少 30s）
  if [ -x "$PROBE_LOCAL" ]; then
    "$PROBE_LOCAL" -op "$@"
  else
    ( cd "$ROOT" && go run ./bench/chaos/probe -op "$@" )
  fi
}

cmd_build() {
  log "building probe binaries"
  ( cd "$ROOT" && go build -o "$PROBE_LOCAL" ./bench/chaos/probe ) || return 1
  probe_bin || return 1
  return 0
}

probe_bin() { # 已编译的 linux 探针（容器内用）
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$PROBE_BIN" ./bench/chaos/probe )
}

wait_container() { # wait_container <container> <ready-cmd>
  local name="$1" ready="$2" waited=0
  while [ "$waited" -lt "$WAIT_SECONDS" ]; do
    if docker exec "$name" sh -c "$ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

# ---------------------------------------------------------------------------
# 容器生命周期
# ---------------------------------------------------------------------------

cmd_preflight() {
  local rc=0
  if command -v docker >/dev/null 2>&1; then
    log "found docker: $(docker --version | head -1)"
  else
    log "missing docker"
    rc=1
  fi
  if command -v go >/dev/null 2>&1; then
    log "found go: $(go version)"
  else
    log "missing go"
    rc=1
  fi
  for image in "$PG_IMAGE" "$MONGO_IMAGE" "$REDIS_IMAGE"; do
    if docker image inspect "$image" >/dev/null 2>&1; then
      log "image present: $image"
    else
      log "image missing locally (will pull): $image"
    fi
  done
  return $rc
}

cmd_build_guard() {
  if [ ! -x "$PROBE_LOCAL" ]; then
    cmd_build || { log "probe build failed"; return 1; }
  fi
  return 0
}

cmd_up() {
  cmd_preflight || return 1
  docker rm -f "$PG_CONTAINER" "$MONGO_CONTAINER" "$REDIS_CONTAINER" >/dev/null 2>&1
  log "starting chaos dependencies"
  docker run -d --name "$PG_CONTAINER" \
    -e POSTGRES_USER=agent -e POSTGRES_PASSWORD=agent_dev_password -e POSTGRES_DB=agent_platform \
    -p "${PG_PORT}:5432" "$PG_IMAGE" >/dev/null || return 1
  docker run -d --name "$MONGO_CONTAINER" -p "${MONGO_PORT}:27017" "$MONGO_IMAGE" >/dev/null || return 1
  docker run -d --name "$REDIS_CONTAINER" -p "${REDIS_PORT}:6379" "$REDIS_IMAGE" >/dev/null || return 1

  wait_container "$PG_CONTAINER" 'pg_isready -U agent -d agent_platform' || { log "pg not ready"; return 1; }
  wait_container "$MONGO_CONTAINER" 'mongosh --quiet --eval "db.adminCommand({ping:1}).ok"' || { log "mongo not ready"; return 1; }
  wait_container "$REDIS_CONTAINER" 'redis-cli ping' || { log "redis not ready"; return 1; }
  log "chaos dependencies ready (pg:$PG_PORT mongo:$MONGO_PORT redis:$REDIS_PORT)"
  return 0
}

cmd_down() {
  docker rm -f "$PG_CONTAINER" "$MONGO_CONTAINER" "$REDIS_CONTAINER" >/dev/null 2>&1
  log "chaos containers removed"
}

cmd_status() {
  docker ps -a --filter "name=hlp-chaos-" --format '{{.Names}}\t{{.Status}}' || true
}

# ---------------------------------------------------------------------------
# 用例
# ---------------------------------------------------------------------------

# 摘要里带时间戳，比较时只取 digest + rows（否则永远"变化"）。
digest_key() {
  local raw="$1" d r
  d="$(printf '%s' "$raw" | sed -n 's/.*"digest":"\([^"]*\)".*/\1/p')"
  r="$(printf '%s' "$raw" | sed -n 's/.*"rows":\([0-9]*\).*/\1/p')"
  printf '%s:%s' "$d" "$r"
}

digest_pg() { probe pg-digest 2>/dev/null; }
digest_mongo() { probe mongo-digest 2>/dev/null; }
digest_redis() { probe redis-digest 2>/dev/null; }

ensure_fixtures() {
  probe pg-migrate -rows 8 >/dev/null || { log "pg fixture failed"; return 1; }
  probe mongo-init >/dev/null || { log "mongo fixture failed"; return 1; }
  probe redis-init >/dev/null || { log "redis fixture failed"; return 1; }
  return 0
}

# 通用停机用例：<case> <container> <stopcmd> <startcmd> <readycmd> <digestfn> <probepop>
outage_case() {
  local case_name="$1" container="$2" stopcmd="$3" startcmd="$4" readycmd="$5" digestfn="$6" probeop="$7"
  local before after degraded
  before="$($digestfn)"
  if [ -z "$before" ]; then
    bad "$case_name: 基线摘要不可用，拒绝把'没测到'当成'通过'"
    record "$case_name" "fail" "no baseline digest"
    return 1
  fi
  log "$case_name: baseline $before"
  eval "$stopcmd" >/dev/null 2>&1
  degraded="$(probe "$probeop" 2>/dev/null)"
  case "$degraded" in
  *'"result":"degraded"'*) ok "$case_name: 停机期间调用显式降级（未静默放行） → $degraded" ;;
  *)
    bad "$case_name: 停机期间仍然成功或未报错 → $degraded"
    record "$case_name" "fail" "no explicit degradation: $degraded"
    eval "$startcmd" >/dev/null 2>&1
    return 1
    ;;
  esac
  eval "$startcmd" >/dev/null 2>&1
  if ! wait_container "$container" "$readycmd"; then
    bad "$case_name: 依赖未能恢复"
    record "$case_name" "fail" "dependency did not recover"
    return 1
  fi
  after="$($digestfn)"
  if [ "$(digest_key "$before")" = "$(digest_key "$after")" ]; then
    ok "$case_name: 恢复后摘要一致（无数据损坏） → $(digest_key "$after")"
    record "$case_name" "pass" "digest unchanged $(digest_key "$after")"
  else
    bad "$case_name: 恢复后摘要变化 before=$(digest_key "$before") after=$(digest_key "$after")"
    record "$case_name" "fail" "digest changed"
    return 1
  fi
}

case_pg_outage() {
  outage_case "pg-outage" "$PG_CONTAINER" "docker stop $PG_CONTAINER" "docker start $PG_CONTAINER" \
    'pg_isready -U agent -d agent_platform' digest_pg pg-probe
}

case_pg_pause() {
  outage_case "pg-pause" "$PG_CONTAINER" "docker pause $PG_CONTAINER" "docker unpause $PG_CONTAINER" \
    'pg_isready -U agent -d agent_platform' digest_pg pg-probe
}

case_mongo_outage() {
  outage_case "mongo-outage" "$MONGO_CONTAINER" "docker stop $MONGO_CONTAINER" "docker start $MONGO_CONTAINER" \
    'mongosh --quiet --eval "db.adminCommand({ping:1}).ok"' digest_mongo mongo-probe
}

case_redis_outage() {
  outage_case "redis-outage" "$REDIS_CONTAINER" "docker stop $REDIS_CONTAINER" "docker start $REDIS_CONTAINER" \
    'redis-cli ping' digest_redis redis-probe
}

# kill -9：事务写到一半被内核强杀，重启后那一行必须不存在（不是"半写"）。
case_kill9() {
  local case_name="kill9-pg"
  local before after txlog row
  before="$(digest_pg)"
  log "$case_name: baseline $before"
  txlog="$ART_DIR/pg-tx.log"
  : >"$txlog"
  ( cd "$ROOT" && go run ./bench/chaos/probe -op pg-tx -hold 120s >"$txlog" 2>&1 ) &
  local pid=$!
  local waited=0
  while ! grep -q '"tx-open"' "$txlog" 2>/dev/null; do
    if [ "$waited" -ge 60 ]; then
      kill -9 "$pid" 2>/dev/null
      bad "$case_name: 事务未能打开"
      record "$case_name" "fail" "tx never opened"
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  row="$(sed -n 's/.*"row":"\([^"]*\)".*/\1/p' "$txlog" | head -1)"
  log "$case_name: tx open with row $row; SIGKILL the database"
  docker kill -s KILL "$PG_CONTAINER" >/dev/null 2>&1
  kill -9 "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  docker start "$PG_CONTAINER" >/dev/null 2>&1
  if ! wait_container "$PG_CONTAINER" 'pg_isready -U agent -d agent_platform'; then
    bad "$case_name: 数据库未能恢复"
    record "$case_name" "fail" "pg did not recover"
    return 1
  fi
  after="$(digest_pg)"
  if [ "$(digest_key "$before")" != "$(digest_key "$after")" ]; then
    bad "$case_name: 未被提交的事务留下了痕迹 before=$(digest_key "$before") after=$(digest_key "$after")"
    record "$case_name" "fail" "uncommitted row survived"
    return 1
  fi
  ok "$case_name: 未提交事务被回滚，摘要不变 → $after"
  record "$case_name" "pass" "uncommitted tx rolled back (row $row)"
}

# 磁盘满：1MB tmpfs 内写 8MB，必须明确报错且不留半写文件。
case_disk_full() {
  local case_name="disk-full"
  if ! probe_bin; then
    bad "$case_name: 无法编译 linux 探针"
    record "$case_name" "fail" "probe build failed"
    return 1
  fi
  local out
  # MSYS_NO_PATHCONV：Git Bash 会把容器内 /work/... 当成 Windows 路径改写。
  out="$(MSYS_NO_PATHCONV=1 docker run --rm --tmpfs /data:size=1m \
    -v "$(winpath "$ART_DIR"):/work" alpine:latest \
    /work/probe-linux -op disk-full -dir /data -size-mb 8 2>&1)"
  if printf '%s' "$out" | grep -q '"result":"surfaced"' &&
    printf '%s' "$out" | grep -q '"no_corrupt":true'; then
    ok "$case_name: 写入明确失败且无半写文件 → $out"
    record "$case_name" "pass" "$out"
  else
    bad "$case_name: 磁盘满时行为不符合预期 → $out"
    record "$case_name" "fail" "$out"
    return 1
  fi
}

case_workspace_drift() {
  local case_name="workspace-drift"
  local out
  out="$(cd "$ROOT" && go test -count=1 -run 'TestChaosWorkspaceDrift' ./bench/chaos/ 2>&1)"
  if [ $? -eq 0 ]; then
    ok "$case_name: 外部改动被识别为 drift（不静默）"
    record "$case_name" "pass" "drift detected"
  else
    bad "$case_name: $out"
    record "$case_name" "fail" "$out"
    return 1
  fi
}

case_app_faults() {
  local case_name="$1" pattern="$2"
  local out
  out="$(cd "$ROOT" && HARNESS_CHAOS=1 go test -count=1 -run "$pattern" ./bench/chaos/ 2>&1)"
  if [ $? -eq 0 ]; then
    ok "$case_name"
    record "$case_name" "pass" "$pattern"
  else
    bad "$case_name: $out"
    record "$case_name" "fail" "$out"
    return 1
  fi
}

run_case() {
  case "$1" in
  pg-outage) case_pg_outage ;;
  pg-pause) case_pg_pause ;;
  mongo-outage) case_mongo_outage ;;
  redis-outage) case_redis_outage ;;
  kill9) case_kill9 ;;
  disk-full) case_disk_full ;;
  workspace-drift) case_workspace_drift ;;
  subtask-hang) case_app_faults "subtask-hang" 'TestChaosSubtaskHang' ;;
  quota-full) case_app_faults "quota-full" 'TestChaosQuotaSaturated' ;;
  models-down) case_app_faults "models-down" 'TestChaosAllModelsUnavailable' ;;
  approval-down) case_app_faults "approval-down" 'TestChaosApprovalUnreachable' ;;
  *)
    log "unknown case: $1"
    return 2
    ;;
  esac
}

cmd_suite() {
  : >"$RESULTS"
  cmd_build_guard || return 1
  cmd_up || { log "dependencies unavailable, suite aborted"; return 1; }
  ensure_fixtures || { log "fixtures unavailable, suite aborted"; return 1; }
  for c in pg-pause pg-outage mongo-outage redis-outage kill9 disk-full workspace-drift \
    subtask-hang quota-full models-down approval-down; do
    log "case: $c"
    run_case "$c"
  done
  log "summary: PASS=$PASS FAIL=$FAIL"
  log "evidence: $RESULTS"
  if [ "$FAIL" -ne 0 ]; then
    return 1
  fi
  return 0
}

main() {
  local cmd="${1:-help}"
  shift || true
  case "$cmd" in
  preflight) cmd_preflight ;;
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  build) cmd_build ;;
  case)
    local name="${1:-}"
    [ -z "$name" ] && { log "usage: inject.sh case <name>"; return 2; }
    cmd_build_guard || return 1
    run_case "$name"
    ;;
  suite) cmd_suite ;;
  *)
    sed -n '2,30p' "${BASH_SOURCE[0]}"
    return 1
    ;;
  esac
}

main "$@"
