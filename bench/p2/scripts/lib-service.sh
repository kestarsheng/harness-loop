#!/usr/bin/env bash
# 压测运行时公共库：构建二进制、起停服务、等就绪。
#
# 为什么服务生命周期必须由脚本自己管：
# 压测要测的是「刚启动的进程」的基线内存曲线。如果服务是上一轮遗留的，
# RSS 已经带着上一批任务没回收的堆，per_task 会被算小。所以每个场景
# 都从 taskkill → 冷启动 → 测量 → 收尸 这条完整链路走一遍。
#
# 被 run-memload.sh / run-events.sh 等 source，不单独执行。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
RUNTIME_DIR="$SCRIPT_DIR/runtime"
BIN_DIR="${P2T1_BIN_DIR:-${TMPDIR:-/tmp}/p2t1bin}"

HLP_ADDR="${HLP_ADDR:-127.0.0.1:50008}"
AGENT_ADDR="${AGENT_ADDR:-127.0.0.1:50002}"

mkdir -p "$BIN_DIR" "$RUNTIME_DIR"

log() { printf '[p2t1] %s\n' "$*" >&2; }

# wait_port <host:port> <timeout-seconds>
wait_port() {
  local hostport="$1" deadline=$(( $(date +%s) + ${2:-30} ))
  local host="${hostport%:*}" port="${hostport##*:}"
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
      exec 3>&- 3<&-
      return 0
    fi
    sleep 0.2
  done
  return 1
}

build_binaries() {
  log "building harnessloop-service / fakeagent / gateway"
  ( cd "$REPO_ROOT" && go build -o "$BIN_DIR/harnessloop-service.exe" ./services/harnessloop-service/cmd )
  ( cd "$REPO_ROOT" && go build -o "$BIN_DIR/fakeagent.exe" ./scripts/fakeagent )
}

kill_named() {
  local name="$1"
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
      taskkill //IM "$name.exe" //F >/dev/null 2>&1 || true
      ;;
    *)
      pkill -f "$name" >/dev/null 2>&1 || true
      ;;
  esac
}

# 冷启动被测服务，并把 PID 写进 $RUNTIME_DIR/service.pid
start_harnessloop() {
  kill_named harnessloop-service
  sleep 0.5
  ( cd "$RUNTIME_DIR" && "$BIN_DIR/harnessloop-service.exe" -config harnessloop.yaml > "$RUNTIME_DIR/service.log" 2>&1 ) &
  SERVICE_PID=$!
  if ! wait_port "$HLP_ADDR" 40; then
    log "harnessloop-service failed to listen on $HLP_ADDR; log tail:"
    tail -20 "$RUNTIME_DIR/service.log" >&2 || true
    return 1
  fi
  # 服务进程本身就是这个后台作业的子进程；真正吃内存的是 harnessloop-service.exe
  log "harnessloop-service up on $HLP_ADDR (launcher pid $SERVICE_PID)"
}

ensure_fakeagent() {
  if wait_port "$AGENT_ADDR" 1; then
    log "fakeagent already listening on $AGENT_ADDR"
    return 0
  fi
  ( cd "$REPO_ROOT" && "$BIN_DIR/fakeagent.exe" -addr "$AGENT_ADDR" > "$RUNTIME_DIR/fakeagent.log" 2>&1 ) &
  FAKEAGENT_PID=$!
  if ! wait_port "$AGENT_ADDR" 20; then
    log "fakeagent failed to listen on $AGENT_ADDR"
    tail -20 "$RUNTIME_DIR/fakeagent.log" >&2 || true
    return 1
  fi
  log "fakeagent up on $AGENT_ADDR"
}

stop_harnessloop() {
  kill_named harnessloop-service
}

# 冷启动网关：只为事件场景服务。
start_gateway() {
  kill_named gateway
  sleep 0.5
  ( cd "$RUNTIME_DIR" && CONFIG_PATH=gateway.yaml "$BIN_DIR/gateway.exe" > "$RUNTIME_DIR/gateway.log" 2>&1 ) &
  GATEWAY_PID=$!
  if ! wait_port "${GW_ADDR:-127.0.0.1:9000}" 40; then
    log "gateway failed to listen; log tail:"
    tail -20 "$RUNTIME_DIR/gateway.log" >&2 || true
    return 1
  fi
  log "gateway up on ${GW_ADDR:-127.0.0.1:9000}"
}

stop_gateway() {
  kill_named gateway
}

service_pid() {
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
      tasklist //FI "IMAGENAME eq harnessloop-service.exe" //FO CSV //NH 2>/dev/null \
        | head -1 | awk -F'","' '{gsub(/"/,"",$2); print $2}'
      ;;
    *)
      pgrep -f harnessloop-service | head -1
      ;;
  esac
}
