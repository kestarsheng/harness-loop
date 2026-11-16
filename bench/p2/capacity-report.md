# P2 容量与资源报告（T-P2-08）

> 口径：本报告只把「实际命令跑出来的数」标为 **实测**；其余为 design 10.6
> 的预算推算，标为 **推算**。两者不混写。

## 1. 采集环境（实测）

| 项 | 值 |
|---|---|
| OS / CPU | Windows 11 / Intel Core Ultra 5 125H（18 逻辑核） |
| Go | go1.22（本机 GOROOT） |
| PostgreSQL | 容器 `hlp-pg-test`，`127.0.0.1:55432`，含 pgvector |
| 度量方式 | `go test`（非生产负载） |

未做真实 20 并发任务的端到端压测；下表第 3 节的并发数字均为**推算**。

## 2. 实测项

| 测量 | 命令 | 结果 |
|---|---|---|
| 空库全量迁移（12 份 DDL + 重放幂等） | `HARNESS_TEST_PG_DSN=... go test ./pkg/pgindex/ -run TestMigrateAll -v` | 12 migrations applied once，重放无变更，1.55s |
| 迁移幂等（存量库） | 同上 `TestMigrateIsIdempotent` | PASS，0.21s |
| `/metrics` 渲染（33 指标、约 200 系列） | `go test ./services/harnessloop-service/internal/metrics -bench BenchmarkRender -benchtime 200x` | **1.52 ms/op，418 KB/op，8784 allocs/op** |
| harnessloop 单测（PG 真实库） | `go test ./services/harnessloop-service/... -count=1` | 全绿（config/handler/metrics/repository/service/watchdog） |
| gateway 单测 | `go test ./services/gateway/... -count=1` | 全绿（handler/middleware/router） |

结论（实测范围）：指标端点每次抓取的 CPU 与内存开销可忽略（按 15s 抓取间隔，
占用 < 0.01% CPU）；迁移耗时与数据量无关（仅 12 份 DDL），存量库重放无写放大。

## 3. 容量预算（推算，来自 design 10.6）

| 资源 | 单实例建议 | 依据 | 状态 |
|---|---|---|---|
| harnessloop-service | 4C / 8G | 20 并发任务 × 单任务 ≤256MB = 5GB，留 3G 余量 | 推算 |
| codeindex | 2C / 4G | 解析 + embedding 外呼，内存给 AST | 推算 |
| postgres | 8C / 32G / 200GB | 向量 + 事件 + 任务；`shared_buffers=8GB` | 推算 |
| redis | 1C / 2G | 事件扇出 + 缓存 | 推算 |
| mongo | 4C / 8G | 检查点与文档 | 推算 |

单任务内存预算：消息历史 ≤50MB + 压缩缓冲 ≤30MB + 检查点序列化 ≤20MB +
运行时 ≤50MB，合计 ≤150MB，为 256MB 上限留 40% 余量（推算）。

## 4. 指标基数（实测结构 + 推算规模）

`/metrics` 预注册 33 个 DFX-M-01 指标，展开约 200 条系列（stage×attempt、
role×status 等固定枚举）。按 design 的标签取值封闭集，基数不随任务数增长：

- 固定枚举展开：约 200 条（实测，`BenchmarkRender` 输出规模）。
- 若未来把 `task_id` / `parent_task_id` 引入标签，单租户 1k 任务即 1k+ 系列，
  会突破 Prometheus 单目标 10k 系列的合理上限（推算，未发生：当前实现只用
  固定枚举与 `tenant_id`/`project_id`，后者建议不超过租户数×项目数）。

## 5. 未覆盖项（如实声明）

1. **未做 20 并发真实任务压测**：runtime 压测需要真实 agent-service + LLM，
   本轮只跑桩（`stubStages`），CPU/RSS 数字不能代表生产。
2. **compaction 内存峰值未实测**：`CompactionLimits` 只是配置上限，没有任何
   一次真实压缩的内存采样。
3. **Redis / Mongo 容量**沿用 design 表格，本轮没有压测数据。
4. **事件积压**（`hlp_event_lag_seconds`）已注册并可在 `/metrics` 抓取，但
   runtime 侧目前没有打点（发布路径未测量 produced→queryable 延迟），抓到的
   值为 0，不代表真实滞后为 0。
5. **索引队列/检索时延**属于 codeindex/knowledge 服务，本轮只在
   harnessloop-service 预注册了指标名与标签，未在对应服务打点。

## 6. 复现命令

```bash
# 迁移（空库全量 + 幂等）
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
  HARNESS_REQUIRE_PG=1 go test ./pkg/pgindex/ -run 'TestMigrate' -v -count=1

# 指标渲染开销
go test ./services/harnessloop-service/internal/metrics/ -run XXX -bench BenchmarkRender -benchtime 200x

# 服务全量
HARNESS_TEST_PG_DSN='...' HARNESS_REQUIRE_PG=1 go test ./services/harnessloop-service/... ./services/gateway/... -count=1
```