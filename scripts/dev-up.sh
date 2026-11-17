#!/usr/bin/env bash
# 一键拉起本地开发栈：PG -> harnessloop-service -> gateway -> vite。
#
# 用法：
#   scripts/dev-up.sh [--demo] [--install] [--rebuild] [--no-build] [-h]
#
#   --demo      演示模式：把三条 P2 新链路开关（编排/模型策略/权限层）临时置为
#               default: true，只写进 .harness/dev/conf 下的临时 features 文件，
#               不改仓库里的 pkg/config/harness_loop/features.yaml
#   --install   允许自动执行 npm ci（默认只提示，不自动装依赖）
#   --rebuild   强制重新编译 Go 服务
#   --no-build  跳过编译，直接使用 .harness/dev/bin 下已有二进制
#
# 环境变量：HLP_DEMO_FLAGS=1 等价于 --demo（便于 CI/脚本里带开关起栈）。
#
# 端口（可用环境变量覆盖）：
#   HLP_PG_PORT=55432  HLP_HARNESSLOOP_PORT=50013  HLP_GATEWAY_PORT=9000  HLP_VITE_PORT=5174
#
# 产物：.harness/dev/{bin,logs,run,conf}（.harness/* 已在 .gitignore 中忽略）

set -uo pipefail

# shellcheck source=./dev-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dev-lib.sh"

# 本地凭据（.harness/dev/env，已 gitignore）：LLM / embedding key 走环境变量，
# 不写进仓库文件；缺失时仍可启动，但 agent-service 调用会失败。
if [ -f "$DEV_DIR/env" ]; then
  # shellcheck disable=SC1091
  . "$DEV_DIR/env"
fi

AGENT_BIN="$BIN_DIR/agent-service${EXE_SUFFIX}"
MCP_BIN="$BIN_DIR/mcp-service${EXE_SUFFIX}"
HARNESS_SVC_BIN="$BIN_DIR/harness-service${EXE_SUFFIX}"

INSTALL_DEPS=0
FORCE_REBUILD=0
NO_BUILD=0
DEMO_FLAGS="${HLP_DEMO_FLAGS:-0}"
STARTED=()

usage() { sed -n '2,19p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --demo)     DEMO_FLAGS=1 ;;
    --install)  INSTALL_DEPS=1 ;;
    --rebuild)  FORCE_REBUILD=1 ;;
    --no-build) NO_BUILD=1 ;;
    -h|--help)  usage; exit 0 ;;
    *) c_err "未知参数：$1"; usage; exit 2 ;;
  esac
  shift
done

case "$DEMO_FLAGS" in
  1|true|TRUE|yes) DEMO_FLAGS=1 ;;
  *)               DEMO_FLAGS=0 ;;
esac

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

# 演示用的 features 文件：三条 P2 新链路开关 default: true，仓库里有一份可复查的
# 副本（scripts/dev-features.demo.yaml），仓库自己的 features.yaml 一个字节都不动。
DEMO_FEATURES_REL="scripts/dev-features.demo.yaml"

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
    -e "s|^\([[:space:]]*entry_agent:[[:space:]]*\).*|\1\"main-agent\"|" \
    "$tpl" >"$out" || return 1

  if [ "$DEMO_FLAGS" = 1 ]; then
    [ -f "$ROOT/$DEMO_FEATURES_REL" ] || { c_err "缺少演示开关文件 $DEMO_FEATURES_REL"; return 1; }
    # 只改 feature_flags.file 这一行：其余 harness_loop 配置仍指向仓库文件。
    sed -e "s|^\([[:space:]]*file:[[:space:]]*\)\"pkg/config/harness_loop/features.yaml\"|\1\"${DEMO_FEATURES_REL}\"|" \
      "$out" >"$out.tmp" && mv "$out.tmp" "$out" || return 1
    c_ok "演示开关：3 条新链路已置为 default: true（$DEMO_FEATURES_REL）" >&2
  fi
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

gen_agent_config() {
  local tpl="$ROOT/services/agent-service/config.example.yaml"
  local out="$CONF_DIR/agent.yaml"
  [ -f "$tpl" ] || { c_err "缺少模板 $tpl"; return 1; }
  local llm_key="${HARNESS_LLM_API_KEY:-}"
  local llm_base="${HARNESS_LLM_BASE_URL:-https://llm-gateway.openkylin.top/v1}"
  local llm_model="${HARNESS_LLM_MODEL:-deepseek-v4.1-flash}"
  local emb_model="${HARNESS_EMBEDDING_MODEL:-text-embedding-v3}"
  if [ -z "$llm_key" ]; then
    c_warn "未设置 HARNESS_LLM_API_KEY（见 .harness/dev/env），agent-service 将无法调用 LLM"
  fi
  sed \
    -e "s|^\([[:space:]]*path:[[:space:]]*\).*agent\.db.*|\1\".harness/dev/agent.db\"|" \
    -e "s|^\([[:space:]]*grpc_port:[[:space:]]*\).*|\1${AGENT_PORT}|" \
    -e "s|^\([[:space:]]*url:[[:space:]]*\).*mongodb.*|\1\"mongodb://127.0.0.1:27017\"|" \
    -e "s|^\([[:space:]]*provider:[[:space:]]*\).*|\1openai|" \
    -e "s|^\([[:space:]]*api_key:[[:space:]]*\).*|\1\"${llm_key}\"|" \
    -e "s|^\([[:space:]]*base_url:[[:space:]]*\).*|\1\"${llm_base}\"|" \
    -e "s|^\([[:space:]]*model:[[:space:]]*\).*|\1\"${llm_model}\"|" \
    -e "s|^\([[:space:]]*embedding_model:[[:space:]]*\).*|\1\"${emb_model}\"|" \
    -e "s|^\([[:space:]]*mcp:[[:space:]]*\).*|\1127.0.0.1:${MCP_PORT}|" \
    -e "s|^\([[:space:]]*harness:[[:space:]]*\).*|\1127.0.0.1:${HARNESS_SVC_PORT}|" \
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
    # dev-down --keep-db 只保留 PG，Mongo 可能已停；这里幂等补齐数据容器。
    docker compose -f "$COMPOSE_FILE" up -d >"$LOG_DIR/pg-compose.log" 2>&1 || c_warn "数据容器补齐失败（Mongo 可能不可用）"
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

gen_mcp_config() {
  local tpl="$ROOT/services/mcp-service/config.example.yaml"
  local out="$CONF_DIR/mcp.yaml"
  [ -f "$tpl" ] || { c_err "缺少模板 $tpl"; return 1; }
  sed \
    -e "s|^\([[:space:]]*grpc_port:[[:space:]]*\).*|\1${MCP_PORT}|" \
    -e "s|^\([[:space:]]*provider:[[:space:]]*\).*|\1openai|" \
    -e "s|^\([[:space:]]*api_key:[[:space:]]*\).*|\1\"${HARNESS_LLM_API_KEY:-}\"|" \
    -e "s|^\([[:space:]]*base_url:[[:space:]]*\).*|\1\"${HARNESS_LLM_BASE_URL:-https://llm-gateway.openkylin.top/v1}\"|" \
    -e "s|^\([[:space:]]*model:[[:space:]]*\).*|\1\"${HARNESS_LLM_MODEL:-deepseek-v4.1-flash}\"|" \
    -e "s|^\([[:space:]]*embedding_model:[[:space:]]*\).*|\1\"${HARNESS_EMBEDDING_MODEL:-text-embedding-v3}\"|" \
    -e "s|^\([[:space:]]*knowledge:[[:space:]]*\).*|\1127.0.0.1:50002|" \
    -e "s|^\([[:space:]]*gateway:[[:space:]]*\).*|\1127.0.0.1:${GATEWAY_PORT}|" \
    -e "s|^\([[:space:]]*url:[[:space:]]*\).*redis://.*|\1\"redis://127.0.0.1:${REDIS_PORT}\"|" \
    "$tpl" >"$out" || return 1
  echo "$out"
}

gen_harness_svc_config() {
  local tpl="$ROOT/services/harness-service/config.example.yaml"
  local out="$CONF_DIR/harness-service.yaml"
  [ -f "$tpl" ] || { c_err "缺少模板 $tpl"; return 1; }
  sed \
    -e "s|^\([[:space:]]*grpc_port:[[:space:]]*\).*|\1${HARNESS_SVC_PORT}|" \
    -e "s|^\([[:space:]]*path:[[:space:]]*\).*harness\.db.*|\1./.harness/dev/harness.db|" \
    -e "s|^\([[:space:]]*provider:[[:space:]]*\).*|\1openai|" \
    -e "s|^\([[:space:]]*api_key:[[:space:]]*\).*|\1\"${HARNESS_LLM_API_KEY:-}\"|" \
    -e "s|^\([[:space:]]*base_url:[[:space:]]*\).*|\1\"${HARNESS_LLM_BASE_URL:-https://llm-gateway.openkylin.top/v1}\"|" \
    -e "s|^\([[:space:]]*model:[[:space:]]*\).*|\1\"${HARNESS_LLM_MODEL:-deepseek-v4.1-flash}\"|" \
    -e "s|^\([[:space:]]*agent:[[:space:]]*\).*|\1127.0.0.1:${AGENT_PORT}|" \
    -e "s|^\([[:space:]]*url:[[:space:]]*\).*redis://.*|\1\"redis://127.0.0.1:${REDIS_PORT}\"|" \
    "$tpl" >"$out" || return 1
  echo "$out"
}

# ---- 步骤 2：redis --------------------------------------------------------
step_redis() {
  if port_open "$REDIS_PORT"; then
    c_ok "redis 已就绪（127.0.0.1:${REDIS_PORT}）"
    return 0
  fi
  command -v docker >/dev/null 2>&1 || { c_err "未找到 docker，无法启动 redis"; return 1; }
  c_step "启动 redis 容器（:${REDIS_PORT}）"
  docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1 || true
  if ! docker run -d --name "$REDIS_CONTAINER" -p "${REDIS_PORT}:6379" redis:7-alpine >"$LOG_DIR/redis.log" 2>&1; then
    c_err "redis 启动失败"
    tail_log redis 20
    return 1
  fi
  if wait_until 30 port_open "$REDIS_PORT"; then
    c_ok "redis 就绪（127.0.0.1:${REDIS_PORT}）"
    return 0
  fi
  c_err "redis 30s 内未就绪"
  return 1
}

# ---- 步骤 3：mcp-service ---------------------------------------------------
step_mcp() {
  local name="mcp"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name")），跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open "$MCP_PORT"; then
    c_warn "$MCP_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  local conf
  conf="$(gen_mcp_config)" || return 1
  build_service "$name" "$MCP_BIN" "./services/mcp-service/cmd" || return 1
  c_step "启动 mcp-service（:${MCP_PORT}，config=$conf）"
  export CONFIG_PATH="$(win_path "$conf")"
  if ! launch "$name" "$ROOT" "$MCP_BIN"; then
    c_err "mcp-service 启动后立即退出"
    return 1
  fi
  if wait_until 40 port_open "$MCP_PORT"; then
    record_pid "$name" "$MCP_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "mcp-service 监听 127.0.0.1:${MCP_PORT}（PID $(pid_of "$name"))"
    return 0
  fi
  c_err "mcp-service 40s 内未监听 ${MCP_PORT}"
  return 1
}

# ---- 步骤 4：harness-service -----------------------------------------------
step_harness_svc() {
  local name="harness-svc"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name")），跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open "$HARNESS_SVC_PORT"; then
    c_warn "$HARNESS_SVC_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  local conf
  conf="$(gen_harness_svc_config)" || return 1
  build_service "$name" "$HARNESS_SVC_BIN" "./services/harness-service/cmd" || return 1
  c_step "启动 harness-service（:${HARNESS_SVC_PORT}，config=$conf）"
  export CONFIG_PATH="$(win_path "$conf")"
  if ! launch "$name" "$ROOT" "$HARNESS_SVC_BIN"; then
    c_err "harness-service 启动后立即退出"
    return 1
  fi
  if wait_until 40 port_open "$HARNESS_SVC_PORT"; then
    record_pid "$name" "$HARNESS_SVC_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "harness-service 监听 127.0.0.1:${HARNESS_SVC_PORT}（PID $(pid_of "$name"))"
    return 0
  fi
  c_err "harness-service 40s 内未监听 ${HARNESS_SVC_PORT}"
  return 1
}

# ---- 步骤 5：agent-service --------------------------------------------------
step_agent() {
  local name="agent"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name")），跳过"
    return 0
  fi
  clear_pidfile "$name"
  if port_open "$AGENT_PORT"; then
    c_warn "$AGENT_PORT 已被其他进程占用，跳过启动（后续健康检查仍需通过）"
    return 0
  fi
  local conf
  conf="$(gen_agent_config)" || return 1
  build_service "$name" "$AGENT_BIN" "./services/agent-service/cmd" || return 1
  c_step "启动 agent-service（:${AGENT_PORT}，config=$conf）"
  # 用 export 而非 `env` 前缀：MSYS/Git Bash 下 env 启动原生 exe 时环境变量不总是透传。
  export CONFIG_PATH="$(win_path "$conf")"
  export PORT="${AGENT_PORT}"
  export HTTP_PORT="${AGENT_HTTP_PORT}"
  export MONGODB_URL="mongodb://127.0.0.1:27017"
  # agent-service 的 context store 用 STORE_PATH（默认 ./data/... 目录不存在会 CANTOPEN）
  export STORE_PATH=".harness/dev/agent-contexts.db"
  if ! launch "$name" "$ROOT" "$AGENT_BIN"; then
    c_err "agent-service 启动后立即退出"
    return 1
  fi
  if wait_until 40 port_open "$AGENT_PORT"; then
    record_pid "$name" "$AGENT_PORT" || c_warn "无法反查 $name 的 PID，dev-down 将按端口清理"
    c_ok "agent-service 监听 127.0.0.1:${AGENT_PORT}（PID $(pid_of "$name"))"
    seed_agent_models
    return 0
  fi
  c_err "agent-service 40s 内未监听 ${AGENT_PORT}"
  return 1
}

# agent 定义存 Mongo；缺 model 字段时 LLM 请求 model 为空，任务会在 a 阶段失败。
# 幂等：只补缺失字段，不覆盖用户已配置的模型。
seed_agent_models() {
  local model="${HARNESS_LLM_MODEL:-deepseek-v4.1-flash}"
  command -v docker >/dev/null 2>&1 || return 0
  docker ps --format '{{.Names}}' 2>/dev/null | grep -qx hlp-mongo || return 0
  docker exec hlp-mongo mongosh --quiet agent_platform \
    --eval "db.agents.updateMany({model:{\$exists:false}}, {\$set:{model:'${model}'}})" >/dev/null 2>&1 \
    || c_warn "agent 模型补齐失败（请手动设置 agents.model=${model}）"
}

# ---- 步骤 3：harnessloop-service ------------------------------------------
step_harnessloop() {
  local name="harnessloop"
  if service_running "$name"; then
    c_ok "$name 已在运行（PID $(pid_of "$name"))，跳过"
    if [ "$DEMO_FLAGS" = 1 ]; then
      c_warn "--demo 只在启动时读开关：当前进程用的还是上次的配置"
      c_dim "  想看到新链路请先 scripts/dev-down.sh 再 scripts/dev-up.sh --demo"
    fi
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

  step_pg || fail_step "pg-compose" "步骤 1/8 PostgreSQL 失败"
  step_redis || fail_step "redis" "步骤 2/8 redis 失败"
  step_mcp || fail_step "mcp" "步骤 3/8 mcp-service 失败"
  step_harness_svc || fail_step "harness-svc" "步骤 4/8 harness-service 失败"
  step_agent || fail_step "agent" "步骤 5/8 agent-service 失败"
  step_harnessloop || fail_step "harnessloop" "步骤 6/8 harnessloop-service 失败"
  step_gateway || fail_step "gateway" "步骤 7/8 gateway 失败"
  step_vite || fail_step "vite" "步骤 8/8 vite 失败"

  local code
  code="$(http_code "$MONITOR_URL" 20)"
  if [ "$code" != "200" ]; then
    c_err "看板地址返回 ${code:-超时}：$MONITOR_URL"
    tail_log "vite" 20
    exit 1
  fi

  printf '\n'
  c_ok "全部 8 项健康检查通过"
  printf '  %-8s %-6s %s\n' "BOARD" "" "$MONITOR_URL"
  printf '  %-22s %-8s %s\n' "agent-service" "$(pid_of agent)" "127.0.0.1:${AGENT_PORT}"
  printf '  %-22s %-8s %s\n' "mcp-service" "$(pid_of mcp)" "127.0.0.1:${MCP_PORT}"
  printf '  %-22s %-8s %s\n' "harness-service" "$(pid_of harness-svc)" "127.0.0.1:${HARNESS_SVC_PORT}"
  printf '  %-22s %-8s %s\n' "harnessloop-service" "$(pid_of harnessloop)" "127.0.0.1:${HARNESSLOOP_PORT}"
  printf '  %-22s %-8s %s\n' "gateway" "$(pid_of gateway)" "127.0.0.1:${GATEWAY_PORT} (/health)"
  printf '  %-22s %-8s %s\n' "vite" "$(pid_of vite)" "127.0.0.1:${VITE_PORT}"
  printf '  %-22s %-8s %s\n' "postgres" "docker" "127.0.0.1:${PG_PORT} (${PG_CONTAINER})"
  printf '  %-22s %-8s %s\n' "redis" "docker" "127.0.0.1:${REDIS_PORT} (${REDIS_CONTAINER})"
  if [ "$DEMO_FLAGS" = 1 ]; then
    printf '\n'
    c_ok "演示开关已生效（features=$DEMO_FEATURES_REL）"
    printf '  %-24s %s\n' "orchestrator" "on  → 多 Agent 编排（DAG / 子任务 / 汇聚）"
    printf '  %-24s %s\n' "model_strategy" "on  → 模型策略选择 + 阶段覆盖 + 回退链"
    printf '  %-24s %s\n' "policy_layers" "on  → 四层权限解析 + 审计 + 高危审批"
    c_dim "  启动日志核对：harnessloop: feature gates (tenant=platform): orchestrator=on ..."
    c_dim "  看板：$MONITOR_URL（多 Agent / 模型 / 权限三视图）"
  fi
  printf '\n'
  c_dim "日志：$LOG_DIR    停止：scripts/dev-down.sh [--keep-db]"
}

main "$@"
