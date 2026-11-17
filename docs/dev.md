# 本地开发栈：dev-up / dev-down

只跑「数据层 + harnessloop + gateway + 前端」的最小闭环，用于本地联调与看板验证。
不引入 compose 全量栈，也不改动任何服务的代码或配置模板。

## 前置

| 依赖 | 说明 |
| --- | --- |
| Docker | 用于 `docker/docker-compose.data.yaml`（PostgreSQL 55432 / MongoDB 27017） |
| Go 1.22+ | 编译 `harnessloop-service`、`gateway` |
| Node 20+ / npm | 前端 vite 开发服务器；首次需 `cd frontend && npm ci` |
| bash | Windows 用 Git Bash（脚本已适配 MSYS）；也可用 `scripts/dev-up.ps1` 包装 |

## 用法

```bash
scripts/dev-up.sh                 # 依次拉起 4 项，末尾打印看板地址与 PID
scripts/dev-up.sh --install       # node_modules 缺失时自动 npm ci
scripts/dev-up.sh --rebuild       # 强制重新编译 Go 服务
scripts/dev-up.sh --no-build      # 跳过编译，直接用已有二进制
scripts/dev-up.sh --demo          # 演示：打开三条 P2 新链路开关（编排/模型策略/权限层）
HLP_DEMO_FLAGS=1 scripts/dev-up.sh # 同上，环境变量写法

scripts/dev-down.sh               # 按 PID 停业务进程，然后停数据容器
scripts/dev-down.sh --keep-db     # 只停业务进程，保留 PG / Mongo
```

退出码：`0` 全部就绪；非 `0` 表示某一步失败，失败时脚本会打印该步日志尾部，并把本次
已启动的进程回滚掉（不会留下半截栈）。

### `--demo`：一键看到新链路

`--demo` 把 harnessloop-service 的 `feature_flags.file` 指向
`scripts/dev-features.demo.yaml`（三条开关 `default: true`），仓库自己的
`pkg/config/harness_loop/features.yaml` 保持默认全关，不被改写。开关只在进程启动时
读一次，所以已经跑着栈的话要先 `scripts/dev-down.sh` 再 `--demo` 起。

开关没有 HTTP 开关接口，验证方式是看启动日志与看板：

```bash
grep 'feature gates' .harness/dev/logs/harnessloop.log
# harnessloop: feature gates (tenant=platform): orchestrator=on model_strategy=on policy_layers=on
```

随后看板的「多 Agent / 模型 / 权限」三块从空态变为有数据；不给 `--demo` 时这行日志是
三个 `off`，行为回到单 Agent + 静态模型 + 旧权限判定。

## 步骤与端口

| # | 组件 | 端口 | 就绪判据 |
| --- | --- | --- | --- |
| 1 | PostgreSQL（容器 `hlp-pg-test`，库 `agent_platform`） | 55432 | `pg_isready`（容器内执行，宿主无 pg_isready 也能用） |
| 2 | harnessloop-service（gRPC） | 50013 | 端口可连 |
| 3 | gateway | 9000 | `GET /health` = 200 |
| 4 | vite（cwd=`frontend/`） | 5174 | `GET http://localhost:5174/` = 200 |

收尾会额外校验看板：`GET http://localhost:5174/harness-loop/monitor` = 200。

端口全部可用环境变量覆盖：

```bash
HLP_PG_PORT=55432 HLP_HARNESSLOOP_PORT=50013 HLP_GATEWAY_PORT=9000 HLP_VITE_PORT=5174 scripts/dev-up.sh
```

## 运行期产物（均不入库）

`.harness/*` 已在 `.gitignore` 中忽略，全部产物落在 `.harness/dev/`：

```
.harness/dev/
  bin/    harnessloop-service / gateway 二进制
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
| `conf/harnessloop.yaml` | `services/harnessloop-service/config.example.yaml` | `server.address` → `127.0.0.1:${HLP_HARNESSLOOP_PORT}`；`postgres.dsn` → `postgres://agent:...@127.0.0.1:${HLP_PG_PORT}/agent_platform`；`policy/models/retry/feature_flags` 的 `config/harness_loop/*.yaml` → `pkg/config/harness_loop/*.yaml`（模板里的目录在仓库中不存在，不改写会全部 fallback 到默认值）；`memory.addr` 与 `approval.addr` 置空（避免本机没起 memory/agent 服务时的无谓重试） |
| `conf/gateway.yaml` | `services/gateway/config.example.yaml` | `server.http_port` → `${HLP_GATEWAY_PORT}`；`services.*` 的 `*-service:port` 一律改写成 `127.0.0.1:port`；`services.harness_loop` → `127.0.0.1:${HLP_HARNESSLOOP_PORT}`（看板接口要打到本地 harnessloop）；`redis.url` 置空（无 Redis 时不起限流器） |

账号沿用 compose 里的默认值：`agent / agent_dev_password / agent_platform`，端口 `55432`。

前端不落配置：`dev-up` 以环境变量 `VITE_API_URL=http://localhost:${HLP_GATEWAY_PORT}` 启动 vite，
覆盖 `vite.config.ts` 里指向远端网关的 `/api` 代理，因此**不需要改前端源码**。

## 已知坑

- **vite 只监听 localhost（`::1`）**：`curl 127.0.0.1:5174` 会连不上，请用 `localhost`。
  脚本内的探活已经统一走 `http://localhost:5174`。
- **Windows 上配置里的绝对路径**：Git Bash 的 `/d/...` 只在 bash 里有效，原生的 Go 进程读不到
  （YAML 里的路径不会被 MSYS 自动转换）。所以配置文件内部一律用**相对仓库根**的路径，
  而 `-config` / `CONFIG_PATH` 这类传给原生进程的参数由 `win_path()` 用 `cygpath -m` 转成 `D:/...`。
- **Windows 上的 PID**：Git Bash 里 `cmd &` 的 `$!` 是 MSYS 中间进程，`kill` 打不到真正的
  服务进程。脚本在 Windows 上用 `netstat -ano` 反查监听端口的真实 PID 落盘，停止时用
  `taskkill //PID ... //T //F`；PID 文件丢失时 `dev-down` 会按端口兜底反查。
- **首次请求慢**：vite 首次访问要预构建依赖（antd / monaco 等），单次请求可能数秒，
  脚本对前端探活给了 30s 单次超时、120s 总超时。
- **数据卷**：compose 里的 `pgdata` / `mongodata` 是 external 卷，`dev-down` 只
  `compose down`，不会删数据。禁止 `docker volume prune`。
- **不做的事**：不自动 `npm install`（`--install` 才装）、不改 `services/**` 源码、
  不新增 Go/npm 依赖。

## 复用与扩展

`scripts/dev-lib.sh` 是公共库（端口常量、`port_open` / `listener_pid` / `stop_pid` /
`wait_until` / 日志与失败处理），新增服务时照 `step_xxx()` 的形状加即可。
