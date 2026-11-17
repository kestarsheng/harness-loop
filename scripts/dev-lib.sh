#!/usr/bin/env bash
# 本地开发栈的公共定义与工具函数。
# 由 dev-up.sh / dev-down.sh source 使用，不单独执行。
#
# 约定：
#   - 运行期产物（二进制 / 日志 / PID / 生成的临时配置）全部落在 .harness/dev/
#   - 端口与账号均可用 HLP_* 环境变量覆盖
#   - 兼容 Windows Git Bash（MSYS）与 Linux / macOS
#
# 关于 Windows 的 PID：在 Git Bash 里 `cmd &` 拿到的 $! 是 MSYS 中间进程的 PID，
# 与真正的 Windows 进程 PID 不同，`kill` 也打不到它。因此本脚本在 Windows 上
# 统一用 netstat 反查监听端口的真实 PID 落盘，停止时用 taskkill。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DEV_DIR="$ROOT/.harness/dev"
BIN_DIR="$DEV_DIR/bin"
LOG_DIR="$DEV_DIR/logs"
RUN_DIR="$DEV_DIR/run"
CONF_DIR="$DEV_DIR/conf"

# ---- 端口 / 数据库 ---------------------------------------------------------
PG_PORT="${HLP_PG_PORT:-55432}"
AGENT_PORT="${HLP_AGENT_PORT:-50002}"
AGENT_HTTP_PORT="${HLP_AGENT_HTTP_PORT:-50009}"
MCP_PORT="${HLP_MCP_PORT:-50005}"
HARNESS_SVC_PORT="${HLP_HARNESS_SVC_PORT:-50007}"
REDIS_PORT="${HLP_REDIS_PORT:-56379}"
REDIS_CONTAINER="${HLP_REDIS_CONTAINER:-hlp-dev-redis}"
HARNESSLOOP_PORT="${HLP_HARNESSLOOP_PORT:-50013}"
GATEWAY_PORT="${HLP_GATEWAY_PORT:-9000}"
VITE_PORT="${HLP_VITE_PORT:-5174}"

PG_USER="${HLP_PG_USER:-agent}"
PG_PASSWORD="${HLP_PG_PASSWORD:-agent_dev_password}"
PG_DB="${HLP_PG_DB:-agent_platform}"
PG_DSN="${HLP_PG_DSN:-postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${PG_DB}?sslmode=disable}"

COMPOSE_FILE="$ROOT/docker/docker-compose.data.yaml"
PG_CONTAINER="${HLP_PG_CONTAINER:-hlp-pg-test}"

MONITOR_PATH="/harness-loop/monitor"
# vite 默认只绑定 localhost（IPv6 / ::1），因此前端探活统一走 localhost 而非 127.0.0.1
VITE_BASE="http://localhost:${VITE_PORT}"
MONITOR_URL="${VITE_BASE}${MONITOR_PATH}"

# ---- 平台差异 --------------------------------------------------------------
IS_WINDOWS=0
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*) IS_WINDOWS=1 ;;
esac

EXE_SUFFIX=""
[ "$IS_WINDOWS" = 1 ] && EXE_SUFFIX=".exe"

# 把 MSYS 路径（/d/...）转成 Windows 原生进程读得懂的路径（D:/...）。
# 让「配置文件里的绝对路径」在两种平台都成立；非 Windows 平台原样返回。
win_path() {
  if [ "$IS_WINDOWS" = 1 ] && command -v cygpath >/dev/null 2>&1; then
    cygpath -m "$1"
  else
    printf '%s\n' "$1"
  fi
}

HARNESSLOOP_BIN="$BIN_DIR/harnessloop-service${EXE_SUFFIX}"
GATEWAY_BIN="$BIN_DIR/gateway${EXE_SUFFIX}"

# ---- 输出 ------------------------------------------------------------------
if [ -t 1 ]; then
  _C_RESET='\033[0m'; _C_CYAN='\033[36m'; _C_GREEN='\033[32m'
  _C_YELLOW='\033[33m'; _C_RED='\033[31m'; _C_DIM='\033[2m'
else
  _C_RESET=''; _C_CYAN=''; _C_GREEN=''; _C_YELLOW=''; _C_RED=''; _C_DIM=''
fi

c_step() { printf "${_C_CYAN}[dev ]${_C_RESET} %s\n" "$*"; }
c_ok()   { printf "${_C_GREEN}[ ok ]${_C_RESET} %s\n" "$*"; }
c_warn() { printf "${_C_YELLOW}[warn]${_C_RESET} %s\n" "$*"; }
c_err()  { printf "${_C_RED}[fail]${_C_RESET} %s\n" "$*" >&2; }
c_dim()  { printf "${_C_DIM}%s${_C_RESET}\n" "$*"; }

ensure_dirs() { mkdir -p "$BIN_DIR" "$LOG_DIR" "$RUN_DIR" "$CONF_DIR"; }

# ---- 基础探测 --------------------------------------------------------------
port_open_host() { # port_open_host <host> <port>
  (exec 3<>"/dev/tcp/$1/$2") >/dev/null 2>&1
}

port_open() { port_open_host 127.0.0.1 "$1"; }

port_open_any() { # port_open_any <port>：127.0.0.1 或 localhost(::1) 任一可达即算占用
  port_open_host 127.0.0.1 "$1" || port_open_host localhost "$1"
}

http_code() { # http_code <url> [timeout_seconds]
  curl -s -o /dev/null -m "${2:-3}" -w '%{http_code}' "$1" 2>/dev/null || true
}

http_is_200() { # http_is_200 <url> [timeout_seconds]
  [ "$(http_code "$1" "${2:-3}")" = "200" ]
}

listener_pid() { # listener_pid <port> -> 正在监听该端口的进程 PID（拿不到则空）
  if command -v ss >/dev/null 2>&1; then
    ss -ltnp 2>/dev/null \
      | awk -v p=":$1" '$4 ~ (p "$")' \
      | sed -n 's/.*pid=\([0-9]\{1,\}\).*/\1/p' | head -1
  elif command -v lsof >/dev/null 2>&1; then
    lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null | head -1
  elif command -v netstat >/dev/null 2>&1; then
    netstat -ano 2>/dev/null \
      | awk -v p=":$1" '$2 ~ (p "$") && $5 ~ /^[0-9]+$/ {print $5; exit}'
  fi
}

pid_of() { # pid_of <name> -> 打印 PID（不存在则为空）
  local f="$RUN_DIR/$1.pid"
  [ -f "$f" ] || return 0
  tr -dc '0-9' <"$f"
}

pid_alive() { # pid_alive <pid>
  [ -n "${1:-}" ] || return 1
  if [ "$IS_WINDOWS" = 1 ]; then
    tasklist //FI "PID eq $1" //FO CSV 2>/dev/null | grep -q "\"$1\""
  else
    kill -0 "$1" 2>/dev/null
  fi
}

service_running() { # service_running <name>
  pid_alive "$(pid_of "$1")"
}

record_pid() { # record_pid <name> <port>：把真实监听 PID 落盘
  local name="$1" port="$2" pid
  pid="$(listener_pid "$port")"
  if [ -n "$pid" ]; then
    echo "$pid" >"$RUN_DIR/$name.pid"
    return 0
  fi
  return 1
}

clear_pidfile() { rm -f "$RUN_DIR/$1.pid"; }

wait_until() { # wait_until <timeout_seconds> <command...>
  local timeout="$1"; shift
  local deadline=$(( $(date +%s) + timeout ))
  while :; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 1
  done
}

wait_port_free() { # wait_port_free <port> [timeout_seconds]
  local port="$1" timeout="${2:-10}"
  local deadline=$(( $(date +%s) + timeout ))
  while :; do
    port_open_any "$port" || return 0
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 1
  done
}

pg_ready() {
  if command -v pg_isready >/dev/null 2>&1; then
    pg_isready -h 127.0.0.1 -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -q
  elif command -v docker >/dev/null 2>&1; then
    docker exec "$PG_CONTAINER" pg_isready -U "$PG_USER" -d "$PG_DB" -q >/dev/null 2>&1
  else
    port_open "$PG_PORT"
  fi
}

# ---- 进程启停 --------------------------------------------------------------
start_service() { # start_service <name> <workdir> <command...>
  local name="$1" workdir="$2"; shift 2
  if [ "$IS_WINDOWS" = 1 ]; then
    # 不做 $! 落盘：MSYS 中间进程 PID 打不到真实服务，改由 record_pid 反查
    ( cd "$workdir" && nohup "$@" >>"$LOG_DIR/$name.log" 2>&1 & )
    sleep 1
    return 0
  fi
  ( cd "$workdir" && nohup "$@" >>"$LOG_DIR/$name.log" 2>&1 & echo $! >"$RUN_DIR/$name.pid" )
  sleep 1
  pid_alive "$(pid_of "$name")"
}

stop_pid() { # stop_pid <pid> [grace_seconds]
  local pid="$1" grace="${2:-10}"
  pid_alive "$pid" || return 0
  if [ "$IS_WINDOWS" = 1 ]; then
    # Windows 控制台程序收不到 SIGTERM，直接连带子进程强杀
    taskkill //PID "$pid" //T //F >/dev/null 2>&1
    local i=0
    while [ "$i" -lt 20 ]; do
      pid_alive "$pid" || return 0
      sleep 0.5
      i=$(( i + 1 ))
    done
    return 1
  fi
  kill -TERM "$pid" 2>/dev/null
  local i=0
  while [ "$i" -lt $(( grace * 2 )) ]; do
    pid_alive "$pid" || return 0
    sleep 0.5
    i=$(( i + 1 ))
  done
  c_warn "PID $pid 未在 ${grace}s 内退出，强制结束"
  kill -KILL "$pid" 2>/dev/null
  sleep 0.5
  pid_alive "$pid" && return 1
  return 0
}

# ---- 失败处理 --------------------------------------------------------------
tail_log() { # tail_log <name> [lines]
  local f="$LOG_DIR/$1.log"
  if [ -f "$f" ]; then
    c_err "----- $1 日志尾部（$f）-----"
    tail -n "${2:-40}" "$f" | sed 's/^/    /' >&2
    c_err "----- 日志结束 -----"
  else
    c_err "（未找到日志文件 $f）"
  fi
}

compose_logs_tail() {
  docker compose -f "$COMPOSE_FILE" logs --tail=40 2>&1 | sed 's/^/    /' >&2
}
