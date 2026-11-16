#!/usr/bin/env bash
# 一键拉起本地开发栈：PG -> harnessloop-service -> gateway -> vite。
#
# 用法：
#   scripts/dev-up.sh [--install] [--rebuild] [--no-build] [-h]
#
#   --install   允许自动执行 npm ci（默认只提示，不自动装依赖）
#   --rebuild   强制重新编译 Go 服务
#   --no-build  跳过编译，直接使用 .harness/dev/bin 下已有二进制
#
# 端口（可用环境变量覆盖）：
#   HLP_PG_PORT=55432  HLP_HARNESSLOOP_PORT=50013  HLP_GATEWAY_PORT=9000  HLP_VITE_PORT=5174
#
# 产物：.harness/dev/{bin,logs,run,conf}（.harness/* 已在 .gitignore 中忽略）

set -uo pipefail

# shellcheck source=./dev-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dev-lib.sh"

INSTALL_DEPS=0
FORCE_REBUILD=0
NO_BUILD=0
STARTED=()

usage() { sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --install)  INSTALL_DEPS=1 ;;
    --rebuild)  FORCE_REBUILD=1 ;;
    --no-build) NO_BUILD=1 ;;
    -h|--help)  usage; exit 0 ;;
    *) c_err "未知参数：$1"; usage; exit 2 ;;
  esac
  shift
done

# ---- 失败收尾 --------------------------------------------------------------
cleanup_started() {
  [ ${#STARTED[@]} -eq 0 ] && return 0
  c_warn "回滚本次已启动的进程：${STARTED[*]}"
  local n pid
  for n in "${STARTED[@]}"; do
    pid="$(pid_of "$n")"
    if pid_alive "$pid"; then stop_pid "$pid" 5; fi
    clear_pidfile "$n"
  done
  STARTED=()
}

fail_step() { # fail_step <组件> <提示>
  c_err "$2"
  tail_log "$1"
  cleanup_started
  exit 1
}

launch() { # launch <组件> <workdir> <command...>
  local name="$1" workdir="$2"; shift 2
  : >"$LOG_DIR/$name.log"
  if start_service "$name" "$workdir" "$@"; then
    STARTED+=("$name")
    return 0
  fi
  return 1
}

# ---- 配置生成（模板 + 环境变量覆盖） --------------------------------------
sed_escape() { printf '%s' "$1" | sed 's/[&|\\]/\\&/g'; }

# 模板里 policy/models/retry/feature_flags 写的是 "config/harness_loop/*.yaml"，
# 而仓库实际把它们放在 pkg/config/harness_loop/。这里改成「相对仓库根」的路径：
# 服务一律以 $ROOT 为工作目录启动，相对路径在 Windows / Linux 上都成立
# （写绝对路径会踩 MSYS 的 /d/... 与 Windows 原生进程不互通的坑）。
gen_harnessloop_config() {
  local tpl="$ROOT/services/harnessloop-service/config.example.yaml"
  local out="$CONF_DIR/harnessloop.yaml"
  [ -f "$tpl" ] || { c_err "缺少模板 $tpl"; return 1; }
  sed \
    -e "s|^\([[:space:]]*address:[[:space:]]*\).*|\1\"127.0.0.1:${HARNESSLOOP_PORT}\"|" \
    -e "s|^\([[:space:]]*dsn:[[:space:]]*\).*|\1\"$(sed_escape "$PG_DSN")\"|" \
    -e "s|\"config/harness_loop/|\"pkg/config/harness_loop/|g" \
    -e "s|^\([[:space:]]*addr:[[:space:]]*\).*|\1\"\"|" \
    "$tpl" >"$out" || return 1
  echo "$out"
}

gen_gateway_config() {
  local tpl="$ROOT/services/gateway/config.example.yaml"
  local out="$CONF_DIR/gateway.yaml"
  [ -f "$tpl" ] || { c_err "缺少模板 $tpl"; return 1; }
  sed \
    -e "s|^\([[:space:]]*http_port:[[:space:]]*\).*|\1${GATEWAY_PORT}|" \
    -e "s|^\([[:space:]]*[a-z0-9_]*:[[:space:]]*\)[a-z0-9-]*-service:|\1127.0.0.1:|" \
    -e "s|^\([[:space:]]*harness_loop:[[:space:]]*\).*|\1127.0.0.1:${HARNESSLOOP_PORT}|" \
    -e "s|^\([[:space:]]*url:[[:space:]]*\)\"\{0,1\}redis://.*|\1\"\"|" \
    "$tpl" >"$out" || return 1
  echo "$out"
}

# ---- 编译 ------------------------------------------------------------------
needs_rebuild() { # needs_rebuild <bin>
  [ -f "$1" ] || return 0
  [ -n "$(find "$ROOT/services" "$ROOT/pkg" -name '*.go' -newer "$1" -print -quit 2>/dev/null)" ]
}

build_service() { # build_service <name> <bin> <pkg>
  local name="$1" bin="$2" pkg="$3"
  if [ "$NO_BUILD" = 1 ]; then
    [ -f "$bin" ] || { c_err "--no-build 但 $bin 不存在"; return 1; }
    c_ok "$name 使用已有二进制（--no-build）"
    return 0
  fi
  if [ "$FORCE_REBUILD" = 0 ] && ! needs_rebuild "$bin"; then
    c_ok "$name 二进制已是最新，跳过编译"
    return 0
  fi
  c_step "编译 $name ..."
  if ! ( cd "$ROOT" && go build -o "$bin" "$pkg" ) >"$LOG_DIR/build-$name.log" 2>&1; then
    c_err "编译 $name 失败"
    tail_log "build-$name" 40
    return 1
  fi
  c_ok "$name 编译完成"
}

# ---- 步骤 1：PostgreSQL ----------------------------------------------------
step_pg() {
  if pg_ready; then
    c_ok "PG 已就绪（127.0.0.1:${PG_PORT} / ${PG_DB}）"
    return 0
  fi
  command -v docker >/dev/null 2>&1 || { c_err "未找到 docker，无法启动 PG"; return 1; }
  c_step "启动 PG 容器（docker compose -f docker/docker-compose.data.yaml up -d）"
  if ! docker compose -f "$COMPOSE_FILE" up -d >"$LOG_DIR/pg-compose.log" 2>&1; then
    c_err "docker compose up 失败"
    sed 's/^/    /' "$LOG_DIR/pg-compose.log" >&2
    return 1
  fi
  if wait_until 90 pg_ready; then
    c_ok "PG 就绪（127.0.0.1:${PG_PORT} / ${PG_DB}）"
    return 0
  fi
  c_err "PG 90s 内未就绪"
  compose_logs_tail
  return 1
}

# ---- 步骤 2：harnessloop-service ------------------------------------------
step_harnessloop() {
  local name="harnessloop"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name"))，跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open "$HARNESSLOOP_PORT"; then
    c_warn "$HARNESSLOOP_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  local conf
  conf="$(gen_harnessloop_config)" || return 1
  build_service "$name" "$HARNESSLOOP_BIN" "./services/harnessloop-service/cmd" || return 1
  c_step "启动 harnessloop-service（:${HARNESSLOOP_PORT}，config=$conf）"
  if ! launch "$name" "$ROOT" "$HARNESSLOOP_BIN" -config "$(win_path "$conf")"; then
    c_err "harnessloop-service 启动后立即退出"
    return 1
  fi
  if wait_until 40 port_open "$HARNESSLOOP_PORT"; then
    record_pid "$name" "$HARNESSLOOP_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "harnessloop-service 监听 127.0.0.1:${HARNESSLOOP_PORT}（PID $(pid_of "$name"))"
    return 0
  fi
  c_err "harnessloop-service 40s 内未监听 ${HARNESSLOOP_PORT}"
  return 1
}

# ---- 步骤 3：gateway -------------------------------------------------------
step_gateway() {
  local name="gateway"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name"))，跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open "$GATEWAY_PORT"; then
    c_warn "$GATEWAY_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  local conf
  conf="$(gen_gateway_config)" || return 1
  build_service "$name" "$GATEWAY_BIN" "./services/gateway/cmd" || return 1
  c_step "启动 gateway（:${GATEWAY_PORT}，config=$conf）"
  if ! launch "$name" "$ROOT" env CONFIG_PATH="$(win_path "$conf")" "$GATEWAY_BIN"; then
    c_err "gateway 启动后立即退出"
    return 1
  fi
  if wait_until 40 http_is_200 "http://127.0.0.1:${GATEWAY_PORT}/health"; then
    record_pid "$name" "$GATEWAY_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "gateway /health 200（PID $(pid_of "$name"))"
    return 0
  fi
  c_err "gateway 40s 内未通过 /health 检查"
  return 1
}

# ---- 步骤 4：vite ----------------------------------------------------------
ensure_frontend_deps() {
  [ -d "$ROOT/frontend/node_modules" ] && return 0
  if [ "$INSTALL_DEPS" = 0 ]; then
    c_err "frontend/node_modules 缺失"
    c_dim "  请先执行：cd frontend && npm ci"
    c_dim "  或重新运行：scripts/dev-up.sh --install"
    return 1
  fi
  command -v npm >/dev/null 2>&1 || { c_err "未找到 npm"; return 1; }
  c_step "安装前端依赖（npm ci）"
  if ! ( cd "$ROOT/frontend" && npm ci --no-audit --no-fund ) >"$LOG_DIR/npm-install.log" 2>&1; then
    c_err "npm ci 失败"
    tail_log "npm-install" 30
    return 1
  fi
  c_ok "前端依赖安装完成"
}

step_vite() {
  local name="vite"
  : >"$LOG_DIR/$name.log"   # 先清空，避免依赖缺失失败时 tail 到上一次运行的旧日志
  ensure_frontend_deps || return 1
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name"))，跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open_any "$VITE_PORT"; then
    c_warn "$VITE_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  command -v node >/dev/null 2>&1 || { c_err "未找到 node"; return 1; }
  local vite_js="$ROOT/frontend/node_modules/vite/bin/vite.js"
  [ -f "$vite_js" ] || { c_err "未找到 $vite_js（node_modules 不完整，请重新 npm ci）"; return 1; }
  c_step "启动 vite（:${VITE_PORT}，API -> http://localhost:${GATEWAY_PORT}）"
  # vite.config.ts 的 /api 代理指向远端网关，本地开发用 VITE_API_URL 覆盖成直连本机 gateway
  if ! launch "$name" "$ROOT/frontend" env "VITE_API_URL=http://localhost:${GATEWAY_PORT}" node "$vite_js" --port "$VITE_PORT" --strictPort; then
    c_err "vite 启动后立即退出"
    return 1
  fi
  if wait_until 120 http_is_200 "${VITE_BASE}/" 30; then
    record_pid "$name" "$VITE_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "vite 首页 200（PID $(pid_of "$name"))"
    return 0
  fi
  c_err "vite 120s 内未返回 200"
  return 1
}

# ---- 主流程 ----------------------------------------------------------------
main() {
  ensure_dirs
  c_step "仓库：$ROOT"
  c_step "产物目录：$DEV_DIR"

  step_pg || fail_step "pg-compose" "步骤 1/4 PostgreSQL 失败"
  step_harnessloop || fail_step "harnessloop" "步骤 2/4 harnessloop-service 失败"
  step_gateway || fail_step "gateway" "步骤 3/4 gateway 失败"
  step_vite || fail_step "vite" "步骤 4/4 vite 失败"

  local code
  code="$(http_code "$MONITOR_URL" 20)"
  if [ "$code" != "200" ]; then
    c_err "看板地址返回 ${code:-超时}：$MONITOR_URL"
    tail_log "vite" 20
    exit 1
  fi

  printf '\n'
  c_ok "全部 4 项健康检查通过"
  printf '  %-8s %-6s %s\n' "BOARD" "" "$MONITOR_URL"
  printf '  %-22s %-8s %s\n' "harnessloop-service" "$(pid_of harnessloop)" "127.0.0.1:${HARNESSLOOP_PORT}"
  printf '  %-22s %-8s %s\n' "gateway" "$(pid_of gateway)" "127.0.0.1:${GATEWAY_PORT} (/health)"
  printf '  %-22s %-8s %s\n' "vite" "$(pid_of vite)" "127.0.0.1:${VITE_PORT}"
  printf '  %-22s %-8s %s\n' "postgres" "docker" "127.0.0.1:${PG_PORT} (${PG_CONTAINER})"
  printf '\n'
  c_dim "日志：$LOG_DIR    停止：scripts/dev-down.sh [--keep-db]"
}

main "$@"
