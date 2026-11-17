# 本地开发栈：dev-up / dev-down

本地一键拉起 **14 项**：4 个数据容器（PostgreSQL / MongoDB / Qdrant / Redis）、
9 个微服务（gateway / chat / knowledge / memory / a2a / mcp / agent / harness /
harnessloop）与前端 vite。不引入生产 compose 全量栈，也不改动服务源码。

启停项、端口与健康口径以 [`scripts/dev-stack.json`](../scripts/dev-stack.json) 为唯一
数据源（T-P2-21 的 `harness-cli` 自举复用同一份声明）。

## 前置

| 依赖 | 说明 |
| --- | --- |
| Docker | 用于 `docker/docker-compose.data.yaml`（PostgreSQL 55432 / MongoDB 27017 / Qdrant 6333） |
| Go 1.22+ | 编译 9 个 Go 服务 |
| Node 20+ / npm | 前端 vite 开发服务器；首次需 `cd frontend && npm ci` |
| bash | Windows 用 Git Bash（脚本已适配 MSYS）；也可用 `scripts/dev-up.ps1` 包装 |

`.harness/dev/env`（已 gitignore）放 LLM / embedding 凭据：

```bash
export HARNESS_LLM_API_KEY=...
export HARNESS_LLM_BASE_URL=https://llm-gateway.openkylin.top/v1
export HARNESS_LLM_MODEL=qwen-plus
export HARNESS_EMBEDDING_API_KEY=...
export HARNESS_EMBEDDING_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
export HARNESS_EMBEDDING_MODEL=text-embedding-v3
```

缺失时服务仍可启动，但 LLM / embedding 调用会失败（knowledge 上传会停在 failed）。

## 用法

```bash
scripts/dev-up.sh                 # 依次拉起 14 项，末尾打印 14 项健康表
scripts/dev-up.sh --install       # node_modules 缺失时自动 npm ci
scripts/dev-up.sh --rebuild       # 强制重新编译 Go 服务
scripts/dev-up.sh --no-build      # 跳过编译，直接用已有二进制
scripts/dev-up.sh --demo          # 演示：打开三条 P2 新链路开关（编排/模型策略/权限层）
HLP_DEMO_FLAGS=1 scripts/dev-up.sh # 同上，环境变量写法

scripts/dev-down.sh               # 停 9 服务 + vite + redis，再停 PG/Mongo/Qdrant
scripts/dev-down.sh --keep-db     # 只停业务进程，保留 PG / Mongo / Qdrant
```

退出码：`0` 表示 14 项全绿；非 `0` 表示某一步失败，失败时打印该步日志尾部，并把本次
已启动的进程回滚掉（按 PID 文件或端口反查，不会留下半截栈）。

### `--demo`：一键看到新链路

`--demo` 把 harnessloop-service 的 `feature_flags.file` 指向
`scripts/dev-features.demo.yaml`（三条开关 `default: true`），仓库自己的
`pkg/config/harness_loop/features.yaml` 保持默认全关，不被改写。开关只在进程启动时
读一次，所以已经跑着栈的话要先 `scripts/dev-down.sh` 再 `--demo` 起。

```bash
grep 'feature gates' .harness/dev/logs/harnessloop.log
# harnessloop: feature gates (tenant=platform): orchestrator=on model_strategy=on policy_layers=on
grep 'memory recall' .harness/dev/logs/harnessloop.log
# harnessloop: memory recall enabled: 127.0.0.1:50003
```

## 步骤与端口（14 项）

| # | 组件 | 端口 | 类型 | 就绪判据 |
| --- | --- | --- | --- | --- |
| 1 | PostgreSQL（`hlp-pg-test`，库 `agent_platform`） | 55432 | container | `pg_isready` |
| 2 | MongoDB（`hlp-mongo`） | 27017 | container | 端口可连 |
| 3 | Qdrant（`hlp-qdrant`） | 6333 | container | `GET /healthz` = 200 |
| 4 | Redis（`hlp-dev-redis`） | 56379 | container | 端口可连 |
| 5 | memory-service | 50003 | process | 端口可连（需 PG DSN + Redis + embedding） |
| 6 | knowledge-service | 50002 | process | 端口可连 |
| 7 | chat-service | 50001 | process | 端口可连 |
| 8 | a2a-service（gRPC / HTTP） | 50004 / 50014 | process | 端口可连 |
| 9 | mcp-service | 50005 | process | 端口可连 |
| 10 | harness-service | 50007 | process | 端口可连 |
| 11 | agent-service（gRPC / HTTP） | 50006 / 50009 | process | 端口可连 |
| 12 | harnessloop-service | 50013 | process | 端口可连 |
| 13 | gateway | 9000 | process | `GET /health` = 200 |
| 14 | vite（cwd=`frontend/`） | 5174 | frontend | `GET http://localhost:5174/` = 200 |

收尾额外校验看板：`GET http://localhost:5174/harness-loop/monitor` = 200。

> `agent` 固定 50006（50002 是 knowledge-service）；harnessloop 的子任务执行器、
> harness-service 的 agent 指向都随 `HLP_AGENT_PORT` 一致改写。

端口全部可用环境变量覆盖：

```bash
HLP_PG_PORT=55432 HLP_MONGO_PORT=27017 HLP_QDRANT_PORT=6333 HLP_REDIS_PORT=56379 \
HLP_CHAT_PORT=50001 HLP_KNOWLEDGE_PORT=50002 HLP_MEMORY_PORT=50003 HLP_A2A_PORT=50004 \
HLP_MCP_PORT=50005 HLP_AGENT_PORT=50006 HLP_HARNESS_SVC_PORT=50007 \
HLP_HARNESSLOOP_PORT=50013 HLP_GATEWAY_PORT=9000 HLP_VITE_PORT=5174 scripts/dev-up.sh
```

## 数据隔离

本地栈默认租户 `tenant-devfull`（`HLP_TENANT` 可覆盖），memory-service 的 PG 派生索引
与 md 同步都写在该租户下：

```bash
export HARNESS_MEM_PG_DSN=postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable
export HARNESS_MEM_TENANT=tenant-devfull
```

## 运行期产物（均不入库）

`.harness/*` 已在 `.gitignore` 中忽略，全部产物落在 `.harness/dev/`：

```
.harness/dev/
  bin/    9 个服务二进制
  conf/   由 config.example.yaml + 环境变量生成的临时配置
  logs/   各组件日志、编译日志、docker compose 日志
  run/    各组件 PID 文件
```

排错先看日志：`tail -f .harness/dev/logs/gateway.log`。
`dev-up` 失败时会自动 `tail -40` 对应日志。

## 配置生成规则

配置**不落库**，每次 `dev-up` 从模板生成到 `.harness/dev/conf/`：

| 目标 | 模板 | 覆盖项 |
| --- | --- | --- |
| `conf/harnessloop.yaml` | `services/harnessloop-service/config.example.yaml` | `server.address` → `127.0.0.1:${HLP_HARNESSLOOP_PORT}`；`postgres.dsn` → 本地 PG；`agent.target` → `127.0.0.1:${HLP_AGENT_PORT}`；`policy/models/retry/feature_flags` 的 `config/harness_loop/*.yaml` → `pkg/config/harness_loop/*.yaml`；运行期再用 `HARNESSLOOP_MEMORY_ADDR` / `HARNESSLOOP_APPROVAL_ADDR` 指向本机 50003 / 50009 |
| `conf/gateway.yaml` | `services/gateway/config.example.yaml` | `server.http_port`；`services.*` 的 `*-service:port` → `127.0.0.1:port`；`services.harness_loop` → 本地 50013；`redis.url` → 本地 56379；LLM key/base/model |
| `conf/agent.yaml` | `services/agent-service/config.example.yaml` | `grpc_port` → 50006；SQLite → `.harness/dev/agent.db`；Mongo → 本地 27017；`services.mcp/harness` → 本地；LLM/embedding |
| `conf/chat.yaml` | `services/chat-service/config.example.yaml` | `grpc_port` → 50001；SQLite → `.harness/dev/chat.db`；Qdrant → 本地 6333；Redis → 本地 56379；`services.*` → 本地；LLM/embedding |
| `conf/knowledge.yaml` | `services/knowledge-service/config.example.yaml` | `grpc_port` → 50002；SQLite → `.harness/dev/knowledge.db`；Qdrant → 本地；Mongo → 本地；Redis → 本地；embedding 凭据 |
| `conf/memory.yaml` | `services/memory-service/config.example.yaml` | `grpc_port` → 50003；SQLite → `.harness/dev/memory.db`；Qdrant → 本地；Redis → 本地；embedding 凭据；PG DSN/tenant 走 `HARNESS_MEM_PG_DSN` / `HARNESS_MEM_TENANT` |
| `conf/a2a.yaml` | `services/a2a-service/config.example.yaml` | `grpc_port` → 50004、`http_port` → 50014；SQLite → `.harness/dev/a2a.db`；`services.agent` → `127.0.0.1:50006` |
| `conf/mcp.yaml` | `services/mcp-service/config.example.yaml` | `grpc_port` → 50005；`services.knowledge/gateway` → 本地；Redis → 本地；LLM |
| `conf/harness-service.yaml` | `services/harness-service/config.example.yaml` | `grpc_port` → 50007；SQLite → `.harness/dev/harness.db`；`services.agent` → 50006；Redis → 本地；LLM |

账号沿用 compose 里的默认值：`agent / agent_dev_password / agent_platform`，端口 `55432`。

前端不落配置：`dev-up` 以环境变量 `VITE_API_URL=http://localhost:${HLP_GATEWAY_PORT}` 启动 vite，
覆盖 `vite.config.ts` 里指向远端网关的 `/api` 代理，因此**不需要改前端源码**。

## 验证 Qdrant 与知识库

```bash
curl -s http://127.0.0.1:6333/healthz            # 200
curl -s http://127.0.0.1:6333/collections        # documents（knowledge）+ memories（memory）

# 真实上传一条（走 gRPC，异步索引到 ready）
go run ./cmd/harness-cli kb upload --file /tmp/probe.txt --kb-server 127.0.0.1:50002
curl -s http://127.0.0.1:6333/collections/documents   # points_count >= 1
```

## 已知坑

- **vite 只监听 localhost（`::1`）**：`curl 127.0.0.1:5174` 会连不上，请用 `localhost`。
  脚本内的探活已经统一走 `http://localhost:5174`。
- **Windows 上配置里的绝对路径**：Git Bash 的 `/d/...` 只在 bash 里有效，原生的 Go 进程读不到
  （YAML 里的路径不会被 MSYS 自动转换）。所以配置文件内部一律用**相对仓库根**的路径，
  而 `-config` / `CONFIG_PATH` 这类传给原生进程的参数由 `win_path()` 用 `cygpath -m` 转成 `D:/...`。
- **Windows 上的 PID**：Git Bash 里 `cmd &` 的 `$!` 是 MSYS 中间进程，`kill` 打不到真正的
  服务进程。脚本在 Windows 上用 `netstat -ano` 反查监听端口的真实 PID 落盘，停止时用
  `taskkill //PID ... //T //F`；PID 文件丢失时按端口兜底反查（失败回滚同理）。
- **首次请求慢**：vite 首次访问要预构建依赖（antd / monaco 等），单次请求可能数秒，
  脚本对前端探活给了 30s 单次超时、120s 总超时。
- **首次拉取 Qdrant 镜像**：compose 用 `qdrant/qdrant:latest`；网络受限时可先
  `docker pull qdrant/qdrant:latest`，或改用本机已有 tag。
- **端口冲突**：旧版 agent-service 曾占用 50002。若 `dev-up` 提示 50002/50005/50007/50013/
  9000/5174 被占用，先确认是否还有旧栈在跑，避免健康检查被别的进程“顶替”。
- **数据卷**：`pgdata` / `mongodata` / `mongoconfig` 是 external 卷，`qdrant-data` 是普通具名卷；
  `dev-down` 只 `compose down`，都不会删数据。禁止 `docker volume prune`。
- **不做的事**：不自动 `npm install`（`--install` 才装）、不改 `services/**` 源码、
  不新增 Go/npm 依赖。

## 复用与扩展

`scripts/dev-lib.sh` 是公共库（端口常量、`service_port_of` / `port_open` / `listener_pid` /
`stop_pid` / `wait_until` / 日志与失败处理），新增服务时照 `start_grpc()` 的形状加即可，
并同步 `scripts/dev-stack.json` 与 `dev-down.sh` 的 `SERVICES`。