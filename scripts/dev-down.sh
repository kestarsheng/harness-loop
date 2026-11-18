#!/usr/bin/env bash
# 停止本地开发栈：按 .harness/dev/run/*.pid 逐个停止，默认连带停掉数据容器。
#
# 用法：
#   scripts/dev-down.sh [--keep-db] [-h]
#
#   --keep-db   保留 PostgreSQL / MongoDB / Qdrant 容器（只停业务进程）
#
# 清理范围：9 个微服务 + vite + redis 容器；数据容器按 --keep-db 决定。
# 幂等：没有 PID 文件或进程已退出时直接跳过；端口最终必须释放。

set -uo pipefail

# shellcheck source=./dev-lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dev-lib.sh"

KEEP_DB=0
usage() { sed -n '2,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --keep-db) KEEP_DB=1 ;;
    -h|--help) usage; exit 0 ;;
    *) c_err "未知参数：$1"; usage; exit 2 ;;
  esac
  shift
done

# 停业务进程：顺序与启动相反；端口用于兜底反查 PID
SERVICES=(vite gateway harnessloop agent harness-svc mcp a2a chat knowledge memory)
service_port() { service_port_of "$1"; }

report_ports() {
  local p
  for p in "$VITE_PORT" "$GATEWAY_PORT" "$HARNESSLOOP_PORT" "$AGENT_PORT" "$AGENT_HTTP_PORT" \
           "$HARNESS_SVC_PORT" "$MCP_PORT" "$A2A_PORT" "$A2A_HTTP_PORT" "$CHAT_PORT" \
           "$KNOWLEDGE_PORT" "$MEMORY_PORT"; do
    if port_open_any "$p"; then
      c_err "端口 $p 仍在监听"
      return 1
    fi
  done
  c_ok "业务端口已释放：${CHAT_PORT} / ${KNOWLEDGE_PORT} / ${MEMORY_PORT} / ${A2A_PORT}(${A2A_HTTP_PORT}) / ${MCP_PORT} / ${AGENT_PORT}(${AGENT_HTTP_PORT}) / ${HARNESS_SVC_PORT} / ${HARNESSLOOP_PORT} / ${GATEWAY_PORT} / ${VITE_PORT}"
  return 0
}

main() {
  local name pid port rc=0

  ensure_dirs
  for name in "${SERVICES[@]}"; do
    port="$(service_port "$name")"
    pid="$(pid_of "$name")"
    if ! pid_alive "$pid"; then
      # PID 文件缺失或已过期：尝试按端口反查（reboot / 手工启动过的场景）
      pid="$(listener_pid "$port")"
    fi
    if pid_alive "$pid"; then
      c_step "停止 $name（PID $pid，端口 $port）"
      if stop_pid "$pid" 10 && wait_port_free "$port" 10; then
        c_ok "$name 已停止"
      else
        c_err "$name（PID $pid）停止失败或端口 ${port} 未释放"
        rc=1
      fi
    elif port_open_any "$port"; then
      c_err "端口 ${port} 仍被占用，但无法定位 $name 的进程"
      rc=1
    else
      c_dim "  $name 未在运行"
    fi
    clear_pidfile "$name"
  done

  report_ports || rc=1

  # redis 是开发辅助容器（无卷），无论 --keep-db 都清掉，避免残留占用端口。
  if command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$REDIS_CONTAINER"; then
    docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1 && c_ok "redis 容器已移除（$REDIS_CONTAINER）"
  fi

  if [ "$KEEP_DB" = 1 ]; then
    c_ok "保留数据容器 PG / Mongo / Qdrant（--keep-db）"
  elif command -v docker >/dev/null 2>&1; then
    c_step "停止数据容器（docker compose -f docker/docker-compose.data.yaml down）"
    if docker compose -f "$COMPOSE_FILE" down >"$LOG_DIR/pg-compose-down.log" 2>&1; then
      c_ok "数据容器已停止（具名卷 pgdata/mongodata/qdrant-data 保留）"
    else
      c_err "docker compose down 失败"
      sed 's/^/    /' "$LOG_DIR/pg-compose-down.log" >&2
      rc=1
    fi
  else
    c_warn "未找到 docker，跳过容器停止"
  fi

  [ "$rc" = 0 ] && c_ok "dev-down 完成" || c_err "dev-down 存在失败项"
  return "$rc"
}

main "$@"
