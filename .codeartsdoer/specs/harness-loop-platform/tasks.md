# Harness Loop 任务循环平台 — 编码任务规划（tasks.md）

| 项 | 内容 |
|---|---|
| 文档编号 | TASKS-HLP-001 |
| 版本 | v1.0（2026-11-10） |
| 状态 | 待评审 |
| 上游文档 | `spec.md` v1.1（170 条 REQ + 38 条 DFX）、`design.md` v1.2（12 章，5 项锁定决策） |
| 代码基线 | `agent-platform` @ `4a05ccb`，Go 1.22 |
| 任务规模 | 107 个任务（P0 27 / P1 36 / P2 44） |
| 已锁定决策 | A 方案记忆事实源、PG 仅承载索引与事件（D2）、RAG 复用 Go knowledge-service（D3）、runtime 复用 `pkg/llm` + cobra/Bubble Tea（D4）、新增 :50008（D6/D14）、自研模板摘要唯一通道（D13）、多 Agent 8/20/20/2（U13-A）、审批一期留 agent-service（U17-A）、阶段模型默认继承（U18-A）、子任务验收复用父验收器（U19-A） |

> 阅读约定：每个任务给出「目标 → 关联需求 → 设计章节 → 依赖 → 文件 → 实施要点 → 验收标准 → 复杂度」。`[N]` 表示新增文件，`[M]` 表示改造存量文件。验收标准优先给出可直接执行的命令或断言。本文件不含实现代码。

---

# 1. 概述

## 1.1 目标

把 `spec.md` 的 170 条需求与 `design.md` 的技术设计，切分为可独立领取、可独立验证、依赖关系明确的编码任务，使团队（或 Agent）能按 P0 → P1 → P2 顺序交付，并在每个阶段结束时通过 `design` 8.4 的验收门槛。

## 1.2 范围

**在范围内**

1. 新增 `pkg/harnessloop/*`（FSM、验收、重试、检查点、压缩、编排、子任务、handoff、模型注册表、权限、事件）与 `pkg/memoryfile`、`pkg/pgindex`、`pkg/codeindex`、`pkg/rrf`、`pkg/harnessclient`。
2. 新增 `services/harnessloop-service`（:50008）与 `cmd/harness-cli` + `internal/cli`。
3. 改造 `services/{memory-service,knowledge-service,gateway,agent-service,mcp-service,harness-service}` 的指定文件。
4. 新增 `proto/harnessloop`、`proto/codeindex`；追加 `proto/{memory,knowledge,mcp}` 字段。
5. 新增前端 `frontend/src/pages/HarnessLoop/*` 与两条路由。
6. 新增 `docker/postgres/*`、`bench/*`、PG 迁移脚本与配置快照目录 `pkg/config/harness_loop/`。

**不在范围内**：模型训练/微调、IDE 能力、IAM 与计费、Qdrant/MongoDB 全量下线与迁移专项、多模态索引、AutoML/RL 策略寻优、分布式多活、前端设计系统重构、外部压缩引擎选型与接入（spec 第 7 章）。

## 1.3 前置条件

| 项 | 要求 | 校验方式 |
|---|---|---|
| 代码基线 | 分支基于 `4a05ccb`，工作区干净 | `git log -1 --format=%h` |
| Go 工具链 | Go 1.22（`go.mod` 已声明） | `go version` |
| proto 工具链 | `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` | `make proto-harnessloop` |
| 数据库 | PostgreSQL 16 + `pgvector` 扩展；MongoDB；Redis | `docker compose --profile test up -d` |
| 测试依赖 | `testcontainers-go`（或 compose profile `test`） | `go test ./pkg/pgindex/...` |
| 权限 | 本机可创建绝对路径工作区，且可执行 `go test` / `go build` | `harness-cli doctor`（P0 后期） |

## 1.4 执行约定

1. **任务编号**：`T-<阶段>-<序号>`，测试任务为 `T-<阶段>-T<序号>`；序号在阶段内唯一且不复用。
2. **完成定义（DoD）**：代码合入 + 任务声明的验收命令全绿 + 涉及需求的事件/错误码可在测试中断言 + 改动文件数 ≤ 8（超出需拆分）。
3. **提交规范**：Conventional Commits（`feat(harnessloop/fsm): ...`、`chore(proto): ...`），一个任务一次提交或一组紧密相关提交。
4. **配置外置**：所有阈值、名单、矩阵放 `pkg/config/harness_loop/*.yaml`，加载为运行期只读快照；业务代码中禁止出现窗口/超时/名单硬编码（DFX-M-05）。
5. **禁止项优先**：REQ-010/019/141/168/169 的负向用例与功能同批交付，不允许"先放行后补测试"。
6. **不写实现代码**：本文件只描述做什么与怎么判定完成。

---

# 2. 里程碑与阶段划分

## 2.1 阶段目标与出口门槛

| 阶段 | 目标 | 出口门槛（对齐 design 8.4） | 可演示能力 |
|---|---|---|---|
| **P0** | FSM 闭环最小可演示，不依赖记忆/索引 | FSM 穷举、终态、中止幂等、检查点恢复、事件幂等全绿；权限沙箱负向集（`../`、绝对路径、符号链接、复合命令、自我提权）全拒；模型能力校验集全绿 | `harness-cli run` → 五阶段推进 → 失败回退 → 中止幂等 → 事件可查 → 越权工具被拒并留痕 |
| **P1** | 记忆、压缩、索引、TUI、大屏、知识增强、多 Agent、模型策略、权限完整版 | 记忆 LRU/原子写、压缩保真、索引增量、检索指标、CLI E2E 全绿；多 Agent 并发/失败传播/handoff/汇聚全绿；模型三策略与回退全绿；权限审计一致性全绿 | 长时任务带记忆注入与压缩；代码可检索；TUI 与大屏可见；父子任务并行并汇聚 |
| **P2** | 多语言索引、rerank 增强、调度优化、运维完善 | 三语言解析准确率 ≥ 90%；知识检索评测通过；50 卡片压测达标；故障注入无数据损坏；公平调度与逃逸模糊测试留档 | 多语言仓库索引、HTTP rerank、WFQ 公平调度、完整指标告警 |

## 2.2 与 design 8.3 切片的映射

| design 切片 | 任务 ID | 说明 |
|---|---|---|
| P0-1 FSM 内核 | T-P0-04 | 8 条合法边 + 非法边拒绝 |
| P0-2 任务状态与 attempt | T-P0-03、T-P0-05、T-P0-07 | DDL + attempt + 乐观锁 |
| P0-3 阶段内执行适配 | T-P0-06、T-P0-16 | `stage_mode` + `ExecuteStream` |
| P0-4 验收 Agent（程序检查优先） | T-P0-10、T-P0-11、T-P0-12 | 干净上下文 + checks + verdict |
| P0-5 回退与三类终态 | T-P0-12、T-P0-17 | rollback + terminal |
| P0-6 检查点与恢复 | T-P0-15 | Mongo + 幂等 + drift |
| P0-7 事件与查询 | T-P0-08、T-P0-09 | schema + seq + ListEvents |
| P0-8 网关与 CLI 最小闭环 | T-P0-21、T-P0-22 | 路由 + run/status/abort/resume |
| P0-9 开关与回滚 | T-P0-23 | feature flag + `E_FSM_DISABLED` |
| P0-10 权限与沙箱最小闭环 | T-P0-19 | 矩阵 + 路径/命令白名单 |
| P0-11 模型注册表与能力校验 | T-P0-20 | 注册表 + 能力匹配 |
| P1-1 markdown 事实源 | T-P1-01、T-P1-02、T-P1-03 | 布局/格式 + LRU + 原子写 |
| P1-2 记忆索引同步 | T-P1-04 | `mdsync` + `hlp_mem` |
| P1-3 记忆混合召回与注入 | T-P1-05、T-P1-06 | 全文 + 按需向量 + 15% 预算 |
| P1-4 上下文压缩（C 方案） | T-P1-07、T-P1-08、T-P1-09 | 模板摘要 + JSONL + 三级降级 |
| P1-5 异常分类与重试（全量） | T-P0-13、T-P0-14、T-P1-T5 | P0 提前落地骨架，P1 补回归 |
| P1-6 代码索引（Go） | T-P1-11、T-P1-12、T-P1-13 | watcher + AST + 单事务 |
| P1-7 CLI TUI | T-P1-27 | Bubble Tea |
| P1-8 大屏（基础视图） | T-P1-28 | SSE + 卡片 + 泳道 + 健康度 |
| P1-9 知识广场增强 | T-P1-15、T-P1-16 | 状态机 + 真实 BM25 + 引用 |
| P1-10 多 Agent 协作（核心） | T-P1-17 ~ T-P1-22 | DAG + 调度 + handoff + 汇聚 |
| P1-11 模型策略与回退 | T-P1-23、T-P1-24 | 三策略 + 回退链 |
| P1-12 审批联动与权限审计聚合 | T-P1-25、T-P1-26 | 审批代理 + `rule_id` 聚合 |
| P1-13（新增）PG 迁移完整 | T-P1-30 | 四份 DDL + 双写矩阵 |
| P2-1 TS/Python 解析 | T-P2-01 | tree-sitter + 正则降级 |
| P2-2 图扩展召回 | T-P2-02 | 第三路 + 来源列表 |
| P2-3 全量索引与性能 | T-P2-03 | `index --full` + 压测 |
| P2-4 Reranker 增强 | T-P2-04 | HTTP cross-encoder |
| P2-5 压缩引擎适配（二期可选） | T-P2-05 | `ContextEngine` 适配器 |
| P2-6 知识索引异步化 | T-P2-06 | worker + 四态 + 重试 |
| P2-7 审计与严格租户 | T-P2-07 | `audit` + `StrictTenant` + 二次确认 |
| P2-8 运维 | T-P2-08 | 指标/告警/trace 桥接 |
| P2-9 公平调度与熔断调优 | T-P2-09 | WFQ + 硬上限 |
| P2-10 多 Agent 高级场景 | T-P2-10 | 跨阶段 handoff + 嵌套 + 重规划 |

## 2.3 阶段依赖与放行规则

1. **P0 内部**：基础设施（T-P0-01/02/03）必须先合入；FSM 内核（T-P0-04）与事件线（T-P0-08/09）可并行；`runtime`（T-P0-17）是 P0 唯一的汇聚点，未合入前不得启动网关/CLI 联调。
2. **P0 → P1**：P0 出口门槛全绿才进入 P1；权限（T-P0-19）与模型（T-P0-20）必须在 P1 记忆/索引开工前可用，否则 P1 的 c 阶段执行不可控。
3. **P1 内部**：记忆线、索引线、压缩线三者互不依赖，可并行；多 Agent 线依赖 `subtask`（T-P1-17）与权限角色联动（T-P1-22）。
4. **P1 → P2**：P1 出口门槛全绿；P2 的 HTTP rerank 与 WFQ 以 P1 的检索/调度基线为对照。

---

# 3. 任务清单

## 3.1 P0：FSM 闭环最小可演示（27 个任务）

### T-P0-01 工程骨架与代码生成流水线

| 字段 | 内容 |
|---|---|
| 目标 | 建立 `pkg/harnessloop/*` 等新包骨架、proto 生成目标与依赖边界校验，使后续任务有稳定的编译与测试基线。 |
| 关联需求 | REQ-096、DFX-C-01、DFX-M-05；design 8.1 / 8.2 |
| 依赖 | 无 |
| 涉及文件 | [N] `pkg/harnessloop/{fsm,acceptance,retry,checkpoint,event}/`（目录占位）、[N] `pkg/config/harness_loop/`（目录）、[N] `scripts/depcheck/main.go`、[N] `scripts/depcheck/main_test.go`、[M] `Makefile` |
| 实施要点 | 1) 按 8.1 建目录，暂以最小可编译空包占位；2) Makefile 增 `proto-harnessloop`、`proto-codeindex`、`build-cli`、`depcheck` 目标；3) `depcheck` 用 `go/packages` 实现 8.2 的 9 条规则（含 `policy` 不 import `pkg/llm`、`compaction` 不 import 外部压缩引擎）；4) 新增依赖仅允许 cobra、bubbletea、pgx、fsnotify、tree-sitter，写入 `go.mod` 前先评审；5) 配置目录约定为运行期只读快照。 |
| 验收标准 | `go build ./...` 通过；`go run ./scripts/depcheck` 对当前仓库返回 0 违规；`go test ./scripts/depcheck/...` 全绿（含 1 条故意违规的负样本）。 |
| 复杂度 | M |

### T-P0-02 proto/harnessloop 契约（P0 子集）

| 字段 | 内容 |
|---|---|
| 目标 | 定义 FSM/验收/检查点/事件/模型的 gRPC 契约与共享消息，供服务、Gateway、CLI 共用一份事实。 |
| 关联需求 | REQ-103、REQ-119、REQ-142；design 6.1 / 6.2 / 4.12.1 |
| 依赖 | T-P0-01 |
| 涉及文件 | [N] `proto/harnessloop/harnessloop.proto`、[N] `pkg/pb/harnessloop/`（生成物）、[M] `Makefile` |
| 实施要点 | 1) 先落 `CreateTask/GetTask/ListTasks/PauseTask/ResumeTask/AbortTask/ListEvents/WatchEvents/ListCheckpoints/RestoreFromCheckpoint/ListModels/ResolveModel/PreflightTool`；2) `HarnessEvent` 字段与 4.12.1 表逐项一致，`parent_task_id/subtask_id/handoff_id/dag_id` 预留字段号；3) `Budget` 含 `wall_clock_seconds`；4) `Criterion.type` 覆盖 `tests_pass/build_pass/lint_pass/artifact_exists/custom`；5) 只追加不改号，`reserved` 标注废弃位。 |
| 验收标准 | `make proto-harnessloop` 无 warning；生成包 `go build ./pkg/pb/harnessloop/...` 通过；用脚本比对 proto 字段清单与 4.12.1 必填表，差异为空。 |
| 复杂度 | M |

### T-P0-03 PG 基础与 P0 表结构

| 字段 | 内容 |
|---|---|
| 目标 | 落地 PostgreSQL 连接池、迁移框架、advisory lock 与 `hlp_task`/`hlp_event` 最小表，支撑状态与事件持久化。 |
| 关联需求 | REQ-009、REQ-103、REQ-106、DFX-R-05；design 5.1 / 5.3 |
| 依赖 | T-P0-01 |
| 涉及文件 | [N] `pkg/pgindex/conn.go`、[N] `pkg/pgindex/migrate.go`、[N] `pkg/pgindex/advisory.go`、[N] `pkg/pgindex/ddl/004_task.sql`、[N] `pkg/pgindex/ddl/003_event.sql`、[N] `docker/postgres/init.sql`、[M] `docker/docker-compose.yaml` |
| 实施要点 | 1) pgx 连接池 + 启动健康检查 + 慢查询日志；2) `migrate.go` 以 `schema_migrations` 记录版本，幂等可重放；3) 建 `task_runs/stage_attempts/acceptance_records`（含 `version` 乐观锁、`policy_snapshot`）；4) 建 `events/tool_idempotency/audit`，`events` 在 `(task_id, seq)` 唯一、`event_id` 唯一；5) 封装 `pg_advisory_xact_lock(hashtext(task_id))`；6) compose 增 `postgres`（pgvector 镜像）与 `hlp-workspace` 卷。 |
| 验收标准 | `go test ./pkg/pgindex/...` 全绿（testcontainers 或 profile `test`）；迁移连续执行两次无错误、无重复表；表字段与 5.1.1 的 DDL 逐项比对一致。 |
| 复杂度 | M |

### T-P0-04 FSM 状态集与转移表

| 字段 | 内容 |
|---|---|
| 目标 | 实现五阶段唯一状态集、8 条合法边与非法边拒绝，纯函数可穷举测试，定义编译进二进制。 |
| 关联需求 | REQ-001、REQ-002、REQ-003、REQ-005、REQ-010；design 4.1.1 / 4.1.2 / 9.2 |
| 依赖 | T-P0-01 |
| 涉及文件 | [N] `pkg/harnessloop/fsm/machine.go`、[N] `pkg/harnessloop/fsm/transitions.go`、[N] `pkg/harnessloop/fsm/terminal.go`、[N] `pkg/harnessloop/fsm/machine_test.go` |
| 实施要点 | 1) `Stage` 仅 a~e，其他值返 `E_INVALID_STAGE` 且零状态变更；2) 转移表为编译期常量（T1~T8），无运行时注册入口；3) 白名单外返 `E_ILLEGAL_TRANSITION`，状态不变并产 `stage.illegal_transition`（severity=error）；4) `e` 无出边；5) 跳过阶段（a→e、b→e、c→e）显式拒绝；6) 每次合法转移返回「待发布事件序列」描述，由上层落库。 |
| 验收标准 | `go test ./pkg/harnessloop/fsm/... -run TestTransitions` 覆盖 8 条合法边 + 全部非法组合；`go test -race ./pkg/harnessloop/fsm/...` 通过；包覆盖率 ≥ 90%。 |
| 复杂度 | M |

### T-P0-05 attempt 生命周期

| 字段 | 内容 |
|---|---|
| 目标 | 阶段进入即建档，`attempt_no` 连续递增，`entry_reason` 四值可追溯。 |
| 关联需求 | REQ-004；design 4.1.1 / 5.1.1、spec 6.2 |
| 依赖 | T-P0-04 |
| 涉及文件 | [N] `pkg/harnessloop/fsm/attempt.go`、[N] `services/harnessloop-service/internal/repository/attempt_repo.go`、[N] `pkg/harnessloop/fsm/attempt_test.go` |
| 实施要点 | 1) 进入非终态阶段立即创建 attempt 并产 `stage.entered`；2) `attempt_no` 按 `(task_id, stage)` 从 1 连续递增；3) `entry_reason` 取 `initial/acceptance_failed/retry/resume`；4) `e` 阶段不建档；5) `finish_received=false` 时 `acceptance_id` 必须为空。 |
| 验收标准 | `go test ./pkg/harnessloop/fsm/... -run TestAttempt`；DB 断言重复插入同一 `(task_id,stage,attempt_no)` 报唯一约束冲突；c 阶段回退后重进 `attempt_no=2`。 |
| 复杂度 | S |

### T-P0-06 stage_finish 与 guard

| 字段 | 内容 |
|---|---|
| 目标 | `finish` 只能由结构化工具调用触发，且必须携带 `attempt_id`+`stage` 并通过 guard 校验；文本式"已完成"无效。 |
| 关联需求 | REQ-006、REQ-010；design 4.1.1 / 4.1.6 |
| 依赖 | T-P0-04、T-P0-05 |
| 涉及文件 | [N] `pkg/harnessloop/fsm/guard.go`、[N] `pkg/harnessloop/fsm/guard_test.go`、[M] `services/agent-service/internal/service/agent_service.go` |
| 实施要点 | 1) 定义 `stage_finish` 工具 schema（`attempt_id/stage/summary/artifacts`）；2) 自然语言"已完成"、消息结束、流关闭一律不触发验收；3) guard 校验 `attempt_id`+`stage` 与当前状态一致，不符则丢弃并记 `invalid_finish`；4) 同 attempt 重复 finish 幂等；5) agent-service 透传 `__stage_mode__`，该模式下"无工具调用"返回 `ErrStageNotFinished` 而非 `MarkCompleted()`。 |
| 验收标准 | `go test ./pkg/harnessloop/fsm/... -run TestFinishGuard`；agent-service 单测断言 stage_mode 下无 finish 不产生完成标记；伪造 `attempt_id` 的 finish 被丢弃且产 `invalid_finish`。 |
| 复杂度 | M |

### T-P0-07 任务仓储与乐观锁

| 字段 | 内容 |
|---|---|
| 目标 | TaskRun 快照持久化与并发转移串行化，保证并发下同一转移仅一次成功、崩溃后可重建。 |
| 关联需求 | REQ-009、DFX-R-05；design 4.1.3 / 5.1.1 |
| 依赖 | T-P0-03、T-P0-04 |
| 涉及文件 | [N] `services/harnessloop-service/internal/repository/task_repo.go`、[N] `services/harnessloop-service/internal/repository/task_repo_test.go` |
| 实施要点 | 1) `version` 乐观锁 CAS 更新，冲突返回可重试错误；2) 转移顺序固定为「先写事件（含预期终态）→ 写快照 → 执行副作用」；3) 崩溃后按 `version` 与事件水位比对，重放或丢弃；4) 所有查询强制注入 `tenant_id/project_id` 过滤；5) 提供 abort 行锁入口。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/repository/... -run TestTransferCAS`：10 goroutine 同边并发仅 1 成功、其余幂等返回；跨租户查询返回空。 |
| 复杂度 | M |

### T-P0-08 事件 schema 与序号

| 字段 | 内容 |
|---|---|
| 目标 | 定义 HarnessEvent 结构、事件类型字典、ULID 与任务内单调 `seq`，生产端强校验。 |
| 关联需求 | REQ-103、REQ-104、REQ-105、DFX-M-04；design 4.12.1 / 4.12.2 / 4.12.3 |
| 依赖 | T-P0-02 |
| 涉及文件 | [N] `pkg/harnessloop/event/schema.go`、[N] `pkg/harnessloop/event/types.go`、[N] `pkg/harnessloop/event/seq.go`、[N] `pkg/harnessloop/event/idempotency.go`、[N] `pkg/harnessloop/event/schema_test.go` |
| 实施要点 | 1) 必填字段校验，缺失拒绝发送并产 `event.invalid`；2) 事件字典覆盖 task/stage/acceptance/retry/checkpoint/compaction/memory/index/tool/error/security；3) `seq` 按 task 单调，恢复时从 `event_seq_watermark` 续号；4) `event_id` 幂等键接口（落 `tool_idempotency` 或事件表唯一约束）；5) `payload` ≤ 8KB，禁密钥/完整提示词/完整文件内容。 |
| 验收标准 | `go test ./pkg/harnessloop/event/...`：缺 `schema_version` 被拒；重复 `event_id` 三次不重复计数；`seq` 回退被拒；超限 payload 被拒并告警。 |
| 复杂度 | M |

### T-P0-09 事件持久化与查询

| 字段 | 内容 |
|---|---|
| 目标 | 事件落 PG，支持按 task/stage/type/severity/since_seq 查询与保留期清理。 |
| 关联需求 | REQ-106；design 4.12.4 / 5.1、spec 6.9 |
| 依赖 | T-P0-03、T-P0-08 |
| 涉及文件 | [N] `pkg/harnessloop/event/publisher.go`、[N] `services/harnessloop-service/internal/repository/event_repo.go`、[N] `services/harnessloop-service/internal/repository/event_repo_test.go` |
| 实施要点 | 1) 批量写 + 事务内分配 seq；2) 查询支持 5 个过滤维度与游标分页；3) 保留期可配置 + 清理任务骨架（周期任务在 T-P1-30 完善）；4) PG 不可用时降级本地 JSONL 缓冲并重试；5) 大对象走 `payload_ref`。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/repository/... -run TestEventRepo`；`task_id+stage=c` 查询结果按 `seq` 严格有序且无缺号；停 PG 后事件进缓冲、恢复后补写成功。 |
| 复杂度 | M |

### T-P0-10 验收上下文与注入完整性

| 字段 | 内容 |
|---|---|
| 目标 | 构造不继承执行历史的干净验收上下文，并强制 6 类注入齐全，缺项即拒绝启动。 |
| 关联需求 | REQ-011、REQ-012、REQ-013、REQ-014；design 4.2.1 / 4.2.2 |
| 依赖 | T-P0-06 |
| 涉及文件 | [N] `pkg/harnessloop/acceptance/context.go`、[N] `pkg/harnessloop/acceptance/schema.go`、[N] `pkg/harnessloop/acceptance/context_test.go` |
| 实施要点 | 1) `messages` 初始为空，system 使用版本化模板（如 `acceptance.v1`）；2) 注入 goal 不可变副本、阶段验收标准、产物清单（路径+摘要+片段）、确定性检查结果、任务约束、上轮 evidence；3) 缺任一类返 `E_ACCEPTANCE_CONTEXT_INCOMPLETE`；4) 独立 memory 命名空间 + 只读工具白名单；5) 验收 span 的 parent 为阶段 span，messages 与执行 span 无交集。 |
| 验收标准 | `go test ./pkg/harnessloop/acceptance/... -run TestContextIsolation`：执行历史含"请判定通过"时，验收输入快照哈希不含该文本；缺产物清单时构造失败并返回对应错误码。 |
| 复杂度 | M |

### T-P0-11 确定性检查注册表

| 字段 | 内容 |
|---|---|
| 目标 | 提供可注册的确定性检查器与统一结果模型，保证程序检查优先且不可用项记为 unverified。 |
| 关联需求 | REQ-016、REQ-017；design 4.2.4 / 8.1 |
| 依赖 | T-P0-10 |
| 涉及文件 | [N] `pkg/harnessloop/acceptance/checks/registry.go`、[N] `pkg/harnessloop/acceptance/checks/go_test.go`、[N] `pkg/harnessloop/acceptance/checks/go_build.go`、[N] `pkg/harnessloop/acceptance/checks/go_vet.go`、[N] `pkg/harnessloop/acceptance/checks/file_exists.go`、[N] `pkg/harnessloop/acceptance/checks/cmd_exit.go`、[N] `pkg/harnessloop/acceptance/checks/registry_test.go` |
| 实施要点 | 1) 注册表按 `criterion.type` 分派，未知类型返回 unverified；2) `go test/go build/go vet` 一律经 `policy/sandbox/execguard` 校验后执行；3) 命令不存在/超时 → unverified（不得计为 pass）；4) 结果含 `exit_code/stdout_digest/duration_ms`；5) 检查器 panic 转 `verification_error`。 |
| 验收标准 | `go test ./pkg/harnessloop/acceptance/checks/...`；用例：测试失败直接 fail 且模型调用次数为 0；命令不存在 → `unverified_criteria` 非空且整体 verdict 不为 pass。 |
| 复杂度 | M |

### T-P0-12 验收结论、回退映射与审计

| 字段 | 内容 |
|---|---|
| 目标 | 产出结构化 verdict，按规则表映射回退目标，落验收审计记录，验收 Agent 只读。 |
| 关联需求 | REQ-015、REQ-018、REQ-019、REQ-020；design 4.2.3 / 4.2.5 / 4.2.6 |
| 依赖 | T-P0-11 |
| 涉及文件 | [N] `pkg/harnessloop/acceptance/verifier.go`、[N] `pkg/harnessloop/acceptance/rollback.go`、[N] `pkg/harnessloop/acceptance/audit.go`、[N] `services/harnessloop-service/internal/repository/acceptance_repo.go`、[N] `pkg/harnessloop/acceptance/verifier_test.go` |
| 实施要点 | 1) verdict 必含 `verdict/reason/evidence/failed_criteria/suggested_rollback_target/confidence/duration_ms`；2) `pass` 但 evidence 为空判 invalid→fail；3) 回退默认 a→a、b→a、c→b、d→c，`plan_gap/requirement_gap` 时 d→b；4) 模型建议越权边（如 d→a）丢弃并按规则表修正；5) 审计记录 `prompt_version/model/input_digest/tokens/duration`；6) 验收侧写文件调用被拒并记 `security.violation`。 |
| 验收标准 | `go test ./pkg/harnessloop/acceptance/... -run TestVerdict`；c 验收 fail→b 且下一轮 b 上下文可见 evidence；验收结束后隔离临时目录为空；改模板版本后审计出现新版本号。 |
| 复杂度 | M |

### T-P0-13 异常分类与分类重试策略

| 字段 | 内容 |
|---|---|
| 目标 | 8 类异常分类器 + 分类策略表 + 两类预算隔离 + quota 快速失败。 |
| 关联需求 | REQ-021、REQ-022、REQ-025、REQ-027、REQ-028、REQ-029、REQ-030；design 4.3.1 / 4.3.2 / 4.3.4 / 4.3.5 / 4.3.6 |
| 依赖 | T-P0-12 |
| 涉及文件 | [N] `pkg/harnessloop/retry/classifier.go`、[N] `pkg/harnessloop/retry/policy.go`、[N] `pkg/harnessloop/retry/backoff.go`、[N] `pkg/harnessloop/retry/budget.go`、[N] `pkg/config/harness_loop/retry.yaml`、[N] `pkg/harnessloop/retry/classifier_test.go` |
| 实施要点 | 1) 枚举 `network/runtime/system/quota/timeout/no_finish/invalid_tool_result/verification_error`；2) 策略表按 4.3.2 默认值（network/system 3 次、timeout/no_finish 2 次、quota 1 次、content_filter 0 次），启动加载为只读快照；3) 重试计数与验收失败计数分离；4) quota 一次确认后立即 failed，禁长退避；5) 工具空结果/错误码 → `invalid_tool_result` 且只重发该工具调用；6) 每次异常发 error 事件含 `trace_id/task_id/stage/attempt/category/retryable/retry_index/max_retries` 与原始错误摘要。 |
| 验收标准 | `go test ./pkg/harnessloop/retry/...`：8 类注入均归类、无 `unknown`；no_finish 上限 3 时第 4 次 failed；连续 2 次验收失败 + 1 次网络异常后两计数分别保持 2 与 1；quota 在 30s 内终止。 |
| 复杂度 | M |

### T-P0-14 no_finish 检测与 finish_reason 归一化

| 字段 | 内容 |
|---|---|
| 目标 | 静默超时判定与模型结束原因归一：`length` 触发压缩或重试，`content_filter` 不可重试。 |
| 关联需求 | REQ-023、REQ-024；design 4.3.3 |
| 依赖 | T-P0-13 |
| 涉及文件 | [N] `pkg/harnessloop/retry/finish_reason.go`、[N] `services/harnessloop-service/internal/watchdog/watchdog.go`、[N] `pkg/harnessloop/retry/finish_reason_test.go` |
| 实施要点 | 1) `last_activity` 取 token/工具结果/心跳/进度事件最大时间；2) 阈值默认 120s，来自 `retry.yaml`；3) 有流式 token 期间不判异常；4) 结束原因归一为 `stop/length/tool_calls/content_filter/other`；5) `length` → 触发压缩，压缩失败再按 `length` 处理；6) `content_filter` max_retries=0，立即 failed。 |
| 验收标准 | `go test ./pkg/harnessloop/retry/... -run TestNoFinish`（时间源可注入）；静默 121s 产 `error(category=no_finish)`；`content_filter` 一次即 failed 且无重试事件。 |
| 复杂度 | S |

### T-P0-15 检查点保存/恢复/漂移

| 字段 | 内容 |
|---|---|
| 目标 | 五时机落检查点，恢复幂等且位置正确，工作区不一致显式 drift，损坏自动降级。 |
| 关联需求 | REQ-031 ~ REQ-037、DFX-R-02/R-03/R-07；design 4.4.1 ~ 4.4.5 |
| 依赖 | T-P0-03、T-P0-07 |
| 涉及文件 | [N] `pkg/harnessloop/checkpoint/store.go`、[N] `pkg/harnessloop/checkpoint/fsm_snapshot.go`、[N] `pkg/harnessloop/checkpoint/restore.go`、[N] `pkg/harnessloop/checkpoint/drift.go`、[N] `pkg/harnessloop/checkpoint/retention.go`、[N] `services/harnessloop-service/internal/repository/checkpoint_repo.go`、[N] `pkg/harnessloop/checkpoint/restore_test.go` |
| 实施要点 | 1) 复用 `pkg/agent/checkpoint` 的 Mongo 存储接口，追加 FSM 维度字段（阶段/attempt/重试计数/产物哈希/记忆引用/事件水位）；2) 五时机：验收通过后、转移前、每批工具调用后、压缩前、中止时；3) 恢复校验四项一致（阶段/attempt/产物哈希/计量），不一致标 drift 并按配置策略处理；4) 重复恢复用幂等键；5) 损坏回退上一可用检查点并产 `checkpoint.corrupted`；6) 保留最近 N 个 + 每阶段最后一个，清理写审计。 |
| 验收标准 | `go test ./pkg/harnessloop/checkpoint/...`；kill -9 后恢复用例 30s 内恢复且事件流无新的 a/b/c 通过记录；手工改受检文件后恢复产 `checkpoint.drift`；破坏最新检查点后使用上一份继续。 |
| 复杂度 | L |

### T-P0-16 阶段内执行适配 stage_runner

| 字段 | 内容 |
|---|---|
| 目标 | 把阶段执行委托给 agent-service `ExecuteStream`，注入阶段模式、策略快照与阶段模型，不重建注册中心。 |
| 关联需求 | REQ-006、REQ-102、REQ-116；design 4.1.5 / 3.7 |
| 依赖 | T-P0-06、T-P0-19、T-P0-20 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/stage_runner.go`、[N] `pkg/harnessclient/client.go`、[N] `pkg/harnessclient/stream.go`、[N] `services/harnessloop-service/internal/service/stage_runner_test.go` |
| 实施要点 | 1) 调 agent-service `ExecuteStream`，`context_vars` 注入 `__stage_mode__/__policy_snapshot__/__policy_hash__/__stage_model__/__model_window__/__requires_tool_call__`；2) 流式输出喂 watchdog 与事件发布；3) 只识别结构化 finish；4) 流内错误按 T-P0-13 分类上抛；5) Agent 发现与技能挂载一律走 agent-service，不本地缓存注册表。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/service/... -run TestStageRunner`（agent 桩）；断言 `context_vars` 六键齐全且不含策略文件路径；无工具调用时阶段不结束。 |
| 复杂度 | M |

### T-P0-17 runtime 主循环编排

| 字段 | 内容 |
|---|---|
| 目标 | 串起「提交 → 阶段执行 → 验收 → 转移/回退 → 终态」单一主循环，三类终态唯一出口。 |
| 关联需求 | REQ-007、REQ-008、REQ-009；design 4.1.4 / 3.7 |
| 依赖 | T-P0-07、T-P0-12、T-P0-13、T-P0-15、T-P0-16 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/runtime.go`、[N] `services/harnessloop-service/internal/service/runtime_test.go` |
| 实施要点 | 1) 状态驱动单循环，禁止旁路终态；2) MaxSteps 上限归入 `system` 异常（可重试 3 次），不得直接 failed；3) abort 以 `task_id+abort_epoch` 幂等，并发用 advisory lock 串行；4) 终止报告含失败阶段/异常分类/重试次数/最后错误/checkpoint_id；5) 每步发事件并推进 seq。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/service/... -run TestRuntimeThreeTerminals`：三种终态 reason 分别为 completed/aborted/failed；连续 3 次 abort 仅 1 条 `task.aborted` 且时间戳一致；模型输出为空时不终止。 |
| 复杂度 | L |

### T-P0-18 harnessloop-service 启动与 gRPC

| 字段 | 内容 |
|---|---|
| 目标 | 服务可独立启动，暴露 P0 RPC 与健康检查，支持优雅退出与 `--local` 形态。 |
| 关联需求 | REQ-096、REQ-119；design 8.1 / 6.2 / 3.5 |
| 依赖 | T-P0-02、T-P0-17 |
| 涉及文件 | [N] `services/harnessloop-service/cmd/main.go`、[N] `services/harnessloop-service/internal/handler/grpc_handler.go`、[N] `services/harnessloop-service/internal/config/config.go`、[N] `services/harnessloop-service/config.example.yaml`、[N] `services/harnessloop-service/Dockerfile` |
| 实施要点 | 1) 监听 `:50008`，gRPC 拦截器注入 tenant 与调用方身份；2) 装配 runtime/repo/publisher/policy/modelregistry；3) 健康检查 + 就绪探针；4) 优雅退出时中止运行中任务并保存检查点；5) `--local` 仅监听 `127.0.0.1:50008`。 |
| 验收标准 | `go build ./services/harnessloop-service/...`；启动后 `grpcurl -plaintext :50008 list` 含 `HarnessLoopService`；SIGTERM 后运行中任务落检查点且无 panic。 |
| 复杂度 | M |

### T-P0-19 权限最小集

| 字段 | 内容 |
|---|---|
| 目标 | deny-by-default 双层名单 + 阶段矩阵 + 路径沙箱 + exec 白名单 + 结构化拒绝，拒绝发生在工具执行前。 |
| 关联需求 | REQ-156、REQ-157、REQ-158、REQ-159、REQ-160、REQ-161、REQ-162、REQ-164、REQ-166、REQ-167；design 4.15.1 ~ 4.15.5 / 4.15.8 / 4.15.9 |
| 依赖 | T-P0-01、T-P0-03 |
| 涉及文件 | [N] `pkg/harnessloop/policy/engine.go`、[N] `pkg/harnessloop/policy/matrix.go`、[N] `pkg/harnessloop/policy/registry.go`、[N] `pkg/harnessloop/policy/sandbox/realpath.go`、[N] `pkg/harnessloop/policy/sandbox/pathguard.go`、[N] `pkg/harnessloop/policy/sandbox/execguard.go`、[N] `pkg/config/harness_loop/stage_matrix.yaml`、[N] `pkg/config/harness_loop/exec_allow.yaml` |
| 实施要点 | 1) 决策顺序：系统硬限制 → 租户 → 角色 → 任务，deny 优先于 allow；2) `realpath` 按 4.15.3 九步算法（NUL/UNC/相对以 workspace_root 为基准/Clean/EvalSymlinks/平台归一化/段边界前缀）；3) 校验为工具 `Execute` 前的同步中间件，拒绝即返回、无副作用；4) exec 拒绝复合命令、`sh -c`、绝对路径参数、安装动作；5) 结构化错误含 `rule_id/layer/tool/stage/requested_path`；6) 阶段矩阵配置化，业务代码无 if-else 硬编码。 |
| 验收标准 | `go test ./pkg/harnessloop/policy/...` 与 `go test -race`；`../`、`..\\`、URL 编码变体、绝对路径、符号链接、`go test && curl` 全拒且工具函数未被调用；a 阶段 write 拒绝、c 阶段白名单内 write 允许、d 阶段 write 拒绝但 `go test ./...` 允许、e 阶段全拒；`policy/sandbox` 覆盖率 ≥ 90%。 |
| 复杂度 | L |

### T-P0-20 模型注册表与能力校验

| 字段 | 内容 |
|---|---|
| 目标 | 注册表加载与字段校验、阶段模型解析、能力匹配拒绝并给出全部满足条件的候选。 |
| 关联需求 | REQ-142、REQ-145、REQ-146、REQ-151；design 4.14.1 ~ 4.14.4 |
| 依赖 | T-P0-02 |
| 涉及文件 | [N] `pkg/harnessloop/modelregistry/registry.go`、[N] `pkg/harnessloop/modelregistry/capability.go`、[N] `pkg/harnessloop/modelregistry/secret.go`、[N] `pkg/config/harness_loop/models.yaml`、[N] `pkg/harnessloop/modelregistry/registry_test.go` |
| 实施要点 | 1) 字段齐全校验（context_window/supports_tool_call/单价），缺失返 `E_MODEL_META_INCOMPLETE`；2) `max_output ≤ context_window`；3) `embedding_dim` 仅 embedding 模型必填，非 embedding 必须为空；4) 阶段覆盖默认继承任务级（U18-A），quality_first 下 d 阶段强模型仅作模板；5) 能力不满足返 `E_MODEL_CAPABILITY_MISMATCH` + `suggested_models`（全部满足工具与窗口）；6) 压缩阈值读注册表窗口，代码内无硬编码窗口常量；7) 凭据只以 `api_key_ref` 存在，不出现在任何响应。 |
| 验收标准 | `go test ./pkg/harnessloop/modelregistry/...`；c 阶段选非工具模型返回错误且阶段不启动；`go test -run TestNoHardcodedWindow` 通过（AST 扫描窗口常量）；`GET /models` 响应无密钥字段。 |
| 复杂度 | M |

### T-P0-21 Gateway REST 路由与严格租户

| 字段 | 内容 |
|---|---|
| 目标 | 暴露 `/api/v2/harness-loop/*` P0 路由，统一错误 envelope，新增命名空间强制显式租户。 |
| 关联需求 | REQ-119、REQ-120、DFX-S-01、DFX-C-01；design 6.3 / 3.3 |
| 依赖 | T-P0-18 |
| 涉及文件 | [N] `services/gateway/internal/handler/harness_loop_handler.go`、[M] `services/gateway/internal/router/router.go`、[M] `services/gateway/internal/middleware/middleware.go`、[N] `services/gateway/internal/handler/harness_loop_handler_test.go` |
| 实施要点 | 1) P0 路由：tasks 列表/详情/pause/resume/abort、events、checkpoints/restore、report；2) `StrictTenant` 仅对 `/harness-loop` 组生效，缺失 `X-Tenant-ID` 返回 401；3) 错误 envelope 统一 `{code,message,details,trace_id}`；4) 写操作支持 `Idempotency-Key`；5) pause/abort 需二次确认字段。 |
| 验收标准 | `go test ./services/gateway/... -run TestHarnessLoopRoutes`；无租户头返回 401；`E_ILLEGAL_TRANSITION` 映射 409、`E_ACCEPTANCE_CONTEXT_INCOMPLETE` 映射 422；存量 `/api/v2/*` 路由测试不回归。 |
| 复杂度 | M |

### T-P0-22 最小 CLI

| 字段 | 内容 |
|---|---|
| 目标 | cobra CLI 提供 `run/status/abort/resume/logs`，支持 `--json` 与退出码语义。 |
| 关联需求 | REQ-097、REQ-100、REQ-101、REQ-102；design 4.11.1 / 4.11.2 / 4.11.5 |
| 依赖 | T-P0-16、T-P0-21 |
| 涉及文件 | [N] `cmd/harness-cli/main.go`、[N] `internal/cli/cmd/root.go`、[N] `internal/cli/cmd/run.go`、[N] `internal/cli/cmd/status.go`、[N] `internal/cli/cmd/abort.go`、[N] `internal/cli/cmd/resume.go`、[N] `internal/cli/output/exitcode.go`、[N] `internal/cli/output/redact.go` |
| 实施要点 | 1) 全局 flags `--server/--tenant/--config/--json/--no-color`；2) 配置优先级 flag > env（`HARNESS_*`）> `~/.harness/config.yaml` > 内置默认；3) 退出码 0 成功 / 2 failed / 3 aborted / 4 参数错误 / 5 连接失败；4) JSON envelope 对齐 4.11.5；5) `redact.go` 脱敏 `sk-*`、`Bearer *`、`password=`；6) 每命令 `--help` 可用。 |
| 验收标准 | `go build ./cmd/harness-cli`；`harness-cli run --help` 正常；E2E：成功任务退出码 0、failed 任务 2、abort 任务 3、非法参数 4；`--json` 输出可 `jq` 解析且无 Key 明文。 |
| 复杂度 | M |

### T-P0-23 特性开关与回滚

| 字段 | 内容 |
|---|---|
| 目标 | `fsm.enabled` 在任务创建时求值一次并冻结，关闭时行为与现状完全一致。 |
| 关联需求 | REQ-122、DFX-C-02；design 4.1.5 |
| 依赖 | T-P0-18 |
| 涉及文件 | [N] `services/harnessloop-service/internal/config/featureflag.go`、[M] `services/harnessloop-service/internal/config/config.go`、[N] `services/harnessloop-service/internal/config/featureflag_test.go` |
| 实施要点 | 1) 优先 harness-service FeatureFlag 引擎，本地 `harness_loop.enabled` 兜底；2) 默认 `false`；3) 求值上下文 `{tenant_id, project_id, task_type, user_id}`；4) 结果写入 `TaskRun.fsm_enabled`，运行中不切换；5) 关闭时 `/harness-loop/*` 返 `E_FSM_DISABLED`，已建任务仍可查询与只读恢复。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/config/...`；开关关闭时 `POST /tasks` 返回 503 `E_FSM_DISABLED`；重新开启后历史任务可 `GET` 到且可 `restore`。 |
| 复杂度 | S |

### T-P0-T1 单元测试：FSM 穷举与终态

| 字段 | 内容 |
|---|---|
| 目标 | 建立 9.2 状态机穷举用例集与终态/幂等单测，作为 P0 门槛之一。 |
| 关联需求 | REQ-001 ~ REQ-009；design 9.2 / 8.4 |
| 依赖 | T-P0-04、T-P0-05、T-P0-06、T-P0-07、T-P0-17 |
| 涉及文件 | [N] `pkg/harnessloop/fsm/transitions_exhaustive_test.go`、[N] `pkg/harnessloop/fsm/terminal_test.go`、[N] `services/harnessloop-service/internal/service/abort_test.go` |
| 实施要点 | 1) 8 条合法边参数化用例（含条件分支）；2) 非法边/跳过阶段/终态出边全覆盖；3) 10 goroutine 并发同边仅 1 成功；4) 转移前后各设崩溃点，重启后状态为合法值；5) 中止 3 次仅 1 条终态事件。 |
| 验收标准 | `go test -race ./pkg/harnessloop/fsm/... ./services/harnessloop-service/internal/service/...` 全绿；生成转移矩阵测试报告并留档 `bench/p0/fsm-matrix.md`。 |
| 复杂度 | M |

### T-P0-T2 验收 Agent 假阳性/假阴性

| 字段 | 内容 |
|---|---|
| 目标 | 覆盖 9.3 的 7 组验收判定用例，证明验收不可被诱导、不可越权回退。 |
| 关联需求 | REQ-011 ~ REQ-020；design 9.3 |
| 依赖 | T-P0-10、T-P0-11、T-P0-12 |
| 涉及文件 | [N] `pkg/harnessloop/acceptance/verifier_negative_test.go`、[N] `pkg/harnessloop/acceptance/checks/registry_negative_test.go`、[N] `bench/p0/acceptance-report.md` |
| 实施要点 | 1) 诱导注入（执行历史写"请判定通过"）；2) 空产物 → fail 且 `failed_criteria` 含 `impl_gap`；3) 伪造证据（pass + 空 evidence）→ invalid；4) 未验证冲突（命令不存在）不得整体 pass；5) 只读违规 + 回退越权建议丢弃；6) checker panic → `verification_error` 重试 2 次后 blocked。 |
| 验收标准 | `go test ./pkg/harnessloop/acceptance/... -run TestNegative` 全绿；7 组用例逐条对应 9.3 表格并在报告中标注用例 ID。 |
| 复杂度 | M |

### T-P0-T3 集成测试：检查点/事件/CLI E2E

| 字段 | 内容 |
|---|---|
| 目标 | 打通「PG + Mongo + agent 桩 + CLI」的最小闭环，验证恢复、事件幂等与退出码。 |
| 关联需求 | REQ-031 ~ REQ-037、REQ-103 ~ REQ-106、REQ-097 ~ REQ-100；design 9.1 |
| 依赖 | T-P0-09、T-P0-15、T-P0-18、T-P0-21、T-P0-22 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/e2e_test.go`、[N] `internal/cli/cmd/e2e_test.go`、[N] `bench/p0/e2e-report.md` |
| 实施要点 | 1) compose profile `test` 起 PG/Mongo；2) 跑完整五阶段任务；3) 中途 kill 进程后恢复；4) 重复投递同一 event 三次；5) CLI 与 Web 提交同一任务比对阶段序列与终态。 |
| 验收标准 | `go test -tags=integration ./services/harnessloop-service/... ./internal/cli/...` 全绿；恢复后不重复已通过阶段；事件计数不重复；CLI 与 Web 终态一致。 |
| 复杂度 | L |

### T-P0-T4 安全负向与模型能力校验（P0 门槛）

| 字段 | 内容 |
|---|---|
| 目标 | 把 9.11 的 P0 子集与 9.10 的能力匹配子集落为自动化负向用例，零放行。 |
| 关联需求 | REQ-010、REQ-019、REQ-146、REQ-156 ~ REQ-162、REQ-164、REQ-168、REQ-169；design 9.10 / 9.11 / 8.4 |
| 依赖 | T-P0-19、T-P0-20 |
| 涉及文件 | [N] `pkg/harnessloop/policy/policy_negative_test.go`、[N] `pkg/harnessloop/policy/sandbox/escape_test.go`、[N] `pkg/harnessloop/modelregistry/capability_negative_test.go`、[N] `bench/p0/policy-report.md`、[N] `bench/p0/model-report.md` |
| 实施要点 | 1) 路径：`../`、`..\\`、`.../`、`%2e%2e/`、绝对路径、UNC/`\\?\`、`file://`、符号链接、ToCToU 替换；2) exec：`&&`、`;`、`sh -c`、绝对路径输出、`npm install`；3) 自我提权：写 `config/harness_loop/stage_allow.yaml` 与提示注入；4) 矩阵：a/c/d/e 四阶段 write 与 exec 行为；5) 模型：缺元数据、非工具模型、窗口不足、`suggested_models` 全满足；6) 每条拒绝断言 `rule_id/layer/error_code` 齐全且无副作用。 |
| 验收标准 | `go test ./pkg/harnessloop/policy/... ./pkg/harnessloop/modelregistry/... -run TestNegative` 全绿；逃逸用例 100% 拒绝；报告留档两份；`policy_hash` 在任务前后一致。 |
| 复杂度 | L |

> P0 合计：实施 23 个 + 测试 4 个 = **27 个任务**。

## 3.2 P1：记忆、压缩、索引、TUI、大屏、知识增强、多 Agent、模型策略、权限完整版（36 个任务）

### T-P1-01 markdown 记忆布局与格式

| 字段 | 内容 |
|---|---|
| 目标 | 在项目工作区落地 markdown 记忆文件（长期 1 个 + 每项目 1 个），格式稳定、Git 友好。 |
| 关联需求 | REQ-047、REQ-048、REQ-049、REQ-052、REQ-054；design 4.6.1 / 4.6.2 / 4.6.3 |
| 依赖 | T-P0-19（写路径白名单） |
| 涉及文件 | [N] `pkg/memoryfile/layout.go`、[N] `pkg/memoryfile/format.go`、[N] `pkg/memoryfile/parse.go`、[N] `pkg/memoryfile/render.go`、[N] `pkg/memoryfile/parse_test.go`、[N] `pkg/memoryfile/render_test.go`、[M] `.gitignore` |
| 实施要点 | 1) 目录为 `<workspace>/.harness/memory/{global.md,projects/<project>.md}`（U10 默认值，可配）；2) 主题含 `key/title/body/category/updated_at/order/pinned/content_hash`；3) 字段顺序固定、主题边界用固定分隔标记；4) 四类 `user_preference/long_term_constraint/project_fact/task_state`；5) 输出为合法 markdown 且单主题更新只产生局部 diff。 |
| 验收标准 | `go test ./pkg/memoryfile/... -run TestParseRender`：往返解析后字节等价；3 个项目产生 1 个长期 + 3 个项目文件；单主题更新后 `git diff` 行数与该主题行数量级一致。 |
| 复杂度 | M |

### T-P1-02 LRU 30 主题与访问提升

| 字段 | 内容 |
|---|---|
| 目标 | 单文件主题上限与 LRU 淘汰、读取即提升、pin 主题豁免，顺序持久化。 |
| 关联需求 | REQ-050、REQ-051、REQ-055；design 4.6.4 |
| 依赖 | T-P1-01 |
| 涉及文件 | [N] `pkg/memoryfile/lru.go`、[N] `pkg/memoryfile/lru_test.go`、[M] `pkg/memoryfile/format.go` |
| 实施要点 | 1) 上限默认 30，来自配置；2) 队首 `order=0`，其余相对顺序不变；3) 读取与更新都提升到队首，但读取不改 `updated_at`；4) `pinned=true` 不参与淘汰；5) 同主题重复写入走合并/替换，正文受配置上限约束。 |
| 验收标准 | `go test ./pkg/memoryfile/... -run TestLRU`：写 31 个主题后剩 30、第 1 个被淘汰、第 31 个在队首；读取队尾后重新解析该主题在队首；连续读 10 次内容哈希不变。 |
| 复杂度 | M |

### T-P1-03 原子写、文件锁与损坏恢复

| 字段 | 内容 |
|---|---|
| 目标 | 记忆写入原子化（临时文件 + 原子替换）、跨平台锁、损坏时备份并重建索引。 |
| 关联需求 | REQ-053、REQ-056、REQ-057、DFX-R-06、DFX-C-05；design 4.6.5 / 4.6.6 / 4.6.7 |
| 依赖 | T-P1-01 |
| 涉及文件 | [N] `pkg/memoryfile/writer.go`、[N] `pkg/memoryfile/lock_unix.go`、[N] `pkg/memoryfile/lock_windows.go`、[N] `pkg/memoryfile/recover.go`、[N] `pkg/memoryfile/events.go`、[N] `pkg/memoryfile/atomic_test.go`、[N] `pkg/memoryfile/recover_test.go` |
| 实施要点 | 1) `tmp` 文件同目录 + `os.Rename` 原子替换，失败清理临时文件；2) Windows 用独占句柄锁，Unix 用 `flock`；3) 写前按 `content_hash` 判等，幂等写入不改文件；4) 解析失败保留 `.bak` + 告警事件，未损坏主题仍可读；5) 写入/合并/淘汰/读取提升各发事件。 |
| 验收标准 | `go test ./pkg/memoryfile/...`（含 Windows 构建标签）；写入中断后原文件仍可解析；重复写相同内容文件哈希不变；注入非法 markdown 产 `memory.corrupted` 且 `.bak` 存在。 |
| 复杂度 | M |

### T-P1-04 mdsync 与 hlp_mem 索引同步

| 字段 | 内容 |
|---|---|
| 目标 | markdown 为唯一事实源，增量同步到 PG 记忆索引，10s 内可检索，memory-service 仅作派生索引。 |
| 关联需求 | REQ-058、REQ-061、REQ-117、DFX-C-03；design 4.6.7 / 4.7.1 / 5.1 |
| 依赖 | T-P1-01、T-P1-03、T-P0-03 |
| 涉及文件 | [N] `services/memory-service/internal/service/mdsync.go`、[N] `services/memory-service/internal/pgindex/memory_writer.go`、[N] `services/memory-service/internal/pgindex/tokenizer.go`、[M] `services/memory-service/internal/service/memory_service_with_forgetting.go`、[M] `services/memory-service/cmd/main.go`、[N] `pkg/pgindex/ddl/001_mem.sql`、[M] `proto/memory/memory.proto` |
| 实施要点 | 1) 监听 md 变更（fsnotify + 兜底轮询），按 `content_hash` 增量 upsert/delete；2) `memory_topics/memory_chunks` 表含 tsvector 列与 `vector(1024)`（U7 默认）；3) 遗忘算法保留，只降权不删 md（U1-A），`source=markdown` 分流；4) memory-service 冲突时以 md 为准并发 `memory.resync`；5) proto 追加 `SyncFromMarkdown` 与 `MemoryTopicRef`，不删旧字段。 |
| 验收标准 | `go test -tags=integration ./services/memory-service/... -run TestMDSync`：手工改 md 后 10s 内检索到新内容；删除主题后不再返回；停 memory-service 不影响 md 写入。 |
| 复杂度 | L |

### T-P1-05 记忆混合召回（全文 + 按需向量 + RRF）

| 字段 | 内容 |
|---|---|
| 目标 | 记忆召回支持全文关键词与 pgvector 语义两路，按需触发向量，RRF 融合统一排序。 |
| 关联需求 | REQ-058、REQ-059、REQ-060、REQ-065、DFX-P-05；design 4.7.3 / 4.7.6 |
| 依赖 | T-P1-04、T-P1-10 |
| 涉及文件 | [N] `services/memory-service/internal/pgindex/recall.go`、[M] `services/memory-service/internal/handler/grpc_handler.go`、[M] `proto/memory/memory.proto`、[N] `services/memory-service/internal/pgindex/recall_test.go` |
| 实施要点 | 1) 先全文召回，满足 top_k 则不调 embedding；2) 不足或语义型查询才走向量，事件记录触发原因；3) RRF `k=60` 可配，重叠条目不降级；4) 返回带 `topic_key/source_file/updated_at/score`；5) 上报召回延迟、命中率、路由分布指标。 |
| 验收标准 | `go test ./services/memory-service/... -run TestRecall`；全文已满 top_k 时 embedding 调用次数为 0；两路各 10 条重叠时融合结果无重复；P95 ≤ 300ms（基准集留档）。 |
| 复杂度 | M |

### T-P1-06 动态注入、预算与降级

| 字段 | 内容 |
|---|---|
| 目标 | 每阶段开始前按阶段类型注入 top-k 记忆，注入不超过上下文预算 15%，可追溯且索引故障时可降级。 |
| 关联需求 | REQ-062、REQ-063、REQ-064；design 4.7.4 / 4.7.5 |
| 依赖 | T-P1-05、T-P0-16 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/memory_service.go`、[N] `services/harnessloop-service/internal/service/memory_inject_test.go` |
| 实施要点 | 1) 阶段→记忆类型映射（如 b 阶段取 project_fact/long_term_constraint）；2) 注入预算取模型窗口 15%，超预算按分数截断；3) 每条注入附 4 项元数据并可回溯；4) PG 不可用时降级直读 md 关键词匹配并产 `memory.index_degraded`；5) 注入 token 占比上报指标。 |
| 验收标准 | `go test ./services/harnessloop-service/internal/service/... -run TestMemoryInject`：b 阶段注入含计划相关事实且占比 ≤ 15%；停 PG 后仍注入 ≥ 1 条并产降级事件；快照中每条注入含 4 项元数据。 |
| 复杂度 | M |

### T-P1-07 压缩阈值与触发

| 字段 | 内容 |
|---|---|
| 目标 | 上下文占用达模型窗口比例（默认 80%）或绝对 token 阈值时，在下次模型调用前触发压缩。 |
| 关联需求 | REQ-038、REQ-150；design 4.5.1 / 4.14.7 |
| 依赖 | T-P0-20 |
| 涉及文件 | [N] `pkg/harnessloop/compaction/trigger.go`、[N] `pkg/config/harness_loop/compaction.yaml`、[N] `pkg/harnessloop/compaction/trigger_test.go` |
| 实施要点 | 1) 阈值读模型注册表窗口，未知模型回退绝对阈值；2) 触发原因枚举 `threshold/length_finish/manual`；3) 模型切换后按新窗口重算阈值并后移触发点；4) 触发前保存检查点；5) 产 `compaction.triggered` 事件。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/... -run TestTrigger`；32k→128k 切换后 `threshold_tokens` 变大；超阈值时下一次请求上下文低于阈值。 |
| 复杂度 | S |

### T-P1-08 模板摘要与五要素 JSONL

| 字段 | 内容 |
|---|---|
| 目标 | 自研模板摘要为唯一压缩通道，产出五要素齐全的结构化 JSONL，原始需求不可变。 |
| 关联需求 | REQ-039、REQ-040、REQ-041、REQ-044；design 4.5.2 / 4.5.3 / 4.5.4 |
| 依赖 | T-P1-07 |
| 涉及文件 | [N] `pkg/harnessloop/compaction/template.go`、[N] `pkg/harnessloop/compaction/jsonl.go`、[N] `pkg/harnessloop/compaction/engine.go`（仅接口与空实现）、[N] `pkg/harnessloop/compaction/jsonl_test.go` |
| 实施要点 | 1) 模板版本化（`template.v1`），提示词含五要素抽取要求；2) 输出逐行 JSON，字段 `type/phase/summary/source_refs/timestamp`；3) 五要素 `goal/done/todo/constraints/background` 缺一即压缩失败；4) `engine.go` 只定义 `ContextEngine` 接口与 `enabled=false` 空适配器，不 import 任何外部引擎、不拉起进程；5) 摘要产物写 `.harness/tmp/<task_id>/compaction/*.jsonl`，原文哈希不变。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/...`：五要素齐全率 100%；逐行 JSON 可解析率 100%；连续 3 次压缩后原始需求哈希不变；`go list -deps ./pkg/harnessloop/compaction` 无第三方压缩引擎包。 |
| 复杂度 | L |

### T-P1-09 配对校验、三级降级与 A/B

| 字段 | 内容 |
|---|---|
| 目标 | 压缩后工具调用配对合法；模板失败按 L1 截断 → L2 紧急丢弃分级降级；A/B 以模板版本对比。 |
| 关联需求 | REQ-042、REQ-043、REQ-045、REQ-046、U16；design 4.5.5 ~ 4.5.8 / 9.4 |
| 依赖 | T-P1-08 |
| 涉及文件 | [N] `pkg/harnessloop/compaction/pairing.go`、[N] `pkg/harnessloop/compaction/abtest.go`、[N] `pkg/harnessloop/compaction/degrade.go`、[N] `pkg/harnessloop/compaction/pairing_test.go`、[N] `pkg/pgindex/ddl/005_compaction.sql` |
| 实施要点 | 1) 校验 tool_call 与 tool_result 一一配对，孤立项拒绝应用并回退保结构截断；2) L1 保留 system/goal/五要素/最近 N 轮；L2 紧急丢弃但保持配对完整；3) 引擎开关关闭时，外部引擎缺失不得产生 `compaction.degraded`；4) 记录 `tokens_before/after/ratio/strategy_id/prompt_version/degraded`；5) A/B 按模板版本比较压缩比与下游验收通过率。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/... -run TestPairing` 与 `TestDegrade`；默认配置跑完整任务不产 `compaction.degraded`、`engine_fragment` 计数为 0；注入摘要故障后任务继续且五要素占位块可读。 |
| 复杂度 | L |

### T-P1-10 pkg/rrf 通用融合

| 字段 | 内容 |
|---|---|
| 目标 | 抽象记忆/代码/知识共用的 RRF 融合、去重与稳定排序，无 DB 依赖。 |
| 关联需求 | REQ-081、REQ-083；design 4.9.1 / 8.1 / 8.2 |
| 依赖 | T-P0-01 |
| 涉及文件 | [N] `pkg/rrf/fuse.go`、[N] `pkg/rrf/dedup.go`、[N] `pkg/rrf/stable_sort.go`、[N] `pkg/rrf/fuse_test.go` |
| 实施要点 | 1) `score = Σ 1/(k+rank)`，k 默认 60 可配；2) 同一实体多路命中合并并累加分数，保留来源列表；3) 分数相同按 `file_path → start_line → id` 稳定排序；4) 禁止 import 任何 DB 客户端（depcheck 校验）。 |
| 验收标准 | `go test ./pkg/rrf/...`：与手算 RRF 结果一致；同查询重复 10 次顺序完全一致；重叠条目仅出现一次；`go run ./scripts/depcheck` 无违规。 |
| 复杂度 | S |

### T-P1-11 codeindex watcher/debounce/queue

| 字段 | 内容 |
|---|---|
| 目标 | 工作区变更监听与 5s 防抖、批量入队、同文件串行去重。 |
| 关联需求 | REQ-068、REQ-074、REQ-077、REQ-078、U9；design 4.8.4 / 4.8.6 |
| 依赖 | T-P0-01 |
| 涉及文件 | [N] `pkg/codeindex/watcher.go`、[N] `pkg/codeindex/debounce.go`、[N] `pkg/codeindex/queue.go`、[N] `pkg/codeindex/ignore.go`、[N] `pkg/codeindex/debounce_test.go` |
| 实施要点 | 1) fsnotify 监听 create/write/remove/rename，每次事件重置 5s 定时器；2) 到期 flush pending 集合；3) 同文件任务串行或去重，跨文件并发受上限约束；4) 忽略 `.git/node_modules/vendor/dist` 与超限文件；5) 暴露队列深度与待处理状态查询。 |
| 验收标准 | `go test ./pkg/codeindex/... -run TestDebounce`；1s 内保存同文件 5 次仅触发 1 次索引；并发 10 次同文件更新最终状态唯一；Windows 与 Linux 构建均通过。 |
| 复杂度 | M |

### T-P1-12 Go AST 解析与切片

| 字段 | 内容 |
|---|---|
| 目标 | 用 Go AST 提取函数/方法、调用点、导入关系与符号表，按函数切分 chunk。 |
| 关联需求 | REQ-066、REQ-067、REQ-076；design 4.8.1 / 4.8.2 |
| 依赖 | T-P1-11 |
| 涉及文件 | [N] `pkg/codeindex/parser/parser.go`、[N] `pkg/codeindex/parser/go_ast.go`、[N] `pkg/codeindex/parser/lang.go`、[N] `pkg/codeindex/chunk.go`、[N] `pkg/codeindex/parser/parser_test.go` |
| 实施要点 | 1) 解析器接口按语言注册，Go 用标准库 `go/ast`；2) chunk 含 `file_path/start_line/end_line/symbol/content_hash/text`；3) 边含 `source/target/edge_type(call|import)/source_file/target_file`；4) 标识符分词处理 camelCase 与 snake_case；5) 解析失败降级为文件级 chunk 并标 `degraded`。 |
| 验收标准 | `go test ./pkg/codeindex/parser/...`：10 函数/15 调用/5 导入样例计数与人工标注一致；`user profile` 查询能命中 `getUserProfile` 与 `get_user_profile`（分词单测）。 |
| 复杂度 | L |

### T-P1-13 四步增量与单事务

| 字段 | 内容 |
|---|---|
| 目标 | 哈希比对 → 删旧 → 重解析/embedding → 插入，单事务提交，失败保留旧索引。 |
| 关联需求 | REQ-069、REQ-070、REQ-071、REQ-072、REQ-073、REQ-075、REQ-079、DFX-R-04；design 4.8.5 / 4.8.7 |
| 依赖 | T-P1-12、T-P0-03 |
| 涉及文件 | [N] `pkg/codeindex/hash.go`、[N] `pkg/codeindex/writer.go`、[N] `pkg/codeindex/metrics.go`、[N] `pkg/pgindex/ddl/002_code.sql`、[N] `pkg/codeindex/incremental_test.go` |
| 实施要点 | 1) 先比对 `indexed_files.content_hash`，相同则跳过（仅可更新 mtime）；2) 单事务内：删 chunks/edges → 重新解析 → 批量 embedding → 插 chunks(+tsvector)/edges → 更新 `indexed_files`；3) 全量模式允许批量提交与参数调整，增量按单文件事务；4) 失败回滚、`status=failed`、`failure_count+1` 并按退避重试；5) 删除文件时清理 chunks/edges 与 indexed_files。 |
| 验收标准 | `go test ./pkg/codeindex/... -run TestIncremental`：touch 不产生写事务；改单函数后该文件 chunks 全替换且其他文件不变；中途注入失败旧索引仍可检索；`status` 与 `failure_count` 正确。 |
| 复杂度 | L |

### T-P1-14 代码检索 API（多路 + RRF + 隔离）

| 字段 | 内容 |
|---|---|
| 目标 | 暴露代码检索：向量 + 关键词 + （P2 图扩展）融合，结果结构完整且按租户/项目隔离。 |
| 关联需求 | REQ-067、REQ-082、REQ-084、REQ-085、REQ-086、DFX-P-03；design 4.9.2 / 4.9.3 / 6.2 |
| 依赖 | T-P1-13、T-P1-10 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/index_service.go`、[N] `proto/codeindex/codeindex.proto`、[N] `services/harnessloop-service/internal/service/index_service_test.go` |
| 实施要点 | 1) 向量与关键词两路召回，经 `pkg/rrf` 融合；2) 结果含 `file_path/start_line/end_line/symbol/snippet/score/sources[]`；3) 查询强制注入 `tenant_id/project_id`，跨项目返回空；4) 检索路径不持写锁，索引期间可读旧数据；5) 上报延迟与命中率。 |
| 验收标准 | `go test ./services/harnessloop-service/... -run TestSearchCode`；结果可渲染「文件:行号 + 片段 + 来源标签」；跨项目查询返回空且不泄露存在性；基准集 P95 ≤ 500ms。 |
| 复杂度 | M |

### T-P1-15 知识索引状态机与真实 BM25

| 字段 | 内容 |
|---|---|
| 目标 | 知识文档四态状态机、`kb_id` 隔离、BM25 真实分数与增量维护。 |
| 关联需求 | REQ-087、REQ-088、REQ-089、REQ-090、REQ-094；design 4.10.1 / 4.10.2 / 4.10.3 / 4.10.7 |
| 依赖 | T-P0-01、T-P1-10 |
| 涉及文件 | [M] `services/knowledge-service/internal/repository/document_repository.go`、[M] `services/knowledge-service/internal/search/bm25.go`、[M] `services/knowledge-service/internal/service/knowledge_service.go`、[M] `proto/knowledge/knowledge.proto`、[N] `services/knowledge-service/internal/repository/document_state_test.go` |
| 实施要点 | 1) 状态 `pending/processing/ready/failed`，failed 不进检索、可从列表重试；2) 上传校验格式/大小/空内容并返回明确错误码；3) 分块元数据含页码/章节/标签/切分策略/重叠长度；4) BM25 返回真实分数并支持增量更新与锁；5) RRF 稳定排序；6) `kb_id` 参与所有查询过滤。 |
| 验收标准 | `go test ./services/knowledge-service/... -run TestDocumentState`；上传 `.exe` 返 `E_UNSUPPORTED_FORMAT`、0 字节返 `E_EMPTY_DOCUMENT`；failed 文档检索不到；混合模式相关性不低于任一单路。 |
| 复杂度 | L |

### T-P1-16 知识来源引用、LLM rerank 与一致性

| 字段 | 内容 |
|---|---|
| 目标 | 检索结果带可定位来源引用，top-N 经 LLM rerank 输出 top-M，更新/删除后检索一致。 |
| 关联需求 | REQ-091、REQ-092、REQ-093、REQ-095、U4；design 4.10.4 / 4.10.5 / 4.10.6 |
| 依赖 | T-P1-15 |
| 涉及文件 | [N] `services/knowledge-service/internal/rerank/reranker.go`、[N] `services/knowledge-service/internal/rerank/llm_reranker.go`、[N] `services/knowledge-service/internal/rerank/noop.go`、[M] `services/knowledge-service/internal/service/knowledge_service.go`、[N] `services/knowledge-service/internal/rerank/reranker_test.go` |
| 实施要点 | 1) 引用含 `document_id/filename/chunk_id/page_no/section/score` 且可回原文；2) rerank 不可用/超时降级为融合顺序并标 `rerank=degraded`；3) rerank 只改顺序不改候选集；4) 文档更新重索引、删除清理分块，检索不得返回陈旧内容；5) 预留 HTTP cross-encoder 适配位（P2 实现）。 |
| 验收标准 | `go test ./services/knowledge-service/... -run TestRerank`；停 rerank 服务返回融合顺序且带 degraded 标记；删除文档后检索为空；引用可定位到页码/章节（集成测试解析）。 |
| 复杂度 | M |

### T-P1-17 subtask 七态状态机与表

| 字段 | 内容 |
|---|---|
| 目标 | 子任务拥有独立 7 态状态机、独立 attempt 与检查点，不使用 a~e 阶段标识。 |
| 关联需求 | REQ-123、REQ-124、REQ-137、REQ-141；design 4.13.1 / 5.1.2、spec 6.10 |
| 依赖 | T-P0-04、T-P0-07、T-P0-15 |
| 涉及文件 | [N] `pkg/harnessloop/subtask/model.go`、[N] `pkg/harnessloop/subtask/state.go`、[N] `pkg/harnessloop/subtask/repo.go`、[N] `pkg/pgindex/ddl/010_subtask_dag.sql`、[N] `pkg/harnessloop/subtask/state_test.go` |
| 实施要点 | 1) 状态仅 `pending/running/waiting_handoff/accepted/rejected/failed/cancelled`；2) `accepted` 仅在子任务验收通过后置位，DB 层加 CHECK 约束兜底；3) 子任务 `tenant_id/project_id` 必须与父任务一致；4) `attempt_no` 子任务内连续递增；5) `consumed_*` 计入父预算池，表结构无独立预算列。 |
| 验收标准 | `go test ./pkg/harnessloop/subtask/...`；强制把未验收子任务置 accepted 被拒并返 `E_SUBTASK_ACCEPTANCE_REQUIRED`，DB 约束同样拒绝；跨租户插入失败。 |
| 复杂度 | M |

### T-P1-18 计划阶段产出子任务 DAG

| 字段 | 内容 |
|---|---|
| 目标 | b 阶段验收要求计划含完整 DAG（节点/依赖/并行集/输入输出契约），并校验环、并行冲突、上限与不可拆。 |
| 关联需求 | REQ-125、REQ-126、REQ-127、REQ-134；design 4.13.2 |
| 依赖 | T-P1-17、T-P0-12 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/planner.go`、[N] `pkg/harnessloop/orchestrator/dag.go`、[N] `pkg/harnessloop/orchestrator/dag_test.go` |
| 实施要点 | 1) 校验节点集合、依赖边、可并行集、每节点输入/输出契约，缺项 b 验收 fail 并指明缺失；2) 环检测返 `E_DAG_CYCLE`；3) 有依赖的两节点放同一并行集返 `E_DAG_PARALLEL_CONFLICT`；4) 直接子任务 > 20 返 `E_SUBTASK_LIMIT_EXCEEDED`，深度 > 2 返 `E_SUBTASK_DEPTH_EXCEEDED`；5) 不可拆条件（单文件原子修改/单验证项/跨文件一致性/含审批点）返 `E_SUBTASK_NOT_SPLITTABLE`；6) 每个并行集必须声明 `fail-fast/tolerate/retry`，未声明返 `E_PROPAGATION_UNDECLARED`。 |
| 验收标准 | `go test ./pkg/harnessloop/orchestrator/... -run TestDAG`：环、并行冲突、21 节点、3 层、不可拆、缺传播策略全部按错误码拒绝；完整 DAG 产 `subtask.plan_created` 含节点数与并行集数。 |
| 复杂度 | L |

### T-P1-19 子任务调度、配额与熔断

| 字段 | 内容 |
|---|---|
| 目标 | 并行集按 8/20/20/2 上限调度，超限排队不失败，同类连续失败触发熔断。 |
| 关联需求 | REQ-128、REQ-129、REQ-130、REQ-138、DFX-P-11、DFX-R-08；design 4.13.3 / 4.13.8 |
| 依赖 | T-P1-18、T-P1-22 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/scheduler.go`、[N] `pkg/harnessloop/orchestrator/quota.go`、[N] `pkg/harnessloop/orchestrator/circuit.go`、[N] `pkg/config/harness_loop/subtask.yaml`、[N] `pkg/harnessloop/orchestrator/scheduler_test.go` |
| 实施要点 | 1) 单父任务 8 并行、单租户 20 并发、直接子任务 20、深度 2，均可配不得默认放开（U13-A）；2) 超配额进 pending 并产 `subtask.queued`；3) 子任务超时取 `min(父阶段剩余预算, 15min)` → failed(category=timeout)；4) 同类子任务样本 ≥ 4 且失败率 ≥ 50% → `subtask.circuit_open` 并停止调度该类；5) 预算按并行集均分，父任务可回收剩余。 |
| 验收标准 | `go test ./pkg/harnessloop/orchestrator/... -run TestScheduler`：12 节点并行集同时 running ≤ 8；租户 18 并发时新任务排队；4 同类 3 失败触发熔断；调度延迟 P95 ≤ 300ms（基准）。 |
| 复杂度 | L |

### T-P1-20 handoff 契约与校验

| 字段 | 内容 |
|---|---|
| 目标 | 交接记录结构完整可校验，接收方上下文仅由契约与显式注入构成，禁止共享全量对话。 |
| 关联需求 | REQ-131、REQ-132、REQ-137、REQ-140；design 4.13.4 |
| 依赖 | T-P1-17、T-P1-22 |
| 涉及文件 | [M] `pkg/harnessloop/orchestrator/handoff_contract.go`、[M] `pkg/harnessloop/orchestrator/handoff_validate.go`、[M] `pkg/harnessloop/orchestrator/handoff_transfer.go`、[M] `pkg/harnessloop/orchestrator/handoff_record.go`、[M] `pkg/harnessloop/orchestrator/handoff_test.go` |
| 实施要点 | 1) 契约含 `handoff_id`、产物引用（路径+哈希）、五要素摘要、未完成项清单、约束（角色/工具/目录/预算/验收标准）、移交方/接收方；2) 缺五要素或未完成项返 `E_HANDOFF_INCOMPLETE`；3) 接收方输入快照 `injected_from` 不含执行历史；4) 传输复用 a2a-service，不新建传输协议；5) 跨租户/跨项目返 `E_CROSS_TENANT_DENIED` 并审计。 |
| 验收标准 | `go test ./pkg/harnessloop/handoff/...`：缺未完成项被拒；B 的输入快照不含 A 的原始消息序列；共享会话读取 A 历史被拒并产 `security.violation`；跨租户交接被拒。 |
| 复杂度 | M |

### T-P1-21 d 阶段汇聚与失败传播

| 字段 | 内容 |
|---|---|
| 目标 | d 阶段强制汇聚全部子任务产物（存在性+哈希+结论），并按并行集声明的策略传播失败。 |
| 关联需求 | REQ-133、REQ-134、REQ-141；design 4.13.5 / 4.13.6 / 4.13.11 |
| 依赖 | T-P1-19 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/aggregate.go`、[N] `pkg/harnessloop/orchestrator/propagation.go`、[N] `pkg/harnessloop/orchestrator/aggregate_test.go` |
| 实施要点 | 1) 逐项校验产物存在与内容哈希，汇总各子任务结论并标注失败/取消项；2) 汇聚报告作为 d 验收必填输入，缺失返 `E_AGGREGATION_MISSING`，校验不全返 `E_AGGREGATION_INCOMPLETE`；3) `fail-fast` 取消该并行集其余子任务并触发父回退（默认 d→c）；4) `tolerate` 按阈值容忍并写入报告；5) `retry` 耗尽后降级为 fail-fast；6) 未 accepted 产物计入汇聚 → 拒绝 + `security.violation`。 |
| 验收标准 | `go test ./pkg/harnessloop/orchestrator/... -run TestAggregate`：10 子任务报告含 10 项引用+哈希+结论；缺报告时 d 不启动；未 accepted 产物被拒；三种传播策略行为符合 9.9。 |
| 复杂度 | M |

### T-P1-22 角色能力与权限联动

| 字段 | 内容 |
|---|---|
| 目标 | 子任务按角色授予工具集与目录范围，未声明角色/工具集的 Agent 不得被计划引用。 |
| 关联需求 | REQ-135、REQ-136、REQ-161；design 4.13.7 / 4.15.4 |
| 依赖 | T-P0-19、T-P1-17 |
| 涉及文件 | [N] `pkg/harnessloop/subtask/registry.go`、[M] `services/agent-service/internal/handler/grpc_handler.go`、[M] `services/agent-service/internal/service/agent_service.go`、[M] `proto/mcp/mcp.proto`、[M] `services/mcp-service/internal/service/mcp_service.go` |
| 实施要点 | 1) 通过 agent-service 新增 `ListAgentTools/GetAgentRoles` 查询，不本地建注册表；2) 未声明角色返 `E_AGENT_ROLE_UNDECLARED`；3) 角色工具集与阶段矩阵取交集，交集外返 `E_TOOL_NOT_PERMITTED`；4) mcp proto `Tool` 追加 `permission_tags`，`ListTools` 返回该字段；5) 子任务继承父阶段列，不另设矩阵。 |
| 验收标准 | `go test ./pkg/harnessloop/subtask/... -run TestRoleTools`；coder 调用未声明部署工具被拒并审计；verifier 调用写文件被拒；未声明权限标签的 MCP 工具登记返 `E_TOOL_TAG_MISSING`。 |
| 复杂度 | M |

### T-P1-23 模型选择策略与阶段覆盖

| 字段 | 内容 |
|---|---|
| 目标 | 实现 cost_first/quality_first/balanced 三策略打分、阶段覆盖与选择记录持久化。 |
| 关联需求 | REQ-143、REQ-144、REQ-145、REQ-154、U18；design 4.14.2 / 4.14.3 / 4.14.9 |
| 依赖 | T-P0-20 |
| 涉及文件 | [N] `pkg/harnessloop/modelregistry/selector.go`、[N] `pkg/pgindex/ddl/008_model_selection.sql`、[N] `pkg/harnessloop/modelregistry/selector_test.go` |
| 实施要点 | 1) 打分输入为能力、窗口、单价、可用状态与近期可靠性；2) 同输入同输出，记录 `model_id/strategy/candidates[].score`；3) 平局（差值 < 0.02）按 reliability → window → id 稳定排序；4) 阶段配置优先于任务级，未配置继承任务级（U18-A），quality_first 下 d 阶段强模型仅作模板；5) 每次选择发 `model.selected` 事件并写 `model_selections`。 |
| 验收标准 | `go test ./pkg/harnessloop/modelregistry/... -run TestSelector`：三策略结果可复现且可解释；仅 d 配置强模型时 a/b/c/e 用任务级；记录表含阶段×模型×token×成本。 |
| 复杂度 | M |

### T-P1-24 回退链、可用性探测与成本联动

| 字段 | 内容 |
|---|---|
| 目标 | 主模型不可用时按回退链尝试，回退计入预算与限次，切换后重算压缩阈值与预算估算。 |
| 关联需求 | REQ-147、REQ-148、REQ-149、REQ-150、REQ-152、REQ-153、REQ-155、U14；design 4.14.5 ~ 4.14.8 |
| 依赖 | T-P1-23、T-P1-07 |
| 涉及文件 | [N] `pkg/harnessloop/modelregistry/fallback.go`、[N] `pkg/harnessloop/modelregistry/hotreload.go`、[N] `pkg/harnessloop/modelregistry/fallback_test.go` |
| 实施要点 | 1) 每阶段 2 个回退候选，限流或连续 3 次超时触发；2) 每个候选先过能力匹配；3) 单阶段回退 ≤ 3 次，第 4 次转异常分类；4) 回退 token/费用计入任务预算池；5) 切换后重算压缩阈值与预算估算并发配置生效事件；6) 连续 5 次失败标 `unavailable` 并移出自动候选，恢复后 `model.recovered`；7) 运行中禁止切换，例外为回退与经审批升级。 |
| 验收标准 | `go test ./pkg/harnessloop/modelregistry/... -run TestFallback`：单阶段 4 次回退第 4 次不发起；回退预算不超上限；32k→128k 后阈值后移；5 次失败后三策略均不选中。 |
| 复杂度 | L |

### T-P1-25 审批联动（复用 agent-service）

| 字段 | 内容 |
|---|---|
| 目标 | 高危工具命中 `requires_approval` 时挂起并复用 `pkg/agent/approval`，超时不放行，拒绝不重试。 |
| 关联需求 | REQ-163、DFX-S-07、U17-A；design 4.15.6 |
| 依赖 | T-P0-19、T-P0-18 |
| 涉及文件 | [N] `pkg/harnessloop/policy/approval.go`、[N] `pkg/config/harness_loop/approval.yaml`、[M] `pkg/agent/approval/flow.go`、[N] `pkg/harnessloop/policy/approval_test.go` |
| 实施要点 | 1) 一期审批队列留在 agent-service，`GetApprovalStatus` 为薄代理转发；2) 高危规则：删除文件/目录、网络写、部署动作、超阈值成本；3) 审批拒绝返 `E_APPROVAL_REJECTED` 且不重试；超时返 `E_APPROVAL_TIMEOUT` 不放行；4) 审批服务不可达时保持挂起并告警；5) 配置校验拒绝"超时自动通过"取值。 |
| 验收标准 | `go test ./pkg/harnessloop/policy/... -run TestApproval`；部署类工具挂起并可查询状态；审批通过后继续、拒绝后不重试；超时不放行；非高危调用不受审批服务故障影响。 |
| 复杂度 | M |

### T-P1-26 权限优先级完善与审计聚合

| 字段 | 内容 |
|---|---|
| 目标 | 四层优先级（system > tenant > role > task）解析可追溯，按 `rule_id` 聚合拒绝比例。 |
| 关联需求 | REQ-165、REQ-166、REQ-167、REQ-168、REQ-170、DFX-S-07、DFX-P-10、U15；design 4.15.7 / 4.15.9 / 4.15.11 |
| 依赖 | T-P0-19、T-P0-03 |
| 涉及文件 | [N] `pkg/harnessloop/policy/resolver.go`、[N] `pkg/harnessloop/policy/cache.go`、[N] `pkg/harnessloop/policy/audit.go`、[N] `pkg/pgindex/ddl/011_audit_index.sql`、[N] `pkg/harnessloop/policy/resolver_test.go` |
| 实施要点 | 1) 低优先级不得放宽高优先级限制，租户只能收紧（U15-A）；2) 系统硬限制（密钥文件、`.harness/policy/`、`config*.yaml`、`proto/`）任何层不可覆盖；3) 决策缓存以 `policy_hash+tool+path+stage` 为键，配置/审批状态变更即失效；4) 审计写入 `hlp_event.audit`（不新建表），按 `task_id/rule_id/时间` 可查；5) `tool.denied` 与审计结论、`rule_id` 完全一致。 |
| 验收标准 | `go test ./pkg/harnessloop/policy/... -run TestResolver`；任务层 allow + 租户层 deny → 拒绝且 `layer=tenant`；缓存修改配置后下一次立即生效；1000 次调用命中 P95 ≤ 5ms、未命中 ≤ 20ms。 |
| 复杂度 | M |

### T-P1-27 CLI TUI（Bubble Tea）

| 字段 | 内容 |
|---|---|
| 目标 | 交互模式实时展示阶段/attempt/验收/重试/预算与最近事件，断线可重连补拉。 |
| 关联需求 | REQ-097、REQ-098、REQ-099；design 4.11.3 / 4.11.4 |
| 依赖 | T-P0-22、T-P0-09 |
| 涉及文件 | [N] `internal/cli/tui/app.go`、[N] `internal/cli/tui/model.go`、[N] `internal/cli/tui/theme.go`、[N] `internal/cli/tui/views/stage.go`、[N] `internal/cli/tui/views/stream.go`、[N] `internal/cli/tui/views/events.go`、[N] `internal/cli/tui/views/help.go`、[M] `internal/cli/cmd/run.go` |
| 实施要点 | 1) gRPC server stream 驱动，UI 限 10fps、token 100ms 批渲染；2) 展示阶段、attempt、验收结论、重试计数、token/费用、最近事件；3) 验收失败显示 evidence 摘要 ≤ 3 条 + `failed_criteria`；4) 断线显示 reconnecting，按 `last_seq` 补拉；5) 快捷键 q/a/l/m/?。 |
| 验收标准 | `go test ./internal/cli/... -run TestTUIModel`（模型层可测）；阶段变化 1s 内反映；断线 10s 重连后事件补齐；`go build ./cmd/harness-cli` 通过。 |
| 复杂度 | M |

### T-P1-28 大屏基础视图

| 字段 | 内容 |
|---|---|
| 目标 | 新建 `/harness-loop/monitor`，展示卡片网格、生命周期泳道、健康度与异常抽屉，SSE 实时并可补拉。 |
| 关联需求 | REQ-107、REQ-108、REQ-109、REQ-110、REQ-111、REQ-113、REQ-112；design 7.1 / 7.2 / 7.4 / 7.5 |
| 依赖 | T-P0-09、T-P0-21 |
| 涉及文件 | [N] `frontend/src/pages/HarnessLoop/Monitor.tsx`、[N] `frontend/src/pages/HarnessLoop/index.tsx`、[N] `frontend/src/pages/HarnessLoop/hooks/useEventStream.ts`、[N] `frontend/src/pages/HarnessLoop/api/harnessLoop.ts`、[N] `frontend/src/pages/HarnessLoop/types.ts`、[M] `frontend/src/App.tsx`、[M] `frontend/src/components/Layout`（菜单项） |
| 实施要点 | 1) SSE `monitor/stream?last_seq`，200ms 合并 flush，指数退避重连并按 seq 跳号补拉；2) 卡片仅 5 指标（阶段/attempt/健康度/最近 verdict/耗时），50 卡片虚拟滚动；3) ECharts 泳道与健康度仪表，颜色取现有 CSS 变量（3 主色 + 灰阶）；4) 异常抽屉按 `trace_id` 一跳展示事件/日志/验收/检查点；5) `reduceTaskState` 按 seq 归约、`event_id` 去重；6) 暂停/中止二次确认。 |
| 验收标准 | `npm run build`（frontend）通过；`reduceTaskState` 单测覆盖全部事件类型；50 卡片 1s 刷新主线程单帧 ≤ 16ms；断线 10s 重连后状态与 DB 一致；`.tsx` 组件 PascalCase 命名。 |
| 复杂度 | L |

### T-P1-29 大屏多 Agent、模型与权限视图

| 字段 | 内容 |
|---|---|
| 目标 | 父任务可展开子任务树与 DAG 进度，展示模型分布/回退趋势与权限拒绝明细。 |
| 关联需求 | REQ-139、REQ-154、REQ-166；design 7.2 / 7.4 / 7.6 |
| 依赖 | T-P1-28、T-P1-19、T-P1-23 |
| 涉及文件 | [N] `frontend/src/pages/HarnessLoop/components/SubagentTree.tsx`、[N] `frontend/src/pages/HarnessLoop/components/DagProgressBar.tsx`、[N] `frontend/src/pages/HarnessLoop/components/DagTimeline.tsx`、[N] `frontend/src/pages/HarnessLoop/components/ModelStageTable.tsx`、[N] `frontend/src/pages/HarnessLoop/components/ModelDistributionChart.tsx`、[N] `frontend/src/pages/HarnessLoop/components/PolicyDenyDrawer.tsx`、[N] `frontend/src/pages/HarnessLoop/components/HandoffGraph.tsx`、[N] `frontend/src/pages/HarnessLoop/TaskDetail.tsx` |
| 实施要点 | 1) 子任务树缩进 2 层可折叠，显示 role/status/attempt/耗时；2) DAG 进度分段着色（accepted/running/failed/cancelled），并行集甘特按时间轴；3) 模型表显示每阶段生效模型/回退次数/token/成本，卡片只显示短名+回退角标；4) `tool.denied` 折叠为一行并提供 rule_id/layer/参数摘要，不提供"忽略拒绝"按钮；5) handoff 小图只显示契约摘要，不展示原始对话；6) 文案使用工程语义，避免"智能选择/AI 优选"。 |
| 验收标准 | 父任务卡片可展开并跳转子任务详情；DAG 视图默认折叠、展开仅 2 层；`PolicyDenyDrawer` 可显示 rule_id 与 layer；前端单测覆盖 3 个归约函数；构建通过。 |
| 复杂度 | L |

### T-P1-30 PG 迁移整合与双写/开关矩阵

| 字段 | 内容 |
|---|---|
| 目标 | 现有 11 份 DDL（`001_mem` ~ `011_audit_index`）在空库与存量库都完整可重放，存量读写与新索引的开关/双写策略代码化并可回退。 |
| 关联需求 | REQ-121、REQ-058、REQ-117、REQ-118、DFX-C-03、DFX-C-04；design 5.1 / 5.2 / 5.3 / 5.4 / 5.5 |
| 依赖 | T-P0-09、T-P1-04、T-P1-13、T-P1-23、T-P1-25、T-P1-26 |
| 涉及文件 | [M] `pkg/pgindex/ddl/001_mem.sql` ~ `011_audit_index.sql`（共 11 份，按需补幂等）、[M] `pkg/pgindex/ddl/008_model_selection.sql`（补 `idx_model_selection_combo (stage, model_id)`）、[N] `pkg/pgindex/sync_matrix.go`、[N] `pkg/pgindex/migrate_test.go`、[M] `pkg/pgindex/migrate.go`（如需扩展前置） |
| 实施要点 | 1) **空库前置扩展**：`001/002` 使用 `vector(1024)` 且 DDL 内无 `CREATE EXTENSION`，迁移器必须在应用 DDL 前执行 `CREATE EXTENSION IF NOT EXISTS vector`，不得依赖运维手工装；2) 11 份 DDL 编号与依赖顺序固定，全部 `IF NOT EXISTS` + 索引幂等，重复迁移零变更；3) 补 `008` 缺失的 `(stage, model_id)` combo 索引，对齐 design 5.1.4 的三个业务索引；4) 双写矩阵：md→PG 单向为主，旧 Qdrant/Mongo 只读共存，必要时双写可开关；5) 开关关闭时回退既有检索路径且功能可用；6) 事件/检查点结构变更提供向前兼容读取；7) 每份 DDL 附版本号，`schema_migrations` 可查询且 001~011 连续。 |
| 验收标准 | `go test ./pkg/pgindex/... -run "TestMigrateAll|TestMigrateIsIdempotent|TestSchemaMatchesDesign"`（真实 DSN）：**新建空库（不预装扩展）一次迁移成功**、重复迁移无变更、`schema_migrations` 含 001~011 且连续；`model_selections` 存在 `(stage, model_id)` combo 索引；关闭新索引开关后旧路径可用；打开后新数据仅在新索引可查。另需 `go build ./...`、`depcheck` 0 violations。 |
| 复杂度 | M |

### T-P1-31 编排、模型策略与权限接入 runtime 及事件发布

| 字段 | 内容 |
|---|---|
| 目标 | 把 orchestrator 的 DAG 规划、调度、handoff、汇聚，modelregistry 的模型选择、回退与热更新，以及 policy 的四层权限解析、审批与审计接入 harnessloop-service 运行时，并让三类事件真正写入事件仓储。 |
| 关联需求 | REQ-123、REQ-125~134、REQ-139、REQ-143~155、REQ-161~170；design 4.13 / 4.14 / 4.15 |
| 依赖 | T-P1-18、T-P1-19、T-P1-20、T-P1-21、T-P1-23、T-P1-24、T-P1-25、T-P1-26、T-P0-09、T-P0-17 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/orchestrator_bridge.go`、[N] `services/harnessloop-service/internal/service/model_bridge.go`、[N] `services/harnessloop-service/internal/service/policy_bridge.go`、[N] `pkg/harnessloop/orchestrator/publisher_emitter.go`、[M] `pkg/harnessloop/event/types.go`、[M] `services/harnessloop-service/internal/service/runtime.go`、[M] `services/harnessloop-service/cmd/main.go`、[M] `services/harnessloop-service/internal/handler/grpc_handler.go`、[N] `services/harnessloop-service/internal/service/orchestrator_bridge_test.go`、[N] `services/harnessloop-service/internal/service/model_bridge_test.go`、[N] `services/harnessloop-service/internal/service/policy_bridge_test.go` |
| 实施要点 | 1) 新增生产 Emitter：把 `orchestrator.Event`、`modelregistry.ModelEvent` 与 `policy` 事件统一转为 `event.Event`，复用既有 Publisher 分配 seq，不允许各组件自行生成 seq；2) `event/types.go` 的 `knownTypes` 补 `subtask.*`、`handoff.*`、`aggregation.*`、`model.selected`、`model.recovered`、`model.config_applied`、`tool.denied`、`approval.*` 类型，保证 `IsKnownType` 通过；3) runtime 在 b 阶段调用 planner 产出 DAG，c/d 阶段用 scheduler 调度子任务，d 阶段先走汇聚再用汇聚报告作为验收必填输入；4) runtime 每阶段经 `Selector` 选模型、失败走 `FallbackChain`，进入新阶段必须调用 `ResetStage`，热更新由 `HotReloader` 驱动并替换 Registry 快照；5) `SelectionRecorder` 落 `hlp_task.model_selections`，`ModelEvent` 与编排事件共用同一发布通道；6) `Handler.PreflightTool` 与 runtime 工具调用改走 `policy.Resolver`（四层优先级），`policy.Engine` 仅保留为兜底，不再作为唯一入口；7) 权限审计写 `hlp_event.audit`（`kind=tool_decision`、payload 带 `layer`/`rule_id`），`tool.denied` 事件与审计结论、`rule_id` 完全一致；高危工具命中 `requires_approval` 时经 `ApprovalGate` 挂起，超时不放行、拒绝不重试；8) 配置或审批状态变更必须使 `policy` 决策缓存失效；9) 三类特性开关（编排/模型策略/权限四层）各自默认关闭，关闭时完全走现有单 Agent、静态模型与 `policy.Engine` 路径且功能不退化；10) 事件按 tenant/task 隔离，跨租户拒绝仍走 `security.violation`。 |
| 验收标准 | `go test ./services/harnessloop-service/... -run "TestOrchestratorBridge|TestModelBridge|TestPolicyBridge"`；真实任务可查到 `subtask.plan_created`、`subtask.queued`、`aggregation.*`、`model.selected`、`tool.denied` 事件且 seq 连续无缺号；`model_selections` 有对应阶段行；`hlp_event.audit` 中 `tool_decision` 的 `layer`/`rule_id` 与 `tool.denied` 完全一致；危险工具挂起可查询状态，超时不放行；租户层只能收紧、不能放宽 system 硬限制；进入新阶段后回退计数归零；热更新后新任务使用新配置、运行中任务不切换；关闭任一特性开关后任务仍按原路径完成；全仓 `go build ./...`、`depcheck` 0 violations。 |
| 复杂度 | L |

### T-P1-T1 记忆 LRU 与原子写测试

| 字段 | 内容 |
|---|---|
| 目标 | 覆盖 8.4 P1 门槛中的记忆部分，并与 9.1 单元/集成分层对齐。 |
| 关联需求 | REQ-047 ~ REQ-057；design 9.1 / 8.4 |
| 依赖 | T-P1-01、T-P1-02、T-P1-03 |
| 涉及文件 | [N] `pkg/memoryfile/memory_integration_test.go`、[N] `bench/p1/memory-report.md` |
| 实施要点 | 1) 31 主题淘汰、读取提升、pin 豁免；2) 写入中断后文件完整；3) 重复写哈希不变；4) 非法 markdown 恢复；5) 事件序列可还原最终状态。 |
| 验收标准 | `go test -race ./pkg/memoryfile/...` 全绿；LRU 与原子写用例逐条对应 4.6.4/4.6.5；报告留档。 |
| 复杂度 | M |

### T-P1-T2 压缩保真度与 C 方案专项

| 字段 | 内容 |
|---|---|
| 目标 | 按 9.4 验证五要素、配对、JSONL、降级与"引擎位为空不降级"。 |
| 关联需求 | REQ-038 ~ REQ-046；design 9.4 / 8.4 |
| 依赖 | T-P1-07、T-P1-08、T-P1-09 |
| 涉及文件 | [N] `pkg/harnessloop/compaction/fidelity_test.go`、[N] `bench/p1/compaction-report.md` |
| 实施要点 | 1) 五要素齐全率/配对合法率/JSONL 可解析率均 100%；2) 20 样本抽检失真率 ≤ 10%；3) 默认配置不产 degraded、`engine_fragment` 为 0；4) 模板故障 → L1/L2 降级；5) mock `ContextEngine` 开关行为；6) 代码扫描无外部引擎 import。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/...` 全绿；专项断言 5 条全部通过；A/B 报告含 `tokens_after/ratio/downstream_acceptance_rate`。 |
| 复杂度 | M |

### T-P1-T3 索引增量正确性测试

| 字段 | 内容 |
|---|---|
| 目标 | 按 9.5 验证哈希跳过、整文件替换、删除清理、事务回滚、并发与防抖。 |
| 关联需求 | REQ-066 ~ REQ-080；design 9.5 / 8.4 |
| 依赖 | T-P1-11、T-P1-12、T-P1-13 |
| 涉及文件 | [N] `pkg/codeindex/incremental_suite_test.go`、[N] `bench/p1/index-report.md` |
| 实施要点 | 1) touch 不写事务；2) 改单函数整文件替换；3) 删除文件/函数清理边；4) 注入失败回滚且旧索引可检索；5) 并发 10 次最终唯一；6) 1s 5 次保存仅 1 次任务；7) 全量后增量仅变更文件参与。 |
| 验收标准 | `go test ./pkg/codeindex/...` 全绿（含集成标签）；单文件 P95 ≤ 2s、100 文件 P95 ≤ 60s 留档。 |
| 复杂度 | M |

### T-P1-T4 检索指标基线

| 字段 | 内容 |
|---|---|
| 目标 | 建立代码/记忆/知识三类检索的 Recall@K、MRR、RRF 增益与延迟基线。 |
| 关联需求 | REQ-065、REQ-085、REQ-095；design 9.6 / 8.4 |
| 依赖 | T-P1-05、T-P1-14、T-P1-16、T-P1-10 |
| 涉及文件 | [N] `bench/retrieval/queries.jsonl`、[N] `bench/retrieval/run.go`、[N] `bench/p1/retrieval-report.md` |
| 实施要点 | 1) 50 query 人工标注集；2) 跑 fulltext/vector/rrf/rrf+graph 四模式；3) 计算 Recall@K、MRR、RRF 增益、P95；4) 目标：代码 K=20 ≥ 0.75、记忆 K=5 ≥ 0.8、MRR 分别 ≥ 0.55/0.65、增益 ≥ 5%。 |
| 验收标准 | `go run ./bench/retrieval` 产出对比表；指标达标或明确记录差距与原因；报告留档。 |
| 复杂度 | M |

### T-P1-T5 多 Agent、模型与权限审计回归

| 字段 | 内容 |
|---|---|
| 目标 | 按 9.9/9.10/9.11 全量回归多 Agent、模型策略与权限审计一致性。 |
| 关联需求 | REQ-123 ~ REQ-141、REQ-142 ~ REQ-155、REQ-156 ~ REQ-170；design 9.9 / 9.10 / 9.11 / 8.4 |
| 依赖 | T-P1-18 ~ T-P1-26、T-P1-31 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/multiagent_suite_test.go`、[N] `pkg/harnessloop/modelregistry/model_suite_test.go`、[N] `pkg/harnessloop/policy/policy_suite_test.go`、[N] `bench/p1/dag-concurrency.md`、[N] `bench/p1/model-fallback.md`、[N] `bench/p1/policy-audit.md` |
| 实施要点 | 1) 多 Agent：父子层级、子任务状态、DAG 完整性、上限、并发、超时、熔断、三种传播、handoff、汇聚、隔离、预算、事件关联；2) 模型：注册表、三策略、阶段覆盖、能力、切换、回退、可用性、成本联动、embedding、密钥、热更新；3) 权限：标签、矩阵、双层名单、角色、路径变体、ToCToU、写入白名单、exec、自我提权、优先级、审计、事件一致、审批、性能。 |
| 验收标准 | 三套 `go test` 全绿；9.9/9.10/9.11 表格逐条编号对照；三份报告留档；零放行（任何应拒未拒即失败）。 |
| 复杂度 | L |

> P1 合计：实施 31 个 + 测试 5 个 = **36 个任务**。

## 3.3 P2：多语言索引、rerank 增强、调度优化、运维完善与 CLI 主线（22 个任务）

### T-P2-01 TypeScript / Python 解析

| 字段 | 内容 |
|---|---|
| 目标 | 以 tree-sitter（cgo，构建标签隔离）为主、正则为降级，支持 TS/Python 的符号与边提取并标注能力级别。 |
| 关联需求 | REQ-066、REQ-067、REQ-080、U5；design 4.8.1 / 4.8.2 / 8.3 P2-1 |
| 依赖 | T-P1-12、T-P1-13 |
| 涉及文件 | [N] `pkg/codeindex/parser/treesitter_cgo.go`、[N] `pkg/codeindex/parser/treesitter_stub.go`、[N] `pkg/codeindex/parser/regex.go`、[M] `pkg/codeindex/parser/lang.go`、[N] `pkg/codeindex/parser/multilang_test.go` |
| 实施要点 | 1) cgo 实现置于 `//go:build cgo && treesitter`，非 cgo 构建走 stub + 正则；2) 语言注册表声明 `capability: full/regex`；3) 提取函数/类方法/调用/导入；4) 解析失败降级为文件级 chunk 并标 degraded；5) 禁止在无 cgo 环境下编译失败。 |
| 验收标准 | `go test -tags=treesitter ./pkg/codeindex/parser/...` 与 `go test ./pkg/codeindex/parser/...`（无标签）均通过；三语言样例符号/边与人工标注一致率 ≥ 90%；能力级别在索引状态中可查。 |
| 复杂度 | L |

### T-P2-02 图扩展召回（第三路）

| 字段 | 内容 |
|---|---|
| 目标 | 代码检索增加 1 跳调用/导入图扩展召回，作为 RRF 第三路并输出命中来源列表。 |
| 关联需求 | REQ-082、REQ-084；design 4.9.2 / 8.3 P2-2 |
| 依赖 | T-P1-14、T-P1-13 |
| 涉及文件 | [N] `services/harnessloop-service/internal/service/graph_expand.go`、[M] `services/harnessloop-service/internal/service/index_service.go`、[N] `services/harnessloop-service/internal/service/graph_expand_test.go` |
| 实施要点 | 1) 从命中符号沿 `code_edges` 扩展 1 跳（调用方/被调用方）；2) 扩展结果参与 RRF，权重可配；3) 结果 `sources` 含 `graph` 标记；4) 限制扩展节点数防爆炸；5) 图查询按 tenant/project 过滤。 |
| 验收标准 | `go test ./services/harnessloop-service/... -run TestGraphExpand`；查函数名返回其直接调用方；结果含 `sources=[vector,fulltext,graph]`；延迟仍满足 P95 ≤ 500ms。 |
| 复杂度 | M |

### T-P2-03 全量索引与批量性能

| 字段 | 内容 |
|---|---|
| 目标 | 提供全量索引命令与批量事务参数，达成 DFX-P-01/02 的批量与单文件时延。 |
| 关联需求 | REQ-075、REQ-080、DFX-P-01、DFX-P-02；design 4.8.7 / 4.8.8 / 8.3 P2-3 |
| 依赖 | T-P1-13、T-P1-27 |
| 涉及文件 | [N] `pkg/codeindex/batch.go`、[M] `pkg/codeindex/writer.go`、[N] `internal/cli/cmd/index.go`、[N] `bench/repo-500/run.sh`、[N] `bench/p2/index-perf.md` |
| 实施要点 | 1) `index --full` 支持并发度与批量大小参数；2) 全量期间检索读旧索引不阻塞；3) `CREATE INDEX CONCURRENTLY` 用于大表；4) 1000 文件仓库首次全量可查；5) 压测脚本固定硬件与仓库快照。 |
| 验收标准 | 1000 文件全量完成且可查；100 文件批量 P95 ≤ 60s；单文件 P95 ≤ 2s / P99 ≤ 5s；批量期间检索请求成功率 100%；报告留档。 |
| 复杂度 | M |

### T-P2-04 HTTP cross-encoder rerank

| 字段 | 内容 |
|---|---|
| 目标 | 为知识检索增加 HTTP cross-encoder 适配，超时/不可用时降级为融合顺序并标注。 |
| 关联需求 | REQ-091、DFX-P-04、U4；design 4.10.4 / 8.3 P2-4 |
| 依赖 | T-P1-16 |
| 涉及文件 | [N] `services/knowledge-service/internal/rerank/http_reranker.go`、[M] `services/knowledge-service/internal/rerank/reranker.go`、[N] `services/knowledge-service/internal/rerank/http_reranker_test.go` |
| 实施要点 | 1) 实现 `Reranker` 接口的 HTTP 适配（top 20 → top 5）；2) 超时预算独立，失败降级并标 `rerank=degraded`；3) 与 LLM rerank 可配置切换；4) 不在请求路径重试超过 1 次；5) 指标记录 rerank 延迟与降级率。 |
| 验收标准 | `go test ./services/knowledge-service/... -run TestHTTPRerank`（httptest 桩）；服务停用时返回融合顺序并带标记；含 rerank 的 P95 ≤ 2.5s。 |
| 复杂度 | M |

### T-P2-05 压缩引擎适配（二期可选）

| 字段 | 内容 |
|---|---|
| 目标 | 在 `ContextEngine` 预留位上实现可选适配器，默认关闭；不改变单通道语义与事件 schema。 |
| 关联需求 | REQ-039、REQ-045、U16；design 4.5.2 / 4.5.7 / 8.3 P2-5 |
| 依赖 | T-P1-09 |
| 涉及文件 | [N] `pkg/harnessloop/compaction/engine_adapter.go`、[M] `pkg/harnessloop/compaction/engine.go`、[N] `pkg/harnessloop/compaction/engine_adapter_test.go` |
| 实施要点 | 1) 仅当 `compaction.engine.enabled=true` 且适配器可用时调用；2) 失败时 reason 并入 L1 降级，不阻塞任务；3) 调用方与事件 schema 不变；4) 开关关闭时零行为差异；5) 选型与接入以 U16 结论为准，未确认前仅留 mock 适配器。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/... -run TestEngineAdapter`；开关关闭时 `engine_fragment` 计数为 0；打开 mock 后产 `engine_fragment` 且失败按 L1 处理。 |
| 复杂度 | S |

### T-P2-06 知识索引异步化

| 字段 | 内容 |
|---|---|
| 目标 | 文档上传后由 worker 异步索引，四态可观测、失败可重试且不进检索。 |
| 关联需求 | REQ-089、REQ-093、REQ-095；design 4.10.2 / 8.3 P2-6 |
| 依赖 | T-P1-15 |
| 涉及文件 | [N] `services/knowledge-service/internal/indexing/worker.go`、[M] `services/knowledge-service/internal/service/knowledge_service.go`、[N] `services/knowledge-service/internal/indexing/worker_test.go` |
| 实施要点 | 1) 上传即 `pending`，worker 取任务置 `processing`；2) 成功 `ready`、失败 `failed` 并保留原因；3) 失败可从列表重试，指数退避上限可配；4) 更新/删除触发重索引或清理；5) `failed` 永不进检索。 |
| 验收标准 | `go test ./services/knowledge-service/... -run TestIndexWorker`；上传→ready 状态流转正确；注入失败后 failed 且可重试；failed 文档检索为空。 |
| 复杂度 | M |

### T-P2-07 审计与严格租户完善

| 字段 | 内容 |
|---|---|
| 目标 | 启停/中止/删除/重建/配置变更全量审计，新增命名空间严格租户与二次确认。 |
| 关联需求 | REQ-114、REQ-120、DFX-S-01、DFX-S-05、U11；design 4.12.8 / 6.3 / 8.3 P2-7 |
| 依赖 | T-P0-21、T-P1-28 |
| 涉及文件 | [N] `services/gateway/internal/handler/harness_loop_audit.go`、[M] `services/gateway/internal/middleware/middleware.go`、[M] `services/gateway/internal/router/router.go`、[N] `services/gateway/internal/handler/audit_test.go` |
| 实施要点 | 1) 审计字段：操作人/时间/租户/对象/结果/来源 IP；2) 审计不可被普通用户删除；3) `StrictTenant` 配置化路径列表，默认仅 `/harness-loop`；4) 暂停/中止/重建/删除需二次确认；5) 越权返回 403 或空结果并记录越权尝试。 |
| 验收标准 | `go test ./services/gateway/... -run TestAudit`；五类操作均可审计查询；普通用户删除审计失败；缺失租户头 401；存量路由行为不变。 |
| 复杂度 | M |

### T-P2-08 运维：指标、告警、trace 桥接

| 字段 | 内容 |
|---|---|
| 目标 | 暴露 DFX-M-01 全量指标，完善告警规则与 trace 全链路桥接，输出容量报告。 |
| 关联需求 | DFX-M-01、DFX-M-02、DFX-M-03、DFX-P-07、DFX-P-08；design 10.1 ~ 10.6 / 8.3 P2-8 |
| 依赖 | T-P1-19、T-P1-24、T-P1-26、T-P0-09 |
| 涉及文件 | [N] `pkg/harnessloop/event/health.go`、[N] `services/harnessloop-service/internal/metrics/metrics.go`、[M] `services/harness-service/internal/rule/rule.go`、[M] `docker/otel-config.yaml`、[N] `bench/p2/capacity-report.md` |
| 实施要点 | 1) 指标含任务终态、阶段时长、验收通过率、异常分类、重试分布、检查点、压缩比、索引队列/耗时、检索延迟/命中、事件滞后、注入占比、子任务/调度/模型/权限系列；2) 告警规则按 10.4 落地；3) trace 层级 task→stage→acceptance/tool，子任务 task→subtask→tool，handoff 为 subtask 子 span；4) 日志结构化并脱敏；5) 容量估算与资源上限留档。 |
| 验收标准 | `/metrics` 抓取到 DFX-M-01 全指标；按 `trace_id` 可串「事件→阶段→验收→检查点→工具」；告警规则在预置故障下触发；容量报告留档。 |
| 复杂度 | L |

### T-P2-09 公平调度（WFQ）与熔断调优

| 字段 | 内容 |
|---|---|
| 目标 | 同租户多父任务按权重公平分配并发与预算，单任务不可独占，熔断恢复路径可用。 |
| 关联需求 | REQ-138、REQ-128、DFX-R-08、U13；design 4.13.3 / 4.13.8 / 8.3 P2-9 |
| 依赖 | T-P1-19 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/fairness.go`、[M] `pkg/harnessloop/orchestrator/quota.go`、[M] `pkg/harnessloop/orchestrator/circuit.go`、[N] `pkg/harnessloop/orchestrator/fairness_test.go` |
| 实施要点 | 1) WFQ 权重可配，按预估权重预算；2) 单任务 slot/token 硬上限，配额耗尽即排队不抢占；3) 熔断参数可调，恢复需人工复位并审计；4) 暴露 `hlp_scheduler_alloc` 占用视图；5) 与 8/20/20/2 默认值兼容。 |
| 验收标准 | `go test ./pkg/harnessloop/orchestrator/... -run TestFairness`；同租户 3 父任务并发时各配额不超限、无单任务独占；熔断复位后该类子任务可重新调度；报告留档。 |
| 复杂度 | M |

### T-P2-10 多 Agent 高级场景

| 字段 | 内容 |
|---|---|
| 目标 | 支持跨阶段 handoff（c→d）、嵌套 DAG（depth=2）与重规划（新 dag_id），旧 DAG 可查。 |
| 关联需求 | REQ-125、REQ-127、REQ-131、REQ-133；design 4.13.2 / 4.13.4 / 4.13.5 / 8.3 P2-10 |
| 依赖 | T-P1-18、T-P1-20、T-P1-21 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/replan.go`、[M] `pkg/harnessloop/orchestrator/dag.go`、[M] `pkg/harnessloop/orchestrator/handoff_transfer.go`、[N] `pkg/harnessloop/orchestrator/replan_test.go` |
| 实施要点 | 1) 重规划生成新 `dag_id`，旧 DAG 标记 `superseded` 且可查；2) 嵌套展开到 depth=2，第 3 层拒绝；3) 跨阶段 handoff 契约五要素齐备并校验；4) 汇聚兼容多 DAG 版本；5) 重规划事件携带新旧 `dag_id`。 |
| 验收标准 | `go test ./pkg/harnessloop/orchestrator/... -run TestReplan`；二层子任务正确展开、三层被拒；旧 DAG 可 `GET`；跨阶段 handoff 缺要素被拒。 |
| 复杂度 | L |

### T-P2-T1 性能压测

| 字段 | 内容 |
|---|---|
| 目标 | 按 9.7 完成索引/检索/验收/事件/大屏/内存七类压测并留档。 |
| 关联需求 | DFX-P-01 ~ DFX-P-09；design 9.7 / 8.4 |
| 依赖 | T-P2-03、T-P1-28、T-P0-09 |
| 涉及文件 | [N] `bench/p2/perf-suite.md`、[N] `bench/p2/k6/monitor.js`、[N] `bench/p2/scripts/run-index-bench.sh` |
| 实施要点 | 1) 固定硬件与 500 文件基准仓库；2) 覆盖单文件/批量索引、代码/记忆检索、验收、事件端到端、50 卡片大屏、20 并发内存；3) 记录 P95/P99 与资源曲线。 |
| 验收标准 | 七类场景指标全部达标或列明偏差与原因；报告可复现（脚本 + 硬件规格 + 数据集版本）。 |
| 复杂度 | L |

### T-P2-T2 混沌与故障注入

| 字段 | 内容 |
|---|---|
| 目标 | 按 9.8 注入依赖故障与进程级故障，验证无数据损坏且可恢复。 |
| 关联需求 | DFX-R-01 ~ DFX-R-08；design 9.8 / 8.4 |
| 依赖 | T-P1-30、T-P1-19、T-P1-25 |
| 涉及文件 | [N] `bench/chaos/inject.sh`、[N] `bench/chaos/cases.md`、[N] `bench/p2/chaos-report.md` |
| 实施要点 | 1) PG/Mongo/LLM/Redis 停机；2) kill -9、磁盘满、工作区外部改动；3) 子任务卡死、配额打满、模型全不可用、审批服务不可达。 |
| 验收标准 | 全部用例无数据损坏、无静默放行；PG 停机记忆降级可用；Mongo 停机标 `checkpoint_degraded`；审批不可达时高危调用保持挂起；演练记录留档。 |
| 复杂度 | L |

### T-P2-T3 三语言解析准确率与知识评测

| 字段 | 内容 |
|---|---|
| 目标 | 量化 Go/TS/Python 解析准确率与知识检索质量，形成可回归基线。 |
| 关联需求 | REQ-066、REQ-090、REQ-095；design 9.5 / 9.6 / 8.4 |
| 依赖 | T-P2-01、T-P1-16 |
| 涉及文件 | [N] `bench/parser/labels.jsonl`、[N] `bench/parser/accuracy.go`、[N] `bench/p2/parser-accuracy.md`、[N] `bench/p2/knowledge-eval.md` |
| 实施要点 | 1) 三语言样例人工标注符号/边；2) 计算准确率与召回；3) 知识固定数据集跑 faithfulness/relevancy/precision 两次对比；4) 回归阈值写入脚本。 |
| 验收标准 | 三语言准确率 ≥ 90%；知识评测两次结果可对比并定位回归；报告留档。 |
| 复杂度 | M |

### T-P2-T4 公平调度压测与沙箱逃逸模糊测试

| 字段 | 内容 |
|---|---|
| 目标 | 验证配额占用曲线与沙箱逃逸零放行，补齐 8.4 P2 门槛。 |
| 关联需求 | REQ-138、REQ-159、REQ-169、DFX-P-10、U20；design 9.11 / 8.4 |
| 依赖 | T-P2-09、T-P1-26 |
| 涉及文件 | [N] `pkg/harnessloop/policy/sandbox/fuzz_escape_test.go`、[N] `bench/p2/fairness-report.md`、[N] `bench/p2/escape-fuzz.md` |
| 实施要点 | 1) 模糊测试生成路径编码/分隔符/UNC/符号链接/junction 变体；2) Windows 与 Linux 双平台跑同一用例集；3) 同租户 3 父任务采样 slot/token 曲线；4) 单任务独占与抢占用例。 |
| 验收标准 | 逃逸用例 100% 拒绝且零副作用；`go test -fuzz=FuzzPathEscape -fuzztime=60s` 无放行样本；公平性报告含配额占用曲线；双平台 CI 通过。 |
| 复杂度 | L |

### T-P2-11 CLI `loop` 入口与全局看板 URL

| 字段 | 内容 |
|---|---|
| 目标 | 新增 `harness loop` 交互入口：进入类 Claude Code 的 TUI，并常驻显示**全局固定**的看板 URL（本地地址、不带域名、不区分项目）；同一机器任意终端执行同一命令，得到同一个 URL。**（2026-11-19 补充：URL 生成规则与多终端一致性仍有效；`loop` TUI 顶部常驻 URL 的展示已由 T-P2-40 覆盖，URL 改由 `loop start` 在当前终端打印。）** |
| 关联需求 | REQ-107~113、REQ-097~099；design 7.1 / 7.2 / 4.12.6 |
| 依赖 | T-P1-27、T-P1-28、T-P2-03 |
| 涉及文件 | [N] `internal/cli/cmd/loop.go`、[M] `internal/cli/cmd/root.go`、[M] `internal/cli/cmd/run.go`、[M] `internal/cli/tui/app.go`、[M] `internal/cli/tui/model.go`、[M] `internal/cli/config` |
| 实施要点 | 1) `harness loop` 为默认交互入口，`run` 保留脚本化；2) 默认值：`--server` 默认 `127.0.0.1:50013`（可被配置/flag 覆盖），`--tenant` 默认 `default`（可用 `HARNESS_TENANT`/配置覆盖），保证多终端与大屏同租户；3) URL 固定为 `${web_base_url}/harness-loop/monitor`，`web_base_url` 默认 `http://localhost:5174`，**只允许本地地址，不拼接域名**；4) URL 生成规则保留；**TUI 顶部常驻 URL 已由 T-P2-40 移出 `loop`**，`o` 键逻辑保留给后续看板入口；终端支持时输出 OSC 8 超链接；5) 无 `--goal` 时进入输入框，有则直接提交并进入监控；6) 每次 `loop` 新建一个任务，退出时保留 URL 与任务号。 |
| 验收标准 | URL 生成规则与多终端一致性仍按本条验收（两终端得到同一 URL）；`o` 能打开浏览器且落到 `/harness-loop/monitor`；`go test ./internal/cli/...` 全绿；显式 `--server`/`--tenant` 优先于默认值。**URL 在 `loop` TUI 顶部的展示由 T-P2-40 验收替代（`loop start` 输出 URL、`loop` 不显示）。** |
| 复杂度 | M |

### T-P2-12 终端会话标签与多终端识别

| 字段 | 内容 |
|---|---|
| 目标 | 每个终端生成 `session_id` 与可读 `terminal_label`（默认 `hostname:cwd`），随任务提交并暴露在任务视图，使同一看板上的多个 agent 可区分。 |
| 关联需求 | REQ-107~113；design 7.2 / 5.1 |
| 依赖 | T-P2-11、T-P0-02 |
| 涉及文件 | [M] `proto/harnessloop/harnessloop.proto`（`TaskCommand`/`TaskView` 增 `session_id`、`terminal_label`）、[M] `pkg/pb/harnessloop/**`、[M] `services/harnessloop-service/internal/repository/task_repo.go`、[N] `pkg/pgindex/ddl/012_task_session.sql`、[M] `services/harnessloop-service/internal/handler/grpc_handler.go` |
| 实施要点 | 1) 字段可为空，兼容存量任务；2) 迁移用 `012_task_session.sql`，`ADD COLUMN IF NOT EXISTS` + 索引；3) `session_id` 每次 `loop` 新建，`terminal_label` 可配置；4) 列表接口原样返回，不做额外聚合。 |
| 验收标准 | 两个终端并发提交后 `ListTasks` 返回两条带不同 `session_id`/`terminal_label`；空值任务仍可列出；`TestMigrateIsIdempotent` PASS；`go test ./services/harnessloop-service/...` 全绿。 |
| 复杂度 | M |

### T-P2-13 全局多 Agent 看板

| 字段 | 内容 |
|---|---|
| 目标 | 大屏在**不按项目过滤**的前提下展示同租户全部活跃 agent，卡片显示终端标签、阶段、健康度与最近事件；保留最近 20 个终态任务灰显。 |
| 关联需求 | REQ-107~113、REQ-139；design 7.1 / 7.2 |
| 依赖 | T-P2-12、T-P1-29 |
| 涉及文件 | [M] `frontend/src/pages/HarnessLoop/Monitor.tsx`、[M] `frontend/src/pages/HarnessLoop/types.ts`、[M] `frontend/src/pages/HarnessLoop/api/harnessLoop.ts`、[M] `frontend/src/pages/HarnessLoop/components/AgentCard.tsx`、[M] `frontend/src/pages/HarnessLoop/components/AgentCardGrid.tsx`、[M] `frontend/src/pages/HarnessLoop/components/SummaryBar.tsx` |
| 实施要点 | 1) `listTasks` 保持不传 `project_id`，只按 tenant；2) 卡片标题优先显示 `terminal_label`，其次 goal 截断；3) 运行中 + 最近 20 个终态；4) SSE 继续按 tenant 订阅、按 task_id 归约；5) 绝不引入项目维度筛选。 |
| 验收标准 | 两个终端同时跑时大屏出现两张卡片并实时更新；关闭一个终端后卡片转终态灰显；URL 不带 project 参数仍显示全部；`npm test`、`npm run build`、eslint 通过。 |
| 复杂度 | M |

### T-P2-14 TUI 监控面板补齐（PDF 第 1 项）

| 字段 | 内容 |
|---|---|
| 目标 | 在 TUI 内补齐 PDF 第 1 项要求的 Agent 生命周期、健康度、终端输出、产物与异常定位。 |
| 关联需求 | REQ-107~113；design 7.1 / 7.2 / 9.9 |
| 依赖 | T-P2-11、T-P1-19、T-P1-23、T-P1-28 |
| 涉及文件 | [N] `internal/cli/tui/views/subagents.go`、[N] `internal/cli/tui/views/models.go`、[N] `internal/cli/tui/views/health.go`、[M] `internal/cli/tui/model.go`、[M] `internal/cli/tui/app.go` |
| 实施要点 | 1) 子任务树 / DAG 进度；2) handoff 链路；3) 模型选择与回退；4) 健康度与异常抽屉（`security.violation`/`tool.denied`/`approval.*`）；5) 产物与检查点；6) 全部复用后端已有查询，不新增聚合层。 |
| 验收标准 | 真实任务下各面板显示真实数据；73 类事件均可读；`go test ./internal/cli/...` 全绿；真实终端目视通过。 |
| 复杂度 | L |

### T-P2-15 CLI 知识检索入口（PDF 第 5 项）

| 字段 | 内容 |
|---|---|
| 目标 | 提供 `harness kb search/upload/show`，检索结果带来源引用，并可在 TUI 侧栏查看。 |
| 关联需求 | REQ-085~095；design 4.7 / 9.6 |
| 依赖 | T-P1-16、T-P2-11 |
| 涉及文件 | [N] `internal/cli/cmd/kb.go`、[M] `internal/cli/cmd/root.go`、[N] `internal/cli/tui/views/kb.go` |
| 实施要点 | 1) 复用 knowledge-service 现有 RPC；2) 输出标题、片段、来源路径与分数；3) 上传走四态状态机，展示解析进度；4) 不新增后端接口。 |
| 验收标准 | `kb search` 返回带来源引用的结果；`kb upload` 后状态可查；`go test ./internal/cli/...` 全绿。 |
| 复杂度 | M |

### T-P2-T5 CLI 多终端与端到端联动测试

| 字段 | 内容 |
|---|---|
| 目标 | 验证「两个终端 → 同一 URL → 同一看板两个 agent」的完整链路，并补齐 CLI 回归。 |
| 关联需求 | REQ-097~113、REQ-139 |
| 依赖 | T-P2-11 ~ T-P2-15 |
| 涉及文件 | [N] `internal/cli/tui/multi_terminal_test.go`、[N] `bench/p2/cli-multi-terminal.md` |
| 实施要点 | 1) 两终端并发 `loop`，断言 URL 相同、tenant 相同；2) 断言 `ListTasks` 返回两条且带不同标签；3) SSE 实时更新与断线重连；4) TUI 目视记录（截图/终端录制）。 |
| 验收标准 | 两终端链路全部通过；报告留档；真实终端目视无异常。 |
| 复杂度 | M |

### T-P2-16 租户池准入层流控

| 字段 | 内容 |
|---|---|
| 目标 | 在调度层之前增加租户池准入校验：池剩余不足时给出可预期反馈，避免新父任务首字节延迟被最长在跑子任务钉死。 |
| 关联需求 | REQ-128、REQ-138、DFX-R-08；design 4.13.3 / 4.13.8 |
| 依赖 | T-P2-09、T-P2-10 |
| 涉及文件 | [N] `pkg/harnessloop/orchestrator/admission.go`、[M] `pkg/harnessloop/orchestrator/fairness.go`、[M] `pkg/harnessloop/orchestrator/errors.go`、[N] `pkg/harnessloop/orchestrator/admission_test.go`、[M] `pkg/config/harness_loop/scheduler.yaml`（若存在） |
| 实施要点 | 1) 在 `TenantFairness.CanAcquire` 之上提供 `Admit`：租户池剩余 ≥ 1 slot 才接单，不足返回可预期的排队/拒绝；2) 错误码**复用** `E_SCHEDULER_LIMIT_EXCEEDED` 并带 `reason=tenant_slots_full`，不新增错误码（新增码会破坏前端/CLI 错误字典）；若需程序化区分，增加结构化 `details.reason` 字段；3) 新增 `max_subtask_minutes` 配置，默认取 `min(阶段超时, 15min)`，作为衰减窗口上界，与父任务 `TimeoutMinutes` 解耦；4) 准入拒绝/排队需产生可观测记录（事件或结构化日志）；5) 保留调度层超额衰减作为兜底，不删除。 |
| 验收标准 | 同租户存在 15 分钟级慢任务时，新父任务在准入层被拦下并返回 `E_SCHEDULER_LIMIT_EXCEEDED` + `reason=tenant_slots_full`；慢任务完成后新任务自动放行；`max_subtask_minutes` 生效且衰减窗口不再无限；`go test ./pkg/harnessloop/orchestrator/... -run "TestAdmission|TestFairness"` 全绿；`go build ./...`、depcheck 0。 |
| 复杂度 | M |

### T-P2-17 事件游标语义修复（P0 级功能缺陷）

| 字段 | 内容 |
|---|---|
| 目标 | 修复「事件 `seq` 为每任务独立、而 `WatchEvents`/`MonitorStream` 把它当跨任务全局游标」导致的漏事件：50 并发任务时仅收到 11% 事件，大屏卡片状态会停在半路。 |
| 关联需求 | REQ-108、REQ-110、REQ-118；design 4.12 / 4.12.6 / 9.7 |
| 依赖 | T-P0-09、T-P1-28、T-P2-T1 |
| 涉及文件 | [M] `services/harnessloop-service/internal/repository/event_repo.go`、[M] `services/harnessloop-service/internal/handler/grpc_handler.go`、[M] `services/gateway/internal/handler/harness_loop_handler.go`、[N] `services/harnessloop-service/internal/repository/event_cursor_test.go`、[M] `bench/p2/perf-suite.md`（复核 4.1） |
| 实施要点 | 1) 现状：`event_repo.go:130` 用 `MAX(seq) WHERE tenant_id AND task_id` 生成 per-task seq；`WatchEvents` 用 `e.Seq > since` 过滤并把 `NextSinceSeq` 当全局游标，新任务 `seq` 从 1 开始即被永久滤掉；2) 三选一（按优先级）：①游标改 `(timestamp_ms, event_id)` 二元组，兼容 per-task seq；②`seq` 改租户级全局序列（单写者递增，需评估并发与迁移）；③按 taskIDs 分组保存游标；3) 补拉（`last_seq`）与新任务接入必须同时正确：既不重复也不漏；4) 前端/CLI 的 `last_seq` 语义若变化，需同步（`MonitorStream` 与事件协议字段）。 |
| 验收标准 | 50 并发任务下事件收集率 **100%**（复跑 `bench/p2/scripts/run-events.sh` 的 50 任务场景）；`last_seq` 补拉不重不漏；跨任务事件不因新任务 seq 回绕被丢弃；大屏 50 卡片状态不再冻结；`go test ./services/harnessloop-service/... ./services/gateway/...` 全绿；容器 `-race`；`go build ./...`、depcheck 0。 |
| 复杂度 | M |

### T-P2-18 子任务执行与状态闭环

| 字段 | 内容 |
|---|---|
| 目标 | 让子任务从「数据库有行」变成「真正执行、状态可查、失败可定位」，并让模型 plan 与平台 fallback 可区分。 |
| 关联需求 | REQ-128、REQ-139、REQ-141；design 4.13 / 9.9 / 8.4 |
| 依赖 | T-P1-31、T-P2-16（d3f8148 已接线但执行与状态未闭环） |
| 涉及文件 | [M] `services/harnessloop-service/internal/repository/subtask_repo.go`、[M] `services/harnessloop-service/internal/service/{orchestrator_bridge,subtask_executor,runtime}.go`、[M] `pkg/harnessloop/orchestrator/{scheduler,events}.go`、必要时 [M] `pkg/harnessloop/event/types.go` |
| 实施要点 | 1) `SubtaskRepo` 新增 `UpdateStatus`/`UpdateExecution`，状态机 `pending→running→accepted/failed`，禁止回退；在 scheduler 的 Start/Finish/Fail 与 executor 真实流转点回写，保证 DB 与事件一致；2) 定位并修复 c 阶段 agent 调用 `tokens=0`（对照 a 阶段成功调用的 stage_mode/messages/model/tenant/tools 参数），禁止跳过执行或直接标 accepted；3) `subtask.failed` 补 `error`、`response_len`、`model_id`、`duration_ms`、`agent_status`；4) `plan_created` 补 `source=model|fallback` 与 `degraded_reason`；5) 测试覆盖状态回写成功/失败、fallback 标识、失败详情、executor 请求参数。 |
| 验收标准 | 真实任务下 `hlp_task.subtasks.status` 与事件序列一致；单节点子任务 `accepted`，失败时原因可读；`plan_created.source` 可区分模型 plan 与 fallback；`go test ./pkg/harnessloop/orchestrator/... ./services/harnessloop-service/... -count=1` 全绿；`go build -p 1 ./...`、depcheck 0；补齐 d3f8148 五个文件零测试的缺口。 |
| 复杂度 | M |

### T-P2-19 dev-up 全量九服务与启停契约

| 字段 | 内容 |
|---|---|
| 目标 | `dev-up`/`dev-down` 覆盖除 `desktop`、`mcp-demo-server` 外的 9 个微服务与全部基础设施，并产出供 `harness-cli` 自举复用的声明式契约。 |
| 关联需求 | REQ-097~113、DFX-M-05；design 7.1 / 7.2 |
| 依赖 | T-P2-16 |
| 涉及文件 | [M] `scripts/dev-up.sh`、[M] `scripts/dev-down.sh`、[M] `scripts/dev-lib.sh`、[N] `scripts/dev-stack.json`、[M] `docker/docker-compose.data.yaml`、[M] `docs/dev.md`、[M] `README.md`、[M] `services/*/config.example.yaml`（仅补模板字段） |
| 实施要点 | 1) 统一端口矩阵：gateway 9000、chat 50001、knowledge 50002、memory 50003、a2a 50004、mcp 50005、agent 50006（HTTP 50009）、harness 50007、harnessloop 50013、PG 55432、Mongo 27017、Redis 56379、Qdrant 6333、vite 5174；**agent 必须从当前 50002 改回 50006**，同步 harnessloop 的 subtask executor 与 harness-service 的 agent 指向；2) `docker-compose.data.yaml` 增 Qdrant（具名卷 `qdrant-data`，首次可启动）；3) 新增 chat/knowledge/memory/a2a 四个 step 与配置生成（LLM/embedding、Qdrant、Mongo、PG、Redis）；4) `dev-down` 同步清理新增服务与 Qdrant，`--keep-db` 保留 PG/Mongo/Qdrant；5) 新增 `scripts/dev-stack.json`（name/kind/port/command/config_template/health/required），作为 T-P2-21 自举的唯一数据源；**`group` 字段由 T-P2-40 增量追加（infra|agent|board），不改变本任务的 14 项验收**；6) 文档更新为 14 项。 |
| 验收标准 | `dev-down.sh` 后 `dev-up.sh --demo` **14 项健康全绿**；9 个微服务端口均可连；`harnessloop.log` 不再出现 `memory recall disabled`；Qdrant 6333 可访问且 knowledge 上传后集合存在；`dev-stack.json` 可解析且与脚本实际启动项逐项一致；不得留半截栈。 |
| 复杂度 | M |

### T-P2-20 会话式 TUI 与命令面（最高优先）

| 字段 | 内容 |
|---|---|
| 目标 | 把 `loop` 从「一次任务、终态即退出」改成常驻会话：可连续输入、终态回输入框、`/harness loop exit` 只结束当前终端；斜杠命令与执行层只有一份。 |
| 关联需求 | REQ-172、REQ-173、REQ-174、REQ-175；design 4.11.7 |
| 依赖 | T-P2-12 |
| 涉及文件 | [M] `internal/cli/cmd/loop.go`、[M] `internal/cli/tui/{app,model}.go`、[M] `internal/cli/tui/views/*`、[M] `internal/cli/cmd/root.go`（仅注册） |
| 实施要点 | 1) 任务到达终态后不再 `tea.Quit`，回到输入框并保留结果；`--exit-on-complete` 恢复旧行为；2) 同一会话连续提交产生多个任务，`session_id`/`terminal_label` 不变；3) `exit`/`/harness loop exit` 只结束当前进程；4) 命令分发集中到一处，终端与 TUI 斜杠命令复用；5) 不新增依赖，输入框沿用现有实现。 |
| 验收标准 | 同一终端连续提交两条输入 → 两个任务且阶段完整；任务终态后界面仍可输入；两终端各开一个会话，其中一个 exit → 另一个与大屏不受影响；`go test ./internal/cli/...` 全绿。 |
| 复杂度 | M（1~1.5 天） |

### T-P2-21 一条命令拉起后端（Go devstack）

| 字段 | 内容 |
|---|---|
| 目标 | `/harness loop` 检测 127.0.0.1:50013，未起则按 `scripts/dev-stack.json` 自动拉起 **infra+agent 13 项**（不含 board/vite）并等待健康，冷启动一条命令进 TUI。**（T-P2-40 起：board/vite 只由 `/harness loop start` 管理，`loop` 不再拉起看板。）** |
| 关联需求 | REQ-176；design 4.11.7 |
| 依赖 | T-P2-19（`dev-stack.json` 已定稿） |
| 涉及文件 | [N] `internal/devstack/{stack,process,health,config}.go`、[M] `internal/cli/cmd/root.go`、[N] `scripts/verify-stack.sh` |
| 实施要点 | 1) 以 `dev-stack.json` 为唯一数据源（脚本与 Go 共用），禁止第二份服务清单；2) 已存在实例则复用，不重复起；3) 进程 pid/日志落 `.harness/dev`，Windows 用端口反查 + taskkill；4) 失败回滚本次已起进程；5) 与 T-P2-20 文件域隔离：本任务只碰 `internal/devstack/**` 与 `root.go`，不改 `tui/**`。 |
| 验收标准 | 干净状态执行 `harness loop` → **infra+agent 13 项就绪**并进 TUI（board/vite 不在 `loop` 起）；再执行一次 <2s 复用；杀进程后重跑可恢复；失败不留半截栈。 |
| 复杂度 | M（1~2 天，可与 T-P2-20 并行） |

### T-P2-22 看板 start/stop 生命周期

| 字段 | 内容 |
|---|---|
| 目标 | `/harness loop start` 以单例方式启动或复用看板并打印同一 URL；每个终端登记一个引用，`/harness loop stop` 只释放自己的引用；关闭终端不释放；任一引用存在时看板不停，全部释放后才停止且 URL 不可访问。 |
| 关联需求 | REQ-171、REQ-178（方案 A 已定，2026-11-18）；design 4.11.7 行为契约 |
| 依赖 | T-P2-21；方案 A 已定：单例 + 引用计数 + 显式 stop，端口不递增 |
| 涉及文件 | [N] `internal/devstack/board.go`、[M] `internal/cli/cmd/loop.go`、[M] `scripts/dev-stack.json`（board 项） |
| 实施要点 | 1) 单例锁 + 固定端口，第二个 start 只登记引用不重复起；2) `refs[]` 按 session 登记/释放，重复 start/stop 幂等；3) 关闭终端不自动释放（接受僵尸引用）；4) 引用非空时 stop 只打印剩余数量，引用为空才停进程并删除锁；5) 不实现端口递增。 |
| 验收标准 | 两个终端 start → 同一 URL、只有一个实例；关闭任一终端 URL 仍 200；其中一个 stop → URL 仍 200；全部 stop → URL 不可访问；重复 start/stop 不重复计数。 |
| 复杂度 | S（0.5~1 天） |

### T-P2-23 多节点 plan（3 节点）

| 字段 | 内容 |
|---|---|
| 目标 | b 阶段一次拆出 3 个节点并真正分发；验收 `node_count>=3`、`edge_count>0`、`accepted>=3`。 |
| 关联需求 | REQ-123~141；design 4.13 |
| 依赖 | T-P2-18 |
| 涉及文件 | [M] `services/harnessloop-service/internal/service/runtime.go`（提示词常量）、[M] `pkg/config/harness_loop/models.yaml`（阶段模型覆盖） |
| 实施要点 | 1) 提示词改为 3 节点示例 + 明确拆分判据，不再给单节点示例；2) b 阶段指定 `qwen3.7-max`；3) 多节点才算合格，fallback 单节点只能算降级；4) 与 T-P2-24 无文件交集，可并行。 |
| 验收标准 | 真实任务 `subtask_count>=3`、`edge_count>0`、事件含多个 `subtask.started` 且 `accepted>=3`；DB 状态与事件一致。 |
| 复杂度 | S（0.5 天） |

### T-P2-24 runtime 迁 openai-go

| 字段 | 内容 |
|---|---|
| 目标 | `pkg/llm` 底层换成 `github.com/openai/openai-go`，对外接口与装饰器链不变。 |
| 关联需求 | REQ-096、REQ-177；design D4（2026-11-17 修订） |
| 依赖 | 无（可与 T-P2-20/21/23 并行） |
| 涉及文件 | [M] `pkg/llm/**`、[M] `go.mod`/`go.sum` |
| 实施要点 | 1) 保留 `Client` 的 Chat/ChatStream/Embed/EmbedBatch 签名；2) FinishReason、tool_call 聚合、超时/取消、错误分类在适配层归一；3) 现有 mock/测试保留，另补适配层单测；4) 不做 RAG 迁移。 |
| 验收标准 | `go test ./pkg/llm/...` 全绿；真实 LLM 五阶段回归一次 completed；`go build -p 1 ./...`、depcheck 0。 |
| 复杂度 | M（1~2 天） |

### T-P2-25 权限审计真实验证

| 字段 | 内容 |
|---|---|
| 目标 | 用触发 `read_file`/`run_command` 的任务验证 policy 决策、审批与审计一一对应。 |
| 关联需求 | REQ-156~170；design 9.11 |
| 依赖 | T-P2-18（执行器修好后才可能触发工具） |
| 涉及文件 | [N] `bench/p2/policy-audit-e2e.md`、[N] `services/harnessloop-service/internal/service/policy_e2e_test.go` |
| 实施要点 | 1) 任务目标必须诱导工具调用；2) 断言 `audit>0` 且与工具调用逐条对应；3) 越权/高危各一条负向；4) 不改业务代码，仅补测试与报告。 |
| 验收标准 | `audit` 条数等于工具调用次数；越权被拒且留痕；报告留档。 |
| 复杂度 | S（0.5 天） |

### T-P2-26 收口与发布

| 字段 | 内容 |
|---|---|
| 目标 | 两终端演示闭环 + 全量验证 + CHANGELOG + `v0.3.0-p2`。 |
| 关联需求 | design 8.4；全部 P2 任务 |
| 依赖 | T-P2-18~25、T-P2-40~42 |
| 涉及文件 | [M] `CHANGELOG.md`、[M] `docs/dev.md`、发布 tag |
| 实施要点 | 1) 冷启动 `harness loop` → 13 项（infra+agent，不含 board）→ TUI（无 URL、后台无可见窗口、logo/组件框）；2) 第二终端 → 同一看板两张卡；3) `exit`/`stop` 隔离验证；4) 全量 build/test/depcheck；5) 合 main 打 tag。 |
| 验收标准 | 上述 4 条演示全过；`loop` TUI 不显示 URL、后台服务无可见窗口、加粗浅蓝 logo + 组件框（T-P2-42）；`go build -p 1 ./...`、`go test ./services/... ./internal/cli/...`、depcheck 0；tag 已推。 |
| 复杂度 | S（0.5 天） |

### T-P2-27 真实 token 计量（流式 usage）

| 字段 | 内容 |
|---|---|
| 目标 | 让流式调用的真实 usage 从网关一路透传到 `consumed_tokens`；估算只作兜底并显式标记来源，不再把估算值当实际值。 |
| 关联需求 | REQ-096、REQ-045、DFX-M-01；design 4.2 / 4.14 |
| 依赖 | T-P2-24（openai-go 已合入） |
| 涉及文件 | [M] `pkg/llm/{client,openai_adapter}.go`、[M] `pkg/agent/engine_stream.go`、[M] `services/agent-service/internal/service/agent_service.go`、[M] `services/harnessloop-service/internal/service/subtask_executor.go`、必要时 [M] `proto/agent/**` |
| 实施要点 | 1) `buildChatParams` 增 `stream_options.include_usage=true`；2) `chatStream` 不再跳过 `len(choices)==0` 的帧，解析其中的 `usage` 并通过 `ChatStreamChunk` 新字段回传；3) 聚合层把 usage 写入 `ChatResponse.TotalTokens/PromptTokens`，`engine_stream` 的 `Step.TokensUsed` 取真实值；4) `SubtaskExecutor` 优先用真实值，为 0 才估算，并在 DB/事件标 `token_source=actual\|estimate`；5) 若网关不支持 `include_usage`，先实测确认并如实报告，禁止静默把估算值当 actual。 |
| 验收标准 | 真实流式任务下 `consumed_tokens` 等于网关返回的 usage，`token_source=actual`；估算分支未被触发；`go test ./pkg/llm/... ./services/agent-service/... ./services/harnessloop-service/...` 全绿；一次真实任务 a~e completed。 |
| 复杂度 | M（1 天） |

### T-P2-28 子任务产物落盘与契约校验

| 字段 | 内容 |
|---|---|
| 目标 | `file_write` 必须落到 workspace 的约定路径，`output_contract` 校验前能真实读到产物；修掉 `output artifact "cmd/server/healthz.go" is missing`。 |
| 关联需求 | REQ-139、REQ-141；design 4.13.6 / 4.15 |
| 依赖 | T-P2-18 |
| 涉及文件 | [M] `services/mcp-service/**`、[M] `pkg/harnessloop/policy/sandbox/**`、[M] `services/agent-service/internal/**`（工具执行落点）、[M] `bench/p2/**` |
| 实施要点 | 1) 确认 MCP `file_write` 的目标根与任务 workspace 一致（沙箱 root 不是暂存目录）；2) 相对路径按 workspace 解析，禁止写到进程 cwd；3) `output_contract` 校验用同一套路径解析，校验前先落盘再断言；4) 路径逃逸仍必须拒绝（复用 `policy/sandbox`）；5) 补真实任务与负向用例（写 root 外被拒）。 |
| 验收标准 | 真实任务 `file_write cmd/server/healthz.go` 后 workspace 存在该文件；`output artifact ... is missing` 不再出现；越权写 root 外被拒且留痕；`go test ./services/mcp-service/... ./pkg/harnessloop/policy/...` 全绿。 |
| 复杂度 | M（1 天） |

### T-P2-29 父任务终态必须等子任务收口

| 字段 | 内容 |
|---|---|
| 目标 | 子任务未全部到达终态或未 accepted 时，父任务不得 `completed`；带失败完成必须显式标注。 |
| 关联需求 | REQ-128、REQ-139；design 4.1.4 / 4.13.9 |
| 依赖 | T-P2-18、T-P2-23 |
| 涉及文件 | [M] `services/harnessloop-service/internal/service/{orchestrator_bridge,runtime}.go`、[M] `pkg/harnessloop/orchestrator/aggregate.go` |
| 实施要点 | 1) d 阶段聚合时若存在 `running/pending` 子任务，不得返回 complete；2) `accepted` 数为 0 时父任务不得 `completed`；3) `tolerate` 策略下确有失败但已完成时，`terminal_reason=completed_with_failed_subtasks` 并在报告列 `failed_subtasks[]`；4) 不得用「父任务 completed」掩盖子任务未收口。 |
| 验收标准 | 复现 `accepted 0/3` 场景 → 父任务**不得** `completed`；`accepted 3/3` → `completed`；带失败完成时终态可辨；DB 与事件一致；`go test ./services/harnessloop-service/... ./pkg/harnessloop/orchestrator/...` 全绿。 |
| 复杂度 | S（0.5 天） |

### T-P2-30 agent 侧工具循环（活栈真实触发）

| 字段 | 内容 |
|---|---|
| 目标 | 真实任务在 harness loop 中必须产生 `tool_call` 并调用 MCP `read_file`/`run_command`，让权限闸门在活栈上有真实输入。 |
| 关联需求 | REQ-156~170、REQ-139；design 4.15 |
| 依赖 | T-P2-28（工具落点可用） |
| 涉及文件 | [M] `services/agent-service/internal/**`、[M] `pkg/agent/engine_stream.go`、[M] `pkg/llm/**`（流式 tool_calls 解析） |
| 实施要点 | 1) 阶段/子任务执行中产生 `tool_call` chunk，不得只回文本；2) `content` 与 `tool_calls` 配对正确（`AGENT_LLM_EMIT_TOOL_CALL_MESSAGES` 语义）；3) 工具结果回填后继续循环直到 `stage_finish`；4) 活栈真实任务必须触发 ≥1 次工具调用；5) 禁止用测试注入或伪造 audit 冒充活栈。 |
| 验收标准 | 活栈 `tenant-policy` 真实任务 `audit>0` 且与工具调用逐条对应；活栈探针输出 `must_trigger=true`；`go test ./services/agent-service/... ./pkg/agent/... ./pkg/llm/...` 全绿；build/depcheck 0。 |
| 复杂度 | M（1~2 天） |

### T-P2-31 策略集注入与审批可达

| 字段 | 内容 |
|---|---|
| 目标 | 生产代码注入 tenant/role/task 三层策略集，使 `require_approval` 在真实任务中可达；四层解析不再只跑 system 层。 |
| 关联需求 | REQ-156~170；design 4.15.3 |
| 依赖 | T-P2-30 |
| 涉及文件 | [M] `services/harnessloop-service/internal/service/{policy_bridge,runtime}.go`、[M] `pkg/harnessloop/policy/**` |
| 实施要点 | 1) 从配置/存储装配 `TenantPolicy/RolePolicy/TaskPolicy`（当前 grep 零命中）；2) `require_approval` 走 `ApprovalGate`，工单批准/驳回闭环；3) 高危命令不得被 exec 白名单提前拒绝到不了 Classify（调整判定顺序或白名单策略）；4) 审计 rule 与决策一一对应。 |
| 验收标准 | 真实任务触发 `require_approval` → 工单 → 批准放行/驳回 `E_APPROVAL_REJECTED`；审计 rule 对齐；四层命中可辨；测试全绿。 |
| 复杂度 | M（1 天） |

### T-P2-32 事件 seq 单写者与错误可见

| 字段 | 内容 |
|---|---|
| 目标 | 消除 `event.Publisher` 内存 `SeqAllocator` 与 repository `max+1` 双写者；emit 失败不得被 `_ =` 吞，事件不得静默丢失。 |
| 关联需求 | REQ-105、REQ-106、DFX-P-07 |
| 依赖 | 无（影响全局，优先做） |
| 涉及文件 | [M] `pkg/harnessloop/event/**`、[M] `services/harnessloop-service/internal/{repository,service}/**` |
| 实施要点 | 1) 明确 seq 单一分配者（租户级或任务级契约），另一方改为读取/回填；2) 所有 `_ = emit/publish` 改为可见错误（日志 + 指标），不得吞；3) 唯一键冲突时重试或跳过但不丢事件；4) 复跑 50 并发任务事件收集率。 |
| 验收标准 | 50 并发任务事件收集率 **100%**；`task.completed`/`tool.denied` 不再缺失；emit 失败有日志与指标；`go test ./pkg/harnessloop/event/... ./services/harnessloop-service/...` 全绿。 |
| 复杂度 | M（0.5~1 天） |

### T-P2-33 compaction 依赖门禁回归

| 字段 | 内容 |
|---|---|
| 目标 | 修复 `TestFidelityNoExternalEngineImports`：T-P2-24 引入 openai-go 后，其传递依赖被误判为「外部压缩引擎」，导致 compaction 包门禁红。 |
| 关联需求 | REQ-039、REQ-045；design 4.5（C 方案：自研模板摘要为唯一通道） |
| 依赖 | T-P2-24 |
| 涉及文件 | [M] `pkg/harnessloop/compaction/fidelity_test.go`（仅测试） |
| 实施要点 | 1) 只检查 compaction 源文件的**直接 import**，不把 `pkg/llm` 的传递依赖算作压缩引擎；2) 保留原意：直接 import 外部压缩引擎（如 langchain / llmlingua / 独立 sidecar）仍必须失败；3) 补一条负向用例证明门禁没被整体放开；4) 不改 compaction 业务代码。 |
| 验收标准 | `go test ./pkg/harnessloop/compaction/... -count=1` 全绿；负向用例（直接 import 外部引擎）仍 FAIL；`go build -p 1 ./...`、depcheck 0。 |
| 复杂度 | S（0.5 天） |

### T-P2-34 子任务失败重试（ActionRetry 接执行侧）

| 字段 | 内容 |
|---|---|
| 目标 | d→c 回退后，failed 子任务能按重试预算重新执行；当前子任务 FSM 终态无出边，回退只恢复 pending 子任务。 |
| 关联需求 | REQ-128、REQ-131；design 4.13.8（传播策略 retry） |
| 依赖 | T-P2-29 |
| 涉及文件 | [M] `pkg/harnessloop/orchestrator/{propagation,scheduler}.go`、[M] `pkg/harnessloop/subtask/model.go`、[M] `services/harnessloop-service/internal/service/**` |
| 实施要点 | 1) 为 `failed`（及 tolerate 外失败）定义受 attempt 上限约束的重试迁移，禁止无限循环；2) 把 `propagation.ActionRetry` 接到执行侧（当前只有测试引用）；3) 重试次数、事件与 DB 状态一致；4) 超过上限仍 failed 并由父任务按 T-P2-29 语义收口。 |
| 验收标准 | 失败子任务在 d→c 回退后真实重跑并 `accepted`；或达到上限后 `failed` 且事件序列可证；`go test ./pkg/harnessloop/orchestrator/... ./services/harnessloop-service/...` 全绿。 |
| 复杂度 | M（1 天） |

### T-P2-35 orchestrator 开启导致 harnessloop-service 崩溃（P0）

| 字段 | 内容 |
|---|---|
| 目标 | 定位并修复「orchestrator 开启后 harnessloop-service 进程稳定被杀」；多节点与工具循环必须能同时运行。 |
| 关联需求 | REQ-123~141、DFX-R-01；design 7.3 |
| 依赖 | 无（阻塞 T-P2-23 达标） |
| 涉及文件 | [M] `services/harnessloop-service/cmd/**`、`internal/service/**`（启动与生命周期）、必要时 [M] `pkg/harnessloop/orchestrator/**` |
| 实施要点 | 1) 复现：orchestrator=on 启动后数秒进程退出；2) 抓取退出码、stderr、Windows 事件日志与 panic；3) 区分 panic/os.Exit/信号/端口冲突；4) 修复后 orchestrator=on 连续跑 3 次真实任务不崩；5) 补回归测试或启动自检。 |
| 验收标准 | `orchestrator=on` 启动 10 分钟不退出；连续 3 个真实任务完成且进程存活；退出原因可查；`go test ./services/harnessloop-service/...` 全绿。 |
| 复杂度 | M（1 天） |

### T-P2-36 HARNESSLOOP_LLM_* 注入导致启动 exit(1)（P0）

| 字段 | 内容 |
|---|---|
| 目标 | 修复注入 `HARNESSLOOP_LLM_API_KEY/BASE_URL/MODEL` 后 harnessloop-service 静默 exit(1)，否则真实 LLM 配置无法在隔离栈生效。 |
| 关联需求 | REQ-096、REQ-101；design 4.11.5 |
| 依赖 | 无 |
| 涉及文件 | [M] `services/harnessloop-service/cmd/**`、`internal/config/**`、[M] `docs/dev.md` |
| 实施要点 | 1) 复现并保留 stderr（当前静默）；2) 校验配置加载顺序（环境变量 vs yaml）与必填项；3) 错误必须打印可读原因并非零退出；4) 补配置单测。 |
| 验收标准 | 注入三变量后服务正常启动并完成一次真实任务；缺少变量时给出明确错误而非静默 exit(1)；配置测试全绿。 |
| 复杂度 | S（0.5 天） |

### T-P2-37 审批结论传递到执行面与工单幂等

| 字段 | 内容 |
|---|---|
| 目标 | harness 批准后，agent/mcp 执行面真正执行该命令；同一调用在有效期内复用同一工单，消除「反复重发 → 每次新工单」的风暴。 |
| 关联需求 | REQ-160~166；design 4.15.3 |
| 依赖 | T-P2-31 |
| 涉及文件 | [M] `services/harnessloop-service/internal/service/approval_client.go`（**唯一业务扩域，方案 B，2026-11-19 拍板**）、[M] `services/harnessloop-service/cmd/**`（仅装配）、[M] `pkg/harnessloop/policy/**`（token 结构 + HMAC 签发/校验）、[M] `services/mcp-service/internal/**`（验签 + nonce 一次性）、[M] `services/agent-service/internal/**`（仅透传 token，**不得持有签发密钥**） |
| 实施要点 | 1) 签发权威在 harness：`ApprovalClient.Await` 观察到 `approved` 时签发一次性 `approval_token`（task_id + tool + args_digest + expiry + nonce + HMAC）；2) agent-service 只透传 token，mcp 执行前验签、校验绑定/过期/一次性，无 token 仍走各自白名单（fail closed）；3) 同一 `(task_id, tool, args_digest)` 在有效期内复用同一工单，**在 `ApprovalClient` 建单前**去重；4) 审计记录 token 校验结果、nonce 消费与工单复用；5) **不得**把签发器放进 agent-service 侧；不得改 `runtime.go`、`orchestrator/**`、`internal/cli/**`、`frontend/**`、`go.mod`。 |
| 验收标准 | 真实任务批准后 `rm scratch/probe.txt` 真的执行（文件消失）；同一调用不产生第二个工单；未批准/过期/重复 nonce 的 token 被拒且留痕；`go test ./services/harnessloop-service/... ./services/mcp-service/... ./services/agent-service/... ./pkg/harnessloop/policy/...` 全绿；回报必须包含签发者、密钥持有方、工单复用证据。 |
| 复杂度 | M（1~2 天） |

### T-P2-38 sandbox 点路径语义（`read_file(".")` 必败）

| 字段 | 内容 |
|---|---|
| 目标 | `Resolve(root, ".")` 解析为 workspace 根，不得再报 `E_UNSAFE_PATH: empty path`；子任务 `read_file(".")` / `list_dir(".")` 可用。 |
| 关联需求 | REQ-139、REQ-160；design 4.15 |
| 依赖 | 无 |
| 涉及文件 | [M] `pkg/harnessloop/policy/sandbox/realpath.go`、[M] `pkg/harnessloop/policy/sandbox/sandbox_test.go`、[M] `services/mcp-service/internal/tools/tools.go`（FileReadTool 目录分支）、[M] `services/mcp-service/internal/tools/workspace_test.go` |
| 实施要点 | 1) 区分 `.` 与空串：`.` 解析为 root，空串仍拒绝；2) `..`、绝对路径、UNC、URL 仍拒绝；3) `FileReadTool` 在 `IsDir()` 时渲染目录列表（条目名 + 是否目录 + 大小），**不递归、不跟随符号链接、条目上限 200**，避免大目录撑爆上下文；4) 目录条目仍受 `resolveWorkspacePath` 约束，越权路径照旧拒绝；5) 补 `Resolve(root, ".") == root`、`read_file(".")` 目录渲染、空目录、条目上限用例；6) 不新增 `list_dir` 工具（另属后续语义）。 |
| 验收标准 | `go test ./pkg/harnessloop/policy/sandbox/... ./services/mcp-service/internal/tools/...` 全绿；真实任务 c 阶段 `read_file(".")` 返回目录列表；`..`/绝对路径仍被拒；`AllowedSubpaths` 下 `.` 仍拒（最小权限）。 |
| 复杂度 | S（0.5 天） |

### T-P2-39 fail-fast 集合重入语义（重试 vs 重新规划）

| 字段 | 内容 |
|---|---|
| 目标 | fail-fast 失败导致同组兄弟被 `cancelled` 时，d→c 重入不能只重试 failed 节点；按集合语义选择「整组重开」或「重新规划」，避免预算空转与兄弟永久取消。 |
| 关联需求 | REQ-128、REQ-131、REQ-139；design 4.13.8 |
| 依赖 | T-P2-34 |
| 涉及文件 | [M] `pkg/harnessloop/orchestrator/{propagation,scheduler}.go`、[M] `pkg/harnessloop/subtask/model.go`、[M] `services/harnessloop-service/internal/service/orchestrator_bridge.go` 及测试 |
| 实施要点 | 1) 区分失败集合策略：`tolerate` 只重试 failed 节点（保持 T-P2-34）；`fail-fast` 整组进入重入决策；2) fail-fast 重入时把同组 `cancelled` 兄弟一并重开为 pending，或走 replan 重新规划；3) 预算按集合级 attempt 上限，禁止同一集合无限重开；4) 事件与 DB 状态一致，`retry.scheduled` 带集合标识；5) 不得改变「终态不可随意回退」的既有约束，仍通过 `RequeueForRetry` 一类受控入口。 |
| 验收标准 | fail-fast 集合重入后同组全部重跑或重新规划，不出现「只重试一个节点 + 兄弟永久取消」；集合级预算耗尽后整组 failed；tolerate 场景行为不变；`go test ./pkg/harnessloop/orchestrator/... ./services/harnessloop-service/...` 全绿。 |
| 复杂度 | M（1 天） |

### T-P2-40 `loop` / `loop start` 职责解耦与按组启动

| 字段 | 内容 |
|---|---|
| 目标 | 彻底拆开「看板 + URL」与「agent 会话」：`/harness loop start` 只在当前终端启动/复用共享看板并打印 URL，不进入 TUI；`/harness loop` 只进入当前终端的对话式 TUI，不显示 URL、不启动 board/vite。 |
| 关联需求 | REQ-171、REQ-178；design 4.11.7 |
| 依赖 | T-P2-21（自举）、T-P2-22（board 单例 + refs） |
| 涉及文件 | [M] `scripts/dev-stack.json`（14 项补 `group`）、[M] `internal/devstack/contract.go`、[M] `internal/devstack/manager.go`、[M] `internal/cli/cmd/loop.go`、[M] `internal/devstack/{contract,manager}_test.go`、[M] `internal/cli/cmd/*_test.go` |
| 实施要点 | 1) 契约仍保持 14 项，只新增 `group: infra|agent|board` 字段（`scripts_test.go` / `contract_test.go` 的 14 项断言不变）；2) `Item.Group` + 校验：4 个数据容器与 redis 为 `infra`，9 个服务为 `agent`，vite 为 `board`，board 项必须 `group=board`；3) `Manager` 新增 `UpGroups(ctx, groups...)` / `EnsureGroups(ctx, groups...)`，`Up`/`Ensure` 保持全量语义（`dev up`、`dev status` 不变）；4) `loop.go` 的 `ensureBackend` 改为 `EnsureGroups("infra","agent")`，冷启动不再触碰 vite；5) `loop` 不再把 `tui.URLFor(...)` 传给 TUI（`WebURL=""`），URL 只由 `loop start` 打印（**覆盖 T-P2-11 的“TUI 顶部常驻 URL”展示项，URL 生成规则不变**）；6) 帮助文案与 `loop start`/`loop` 的 Short/Long 同步改写，明确 `loop start` 不进 TUI、`loop` 不显示 URL。 |
| 验收标准 | 冷启动执行 `/harness loop`：5174 不监听、TUI 无 URL 行；执行 `/harness loop start`：当前终端输出固定 URL、不进入 TUI、不打开新窗口；`/harness dev up` 仍启动 14 项；`go test ./internal/devstack/... ./internal/cli/...` 全绿；`go build ./...`、depcheck 0；契约项数仍为 14。 |
| 复杂度 | M（1 天） |

### T-P2-41 后台服务无可见窗口（Windows）

| 字段 | 内容 |
|---|---|
| 目标 | `/harness loop` 冷启动 infra+agent 时，9 个服务进程与 4 个容器全部在后台隐藏运行；用户可见窗口始终只有当前终端，不再出现「很多终端」。 |
| 关联需求 | REQ-171（不弹多终端）；2026-11-19 用户口径 |
| 依赖 | T-P2-40 |
| 涉及文件 | [M] `internal/devstack/process_windows.go`、[M] `internal/devstack/process.go`（如需要）、[M] `internal/devstack/manager_test.go` |
| 实施要点 | 1) `startDetached` 的 `CreationFlags` 由 `DETACHED_PROCESS` 改为 `DETACHED_PROCESS | CREATE_NO_WINDOW`（0x08000000），确保 `cmd /c` 与子进程不分配可见控制台窗口；2) 保持进程存活（不能改用会随父终端退出的前台启动）；3) 启动日志仍写 `.harness/dev/logs/<name>.log`，不把 stdout 回显到当前终端；4) `resolveCommand` 继续使用 `.\` 相对路径 + `.exe`，不用引号绝对路径（避免 cmd 转义问题）；5) 只改 Windows 实现，Linux/macOS 启动语义不变。 |
| 验收标准 | 在 Windows 冷启动 `/harness loop` 后：`Get-Process cmd` / `conhost` 不新增可见窗口（`MainWindowTitle` 为空），`dev status` 的 9 个服务全部健康；当前终端保持 TUI 不被抢占；容器进程不弹窗；`go test ./internal/devstack/...` 全绿；Linux 容器内 `go test` 不受影响。 |
| 复杂度 | S（0.5 天） |

### T-P2-42 Claude 式 TUI：加粗浅蓝 logo + 组件框

| 字段 | 内容 |
|---|---|
| 目标 | `/harness loop` 进入的 TUI 具备 Claude Code 式的对话界面：顶部加粗 `harness loop` logo（浅蓝色），输入/状态区用带边框的组件框呈现，去掉看板 URL 行。 |
| 关联需求 | REQ-171（会话式 TUI）；2026-11-19 用户视觉口径 |
| 依赖 | T-P2-20（会话式 TUI 已落地）、T-P2-40 |
| 涉及文件 | [M] `internal/cli/tui/views/theme.go`、[M] `internal/cli/tui/views/stage.go`、[M] `internal/cli/tui/views/urlbar.go`、[M] `internal/cli/tui/prompt.go`、[M] `internal/cli/tui/model.go`、[M] 对应 `*_test.go` |
| 实施要点 | 1) 新增 `views.RenderLogo()`：`harness loop` 文本，`lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#8FD3FF"))`，`NO_COLOR` 时降级为纯文本加粗；2) 新增 `views.RenderBox(title, body, width)`：圆角边框（`lipgloss.RoundedBorder()`），宽度随终端自适应（最小 40、最大 120）；3) `PromptModel.View()` 改为「logo + 说明 + 输入组件框 + 快捷键提示」，输入行放进组件框；4) `Model.View()` 顶部渲染 logo（不再渲染 URL 栏），session 待输入时输入框套组件框；流式/事件区保持现有组件，只在视觉上对齐；5) 不使用新的 TUI 依赖（继续 bubbletea + lipgloss）；6) 测试：`NO_COLOR` 降级、边框字符存在、宽度自适应、logo 文本与颜色样式断言；`go test ./internal/cli/tui/...` 全绿。 |
| 验收标准 | 真实终端执行 `/harness loop`：顶部可见加粗浅蓝 `harness loop`，输入区有圆角组件框，界面无 URL 行；`NO_COLOR=1` 时无 ANSI 颜色但仍有 logo 文本与边框；TUI 宽度 < 60 时不溢出；单测覆盖 logo/框/降级；用户目视确认（无 TTY 时如实说明，不得宣称已目视）。 |
| 复杂度 | M（1 天） |

### T-P2-43 契约显式声明栈探针（probe:true）

| 字段 | 内容 |
|---|---|
| 目标 | 把 `Ensure`/`EnsureGroups` 的复用判定从「选中集里有没有 harnessloop」改为契约显式声明的 `probe:true` 项，消除新增分组（如 `obs`）时的隐式耦合；未声明时回退 harnessloop，保持旧契约兼容。 |
| 关联需求 | REQ-176；T-P2-19 契约唯一数据源、T-P2-40 分组启动 |
| 依赖 | T-P2-40 |
| 涉及文件 | [M] `scripts/dev-stack.json`、[M] `internal/devstack/contract.go`、[M] `internal/devstack/manager.go`、[M] 对应 `*_test.go` |
| 实施要点 | 1) `Item` 增 `Probe bool`（json:`probe`）；`Contract.ProbeItem()` 返回 `probe:true` 项；2) `Validate`：probe 至多一个、必须是 process、health 必须非空、必须属于 `agent` 组；3) `dev-stack.json` 只给 `harnessloop` 标 `"probe": true`，14 项/端口/命令不变；4) `Ensure`/`EnsureGroups` 统一规则：取「选中集 ∩ probe 项」，命中则用该探针健康判定复用，未命中则退化为「选中项全健康才复用」；5) probe 未声明时回退 `HarnessLoop()`（harnessloop），旧契约与脚本不受影响；6) 测试覆盖：probe 缺失回退、probe 在选中集、probe 不在选中集、多个 probe 拒绝、probe 非 process 拒绝、`obs` 新组不因命名产生隐式耦合。 |
| 验收标准 | 契约仍 14 项；`EnsureGroups("obs")` 只按 `obs` 组自身健康判定，不再依赖 harnessloop 命名；`EnsureGroups(infra,agent)` 仍复用 50013；未声明 probe 的旧 fixture 仍按 harnessloop 回退；`go test ./internal/devstack/... ./internal/cli/...`、`go build -p 1 ./...`、depcheck 0。 |
| 复杂度 | S（0.5 天） |

### T-P2-44 devstack 配置渲染补齐模型凭据与入口 agent，并透传阶段错误

| 字段 | 内容 |
|---|---|
| 目标 | `dev up` 冷启动后即可真实调用模型：配置渲染注入 `.harness/dev/env` 的 LLM 凭据、把 `entry_agent` 固定为已注册的 `main-agent`；阶段失败保留上游错误文本，不再一律显示 `unclassified stage error`。 |
| 关联需求 | REQ-176；T-P2-21 自举、T-P2-40 分组启动 |
| 依赖 | T-P2-21、T-P2-40 |
| 涉及文件 | [M] `internal/devstack/{config,config_test}.go`、[M] `scripts/dev-stack.json`（agent env）、[M] `services/harnessloop-service/internal/service/stage_runner.go` |
| 实施要点 | 1) `RenderConfig` 解析 `.harness/dev/env`，仅向 YAML 的 `llm:` 段注入 `provider/api_key/base_url/model`，MCP `tools/services` 下的 key 不得覆盖；2) `entry_agent` 渲染为 `main-agent`（模板里的 `default` 只是占位符，agent 注册表无 default）；3) agent 项补 `AGENT_LLM_EMIT_TOOL_CALL_MESSAGES=true`；4) `classifyStageError` 在未分类分支保留 `streamErr`（截断 500 字符），`content_filter` 分支不变；5) env 缺失时保留占位符，让服务暴露真实配置错误。 |
| 验收标准 | 真实 `harness-cli run --goal "hi" --workspace . --no-tui` → `completed`，a~e 各 `attempts=1 accepted=true`；`conf/agent.yaml` 不再含 `your-api-key`；`conf/harnessloop.yaml` 的 `entry_agent=main-agent`；MCP server key 保持原值；失败事件包含上游错误文本；`go test ./internal/devstack/... ./services/harnessloop-service/... ./internal/cli/...`、`go build -p 1 ./...`、depcheck 0。 |
| 复杂度 | S（已修复 `1b8dd91`） |

### T-P2-45 `loop` 默认直答、`/task` 才走 a~e

| 字段 | 内容 |
|---|---|
| 目标 | `/harness loop` 的输入默认走 agent 普通对话（非 stage mode），TUI 直接显示流式回复；只有显式 `/task <goal>`（或 `--goal`）才创建 a~e 任务。消除「发 hi 却跑五阶段、b 无子任务、d 被 exec 白名单拒绝」的错配。 |
| 关联需求 | REQ-171~175；design 4.11.7；用户 2026-11-19 拍板 A（直答优先） |
| 依赖 | T-P2-20（会话式 TUI）、T-P2-40（loop/loop start 解耦） |
| 涉及文件 | [M] `internal/cli/cmd/loop.go`、[M] `internal/cli/tui/{model,session,app}.go`、[M] `internal/cli/tui/views/*`（新增对话渲染）、[M] `internal/cli/cmd/root.go`（agent 连接/flag）、[M] 对应 `*_test.go` |
| 实施要点 | 1) CLI 新增 agent-service 连接（默认 `127.0.0.1:50006`，可用 `--agent-server`/`HARNESS_AGENT_SERVER` 覆盖），复用 `pkg/pb/agent` 已生成的 `ExecuteStream`，不改 proto；2) `SessionConfig` 增加 `Chat func(ctx, message) (<-chan ChatChunk, error)`；`Model` 增加 chat 消息列表与流式状态；3) `handleSessionSubmit` 判定：`/task ` 前缀 → 现有 `CreateTask`（a~e）；其他 → `Chat`；`--goal` 仍一次性 a~e；4) 直答请求：`EntryAgent=main-agent`、`StageMode=false`（不写 `__stage_mode__`）、携带 tenant/session/workspace；5) `ExecuteStream` 的 `response/token` 追加到 assistant 消息，`error` chunk 显示在对话中（不得静默）；6) TUI 默认视图显示「用户消息 + assistant 流式回复 + 输入框」，`/task` 仍切回现有 stage/事件视图；7) `/harness loop start`、`loop stop` 与看板 URL 一律不动。 |
| 验收标准 | 真实 TTY 执行 `/harness loop` 发 `hi`：不创建 task、不出现 a~e 阶段，几秒内显示 assistant 回复；`/task 实现 xxx`：创建 task 并显示原有 a~e 视图；`loop --goal xxx` 仍走 a~e；`ExecuteStream` 报错时对话区显示具体错误；`go test ./internal/cli/...`、`go build -p 1 ./...`、depcheck 0；`loop start` 仍只打印 URL。 |
| 复杂度 | M（1~2 天） |

> P2 合计：实施 45 个 + 测试 5 个 = **50 个任务**。

---

# 4. 任务依赖图与关键路径

## 4.1 P0 依赖图

```mermaid
flowchart LR
  A01[T-P0-01 骨架] --> A02[T-P0-02 proto]
  A01 --> A03[T-P0-03 PG]
  A01 --> A04[T-P0-04 FSM]
  A02 --> A08[T-P0-08 事件schema]
  A02 --> A20[T-P0-20 模型注册表]
  A02 --> A18[T-P0-18 服务]
  A03 --> A07[T-P0-07 仓储]
  A03 --> A09[T-P0-09 事件持久化]
  A03 --> A15[T-P0-15 检查点]
  A03 --> A19[T-P0-19 权限]
  A03 --> A21[T-P0-21 网关]
  A04 --> A05[T-P0-05 attempt]
  A05 --> A06[T-P0-06 finish/guard]
  A06 --> A10[T-P0-10 验收上下文]
  A10 --> A11[T-P0-11 checks]
  A11 --> A12[T-P0-12 verdict/rollback]
  A12 --> A13[T-P0-13 异常/重试]
  A13 --> A14[T-P0-14 no_finish]
  A07 --> A17[T-P0-17 runtime]
  A12 --> A17
  A13 --> A17
  A15 --> A17
  A19 --> A16[T-P0-16 stage_runner]
  A20 --> A16
  A06 --> A16
  A16 --> A17
  A08 --> A09
  A17 --> A18
  A18 --> A21
  A18 --> A23[T-P0-23 开关]
  A16 --> A22[T-P0-22 CLI]
  A21 --> A22
  A04 --> T1[T-P0-T1]
  A07 --> T1
  A12 --> T2[T-P0-T2]
  A09 --> T3[T-P0-T3]
  A15 --> T3
  A22 --> T3
  A19 --> T4[T-P0-T4]
  A20 --> T4
```

## 4.2 P1 依赖图

```mermaid
flowchart LR
  M01[T-P1-01 记忆格式] --> M02[T-P1-02 LRU]
  M01 --> M03[T-P1-03 原子写]
  M02 --> M04[T-P1-04 mdsync]
  M03 --> M04
  M04 --> M05[T-P1-05 混合召回]
  M05 --> M06[T-P1-06 注入]
  R10[T-P1-10 RRF] --> M05
  C07[T-P1-07 压缩触发] --> C08[T-P1-08 模板摘要]
  C08 --> C09[T-P1-09 配对/降级/AB]
  I11[T-P1-11 watcher] --> I12[T-P1-12 解析]
  I12 --> I13[T-P1-13 增量事务]
  I13 --> I14[T-P1-14 检索API]
  R10 --> I14
  K15[T-P1-15 知识状态机] --> K16[T-P1-16 引用/rerank]
  R10 --> K15
  S17[T-P1-17 subtask] --> S18[T-P1-18 DAG]
  P22[T-P1-22 角色权限] --> S18
  S18 --> S19[T-P1-19 调度]
  S17 --> H20[T-P1-20 handoff]
  P22 --> H20
  S19 --> G21[T-P1-21 汇聚]
  MR23[T-P1-23 模型策略] --> MR24[T-P1-24 回退]
  C07 --> MR24
  POL25[T-P1-25 审批] --> POL26[T-P1-26 优先级/审计]
  P19 --> POL25
  CLI22 --> TUI27[T-P1-27 TUI]
  M09 --> TUI27
  M09 --> MON28[T-P1-28 大屏]
  MON28 --> MON29[T-P1-29 多Agent视图]
  S19 --> MON29
  MR23 --> MON29
  M04 --> DB30[T-P1-30 迁移/双写]
  I13 --> DB30
  M03 --> TT1[T-P1-T1]
  C09 --> TT2[T-P1-T2]
  I13 --> TT3[T-P1-T3]
  I14 --> TT4[T-P1-T4]
  K16 --> TT4
  M05 --> TT4
  S18 --> TT5[T-P1-T5]
  MR24 --> TT5
  POL26 --> TT5
  S18 --> X31[T-P1-31 接入runtime+模型+权限]
  S19 --> X31
  G21 --> X31
  MR24 --> X31
  POL26 --> X31
  X31 --> TT5
```

## 4.3 P2 依赖图

> 只画 P2 内部依赖边；P0/P1 前置（如 T-P1-13/19/27/28/31）见各任务「依赖」字段。`T-P2-18~25`、`T-P2-11~15`、`T-P2-40~42` 已展开为逐条边。

```mermaid
flowchart LR
  subgraph G1["检索与RAG"]
      P01["T-P2-01 以 tree-sitter（cg…"]
      P02["T-P2-02 代码检索增加 1 跳调用/导入图…"]
      P03["T-P2-03 提供全量索引命令与批量事务参数，…"]
      P04["T-P2-04 为知识检索增加 HTTP cro…"]
      P05["T-P2-05 在 ContextEngine …"]
      P06["T-P2-06 文档上传后由 worker 异步…"]
      P07["T-P2-07 启停/中止/删除/重建/配置变更…"]
      P08["T-P2-08 暴露 DFX-M-01 全量指标…"]
      P09["T-P2-09 同租户多父任务按权重公平分配并发…"]
      P10["T-P2-10 支持跨阶段 handoff（c→…"]
      PTT1["T-P2-T1 按 9.7 完成索引/检索/验收…"]
      PTT2["T-P2-T2 按 9.8 注入依赖故障与进程级…"]
      PTT3["T-P2-T3 量化 Go/TS/Python …"]
      PTT4["T-P2-T4 验证配额占用曲线与沙箱逃逸零放行…"]
  end
  subgraph G2["CLI 与看板"]
      P11["T-P2-11 新增 harness loop …"]
      P12["T-P2-12 每个终端生成 session_i…"]
      P13["T-P2-13 大屏在**不按项目过滤**的前提…"]
      P14["T-P2-14 在 TUI 内补齐 PDF 第 …"]
      P15["T-P2-15 提供 harness kb se…"]
      P16["T-P2-16 在调度层之前增加租户池准入校验：…"]
      P17["T-P2-17 修复「事件 seq 为每任务独立…"]
      P20["T-P2-20 把 loop 从「一次任务、终态…"]
      P21["T-P2-21 /harness loop 检测…"]
      P22["T-P2-22 /harness loop st…"]
      P40["T-P2-40 彻底拆开「看板 + URL」与「…"]
      P41["T-P2-41 /harness loop 冷启…"]
      P42["T-P2-42 /harness loop 进入…"]
      PTT5["T-P2-T5 验证「两个终端 → 同一 URL…"]
  end
  subgraph G3["执行与治理"]
      P18["T-P2-18 让子任务从「数据库有行」变成「真…"]
      P19["T-P2-19 dev-up/dev-down …"]
      P23["T-P2-23 b 阶段一次拆出 3 个节点并真…"]
      P24["T-P2-24 pkg/llm 底层换成 git…"]
      P25["T-P2-25 用触发 read_file/ru…"]
      P26["T-P2-26 两终端演示闭环 + 全量验证 +…"]
      P27["T-P2-27 让流式调用的真实 usage 从…"]
      P28["T-P2-28 file_write 必须落到 …"]
      P29["T-P2-29 子任务未全部到达终态或未 acc…"]
      P30["T-P2-30 真实任务在 harness lo…"]
      P31["T-P2-31 生产代码注入 tenant/ro…"]
      P32["T-P2-32 消除 event.Publish…"]
      P33["T-P2-33 修复 TestFidelityN…"]
      P34["T-P2-34 d→c 回退后，failed 子…"]
      P35["T-P2-35 定位并修复「orchestrat…"]
      P36["T-P2-36 修复注入 HARNESSLOOP…"]
      P37["T-P2-37 harness 批准后，agen…"]
      P38["T-P2-38 Resolveroot, . 解…"]
      P39["T-P2-39 fail-fast 失败导致同组…"]
  end
  
    P03 --> PTT1
    P03 --> P11
    P11 --> P12
    P12 --> P13
    P11 --> P14
    P11 --> P15
    P09 --> P16
    P10 --> P16
    P16 --> P18
    P16 --> P19
    P12 --> P20
    P19 --> P21
    P21 --> P22
    P18 --> P23
    P20 --> P24
    P18 --> P25
    P18 --> P26
    P19 --> P26
    P20 --> P26
    P21 --> P26
    P22 --> P26
    P23 --> P26
    P24 --> P26
    P25 --> P26
    P40 --> P26
    P41 --> P26
    P42 --> P26
    P24 --> P27
    P18 --> P28
    P18 --> P29
    P23 --> P29
    P28 --> P30
    P30 --> P31
    P24 --> P33
    P29 --> P34
    P23 --> P35
    P31 --> P37
    P34 --> P39
    P21 --> P40
    P22 --> P40
    P40 --> P41
    P20 --> P42
    P40 --> P42
    P40 --> P43
    P21 --> P44
    P40 --> P44
    P20 --> P45
    P40 --> P45
    P01 --> PTT3
    P09 --> PTT4
    P11 --> PTT5
    P12 --> PTT5
    P13 --> PTT5
    P14 --> PTT5
    P15 --> PTT5
```

## 4.4 关键路径

1. **P0 主关键路径（15 环）**：`T-P0-01 → T-P0-02 → T-P0-03 → T-P0-04 → T-P0-05 → T-P0-06 → T-P0-10 → T-P0-11 → T-P0-12 → T-P0-16 → T-P0-17 → T-P0-18 → T-P0-21 → T-P0-22 → T-P0-T3`。
2. **P0 次关键路径**：`T-P0-01 → T-P0-03 → T-P0-15 → T-P0-17`（检查点是 runtime 的前置，破坏它将直接推迟 P0 出口）。
3. **P1 主关键路径（5 环）**：`T-P1-01 → T-P1-03 → T-P1-04 → T-P1-05 → T-P1-06`。
4. **P1 次关键路径**：`T-P1-17 → T-P1-18 → T-P1-19 → T-P1-21 → T-P1-T5`（多 Agent 线，环数多、并行度高，易成为实际瓶颈）。
5. **P2 关键路径**：`T-P2-01 → T-P2-03 → T-P2-T1`；`T-P2-09 → T-P2-T4`。

> 关键路径上的任务不得并行压缩：每个任务都必须先有可执行验收，再进入下游。

---

# 5. 测试任务与 design 9.1 ~ 9.11 对齐

| design 章节 | 内容 | 落地任务 | 阶段 |
|---|---|---|---|
| 9.1 测试分层 | 单元/集成/E2E/性能/混沌/安全 | T-P0-T1、T-P0-T3、T-P1-T1 ~ T5、T-P2-T1 ~ T4 | P0/P1/P2 |
| 9.2 状态机穷举 | 8 合法边、非法边、跳过、终态、并发、崩溃点 | T-P0-T1 | P0 |
| 9.3 验收假阳/假阴 | 诱导、空产物、伪造证据、未验证冲突、只读、越权回退、panic | T-P0-T2 | P0 |
| 9.4 压缩保真（C 方案） | 五要素、配对、JSONL、降级、引擎位专项 | T-P1-T2 | P1 |
| 9.5 索引增量正确性 | touch、单函数、删除、回滚、并发、debounce、全量后增量 | T-P1-T3、T-P2-T3 | P1/P2 |
| 9.6 检索指标 | Recall@K、MRR、RRF 增益、延迟 | T-P1-T4、T-P2-T3 | P1/P2 |
| 9.7 性能压测 | 索引/检索/验收/事件/大屏/内存 | T-P2-T1 | P2 |
| 9.8 混沌与故障注入 | PG/Mongo/LLM/进程/磁盘/子任务/配额/审批 | T-P2-T2 | P2 |
| 9.9 多 Agent 协作 | 父子层级、7 态、DAG、上限、并发、超时、熔断、传播、handoff、汇聚、隔离、预算、事件 | T-P1-T5 | P1 |
| 9.10 模型选择 | 注册表、三策略、阶段覆盖、能力、切换、回退、可用性、成本、embedding、密钥、热更新 | T-P1-T5、T-P0-T4（能力子集） | P0/P1 |
| 9.11 权限与沙箱 | 标签、矩阵、双层名单、角色、路径变体、ToCToU、写入、exec、提权、优先级、审计、审批、性能 | T-P0-T4（P0 子集）、T-P1-T5、T-P2-T4 | P0/P1/P2 |

**安全负向用例落地要求**

1. REQ-010（状态机防篡改）、REQ-019（验收只读）、REQ-141（跨租户/未验收汇入/无限拆分）、REQ-168（自我提权）、REQ-169（沙箱绕过）必须各自至少 1 条负向用例，断言「拒绝 + 审计留痕 + 无副作用」。
2. 负向用例与功能同批合入，禁止以"后续补测试"方式延期。
3. 每个阶段的出口必须附「拒绝比例报告」或「逃逸用例通过率」，作为 8.4 留档产物。

---

# 6. 风险与阻塞项

## 6.1 阻塞结论

- **P0 无阻塞**：所有 P0 任务均可在现有默认值下开工。
- P1 有 6 项需在指定任务开工前确认（见 6.2），未确认时按默认值实现并在任务验收中标注「按默认值，待 U 项复核」。

## 6.2 待确认项与最晚给值时点

| 编号 | 问题 | 默认值（当前设计） | 最晚给值时点 | 影响任务 |
|---|---|---|---|---|
| U2 | 任务总超时默认值 | 8h | **T-P0-13 开工前**（重试表与预算校验共用） | T-P0-13、T-P0-17 |
| U3 | 长期记忆主题上限 | 软上限 50 + 告警 | **T-P1-02 开工前** | T-P1-02 |
| U4 | reranker 实现 | P1 用 LLM，P2 评估 HTTP | T-P1-16 开工前 | T-P1-16、T-P2-04 |
| U5 | TS/Python 是否允许 cgo | 允许（构建标签隔离） | T-P2-01 开工前（不阻塞 P1） | T-P2-01 |
| U6 | 事件保留期 | 30 天 | T-P1-30 开工前 | T-P0-09（保留期参数）、T-P1-30 |
| U7 | embedding 维度是否固定 1024 | 固定 1024 | **T-P1-04 开工前**（表结构 `vector(1024)` 返工成本高） | T-P1-04、T-P1-13 |
| U8 | 是否需要 KB 细粒度 ACL | 第一期不需要 | T-P1-15 开工前 | T-P1-15 |
| U9 | CLI 是否支持 Windows 本机 runtime | 需要 | **T-P0-19 开工前**（realpath 平台归一化） | T-P0-19、T-P1-03、T-P1-11 |
| U10 | 项目 md 是否放项目目录 `.harness/` | 接受 | T-P1-01 开工前 | T-P1-01 |
| U11 | 存量软租户是否一并收紧 | 仅新增命名空间 | **T-P0-21 开工前** | T-P0-21、T-P2-07 |
| U12 | 大屏是否多租户切换与权限分级 | 只读 + 运维角色 | T-P1-28 开工前 | T-P1-28 |
| U14 | 回退模型清单粒度与触发阈值 | 每阶段 2 候选，限流或连续 3 次超时 | T-P1-24 开工前 | T-P1-24 |
| U15 | 租户策略能否覆盖阶段矩阵 | 只可收紧，放宽需平台审批 | T-P1-26 开工前 | T-P1-26 |
| U16 | 压缩引擎适配接入时机 | 二期按压缩比/延迟评估 | T-P2-05 开工前（P1 不受影响） | T-P2-05 |
| U20 | Windows junction/reparse 处理 | A+B 兜底 | **T-P0-19 开工前** | T-P0-19、T-P2-T4 |

**已确认、无需再议**：U1-A、U13-A、U17-A（二期转 B）、U18-A、U19-A。

## 6.3 主要技术风险

| 风险 | 影响 | 缓解任务 |
|---|---|---|
| 数据层二元化（PG + Qdrant/Mongo）导致一致性缺口 | 检索结果不一致、回归难定位 | T-P0-03、T-P1-30、T-P2-T2 |
| Windows 路径语义（大小写、短名、junction）导致沙箱漏判 | 本地 CLI 模式被绕过 | T-P0-19、T-P2-T4 |
| 模型回退后验收标准不适配，产生假阴/假阳 | 任务抖动、成本上升 | T-P0-12、T-P1-24、T-P1-T5 |
| 权限三处来源（阶段矩阵/角色/任务）漂移 | 计划可做而运行被拒 | T-P0-19、T-P1-22、T-P1-26 |
| 多 Agent 并发与预算耦合导致饥饿 | 单任务独占、其他任务排队 | T-P1-19、T-P2-09 |
| 压缩质量不足导致后续阶段验收下降 | 返工与长时任务失败 | T-P1-09、T-P1-T2 |
| 事件量放大导致 PG 写入瓶颈 | 大屏滞后、审计丢失 | T-P0-09、T-P1-26（审计复用）、T-P2-08 |

---

# 7. 执行顺序与并行化建议

## 7.1 P0 建议波次

| 波次 | 任务 | 并行说明 |
|---|---|---|
| Wave 0 | T-P0-01 | 单人完成，其他人可先读 spec/design 与准备测试数据 |
| Wave 1 | T-P0-02、T-P0-03、T-P0-04 | 三条线可并行：契约、数据、状态机；T-P0-04 不依赖 PG |
| Wave 2 | T-P0-05、T-P0-06、T-P0-07、T-P0-08、T-P0-19、T-P0-20 | FSM 线（05/06）、仓储线（07）、事件线（08）、安全线（19）、模型线（20）互不阻塞 |
| Wave 3 | T-P0-09、T-P0-10、T-P0-11、T-P0-12、T-P0-13、T-P0-14、T-P0-15 | 验收线（10→11→12）、重试线（13→14）、检查点（15）、事件落库（09）可并行 |
| Wave 4 | T-P0-16、T-P0-17 | 16 需 19/20 就绪；17 是本阶段汇合点，建议单人负责 |
| Wave 5 | T-P0-18、T-P0-23 | 服务化与开关 |
| Wave 6 | T-P0-21、T-P0-22 | 网关与 CLI 可并行（都依赖 18） |
| 贯穿 | T-P0-T1 ~ T4 | 每个 Wave 结束即跑对应测试，T4 在 19/20 完成后立即启动 |

## 7.2 P1 并行分组

| 组 | 任务 | 建议人数/Agent | 前置 |
|---|---|---|---|
| 记忆组 | T-P1-01、02、03、04、05、06 | 1~2 | P0 完成；U7/U10 已确认 |
| 压缩组 | T-P1-07、08、09 | 1 | P0-20 完成 |
| 索引组 | T-P1-10、11、12、13、14 | 1~2 | P0-03 完成 |
| 知识组 | T-P1-15、16 | 1 | T-P1-10 完成；U4/U8 已确认 |
| 多 Agent 组 | T-P1-17 ~ 22 | 2 | P1-17 先行，18/19/21 串行 |
| 模型组 | T-P1-23、24 | 1 | P0-20 完成；U14 已确认 |
| 权限组 | T-P1-25、26 | 1 | P0-19 完成；U15 已确认 |
| 前端组 | T-P1-27、28、29 | 1~2 | P0-09/21 完成；U12 已确认 |
| 数据组 | T-P1-30 | 1 | 与记忆/索引组协同 |
| 测试 | T-P1-T1 ~ T5 | 与各功能组同人负责 | 对应功能完成后立即执行 |

## 7.3 建议的第一批可执行任务（前 5 个）

1. **T-P0-01** 工程骨架与代码生成流水线（所有人解除阻塞的前提）。
2. **T-P0-02** proto/harnessloop 契约（接口先行，前端/网关/CLI 可据此并行）。
3. **T-P0-03** PG 基础与 P0 表结构（状态与事件的落点）。
4. **T-P0-04** FSM 状态集与转移表（核心语义，纯逻辑可快速验收）。
5. **T-P0-19** 权限最小集（与 01/03 并行启动，避免"先开门再装锁"）。

> 第 5 项可与第 2~4 项并行，由独立人员/Agent 负责；T-P0-20 同理可提前并行。

## 7.4 不建议并行的组合

1. T-P0-15（检查点）与 T-P0-17（runtime）：前者接口变更会直接导致后者返工。
2. T-P1-18（DAG 校验）与 T-P1-19（调度）：并行集会反复调整，建议串行。
3. T-P1-26（权限优先级）与 T-P2-07（严格租户）：都改 resolver/middleware，易冲突。
4. T-P2-03（全量索引）与 T-P2-T1（压测）：压测必须在索引实现稳定后进行。

---

# 8. 需求覆盖自检

| 需求模块 | REQ 区间 | 覆盖任务 | 状态 |
|---|---|---|---|
| 5.1 FSM 状态机 | REQ-001 ~ 010 | T-P0-04/05/06/07/17/23、T-P0-T1、T-P0-T4 | 已覆盖 |
| 5.2 阶段验收 Agent | REQ-011 ~ 020 | T-P0-10/11/12、T-P0-T2、T-P0-T4 | 已覆盖 |
| 5.3 异常与重试 | REQ-021 ~ 030 | T-P0-13/14/17、T-P1-T5 | 已覆盖 |
| 5.4 检查点与恢复 | REQ-031 ~ 037 | T-P0-15、T-P0-T3 | 已覆盖 |
| 5.5 上下文压缩 | REQ-038 ~ 046 | T-P1-07/08/09、T-P1-T2、T-P2-05 | 已覆盖 |
| 5.6 markdown 记忆 | REQ-047 ~ 057 | T-P1-01/02/03、T-P1-T1 | 已覆盖 |
| 5.7 记忆检索与注入 | REQ-058 ~ 065 | T-P1-04/05/06、T-P1-T4 | 已覆盖 |
| 5.8 代码索引 | REQ-066 ~ 080 | T-P1-11/12/13、T-P2-01/03、T-P1-T3 | 已覆盖 |
| 5.9 混合检索 RRF | REQ-081 ~ 086 | T-P1-10/14、T-P2-02、T-P1-T4 | 已覆盖 |
| 5.10 知识广场 | REQ-087 ~ 095 | T-P1-15/16、T-P2-04/06、T-P2-T3 | 已覆盖 |
| 5.11 CLI 与 runtime | REQ-096 ~ 102 | T-P0-18/22、T-P1-27、T-P0-T3 | 已覆盖 |
| 5.12 事件与大屏 | REQ-103 ~ 114 | T-P0-08/09、T-P1-28/29、T-P2-07/08 | 已覆盖 |
| 5.13 平台集成与兼容 | REQ-115 ~ 122 | T-P0-16/21/23、T-P1-04/15/30 | 已覆盖 |
| 5.14 多 Agent 协作 | REQ-123 ~ 141 | T-P1-17 ~ 22、T-P2-09/10、T-P1-T5 | 已覆盖 |
| 5.15 模型选择 | REQ-142 ~ 155 | T-P0-20、T-P1-23/24、T-P1-T5 | 已覆盖 |
| 5.16 工具权限与沙箱 | REQ-156 ~ 170 | T-P0-19、T-P1-22/25/26、T-P2-07、T-P0-T4、T-P1-T5、T-P2-T4 | 已覆盖 |
| DFX-P | P-01 ~ P-12 | T-P1-13/14、T-P1-05、T-P1-19、T-P1-26、T-P2-03/08、T-P2-T1 | 已覆盖 |
| DFX-R | R-01 ~ R-08 | T-P0-03/15、T-P1-13、T-P1-19、T-P2-T2 | 已覆盖 |
| DFX-S | S-01 ~ S-08 | T-P0-19/21、T-P1-25/26、T-P2-07、T-P0-T4 | 已覆盖 |
| DFX-M | M-01 ~ M-05 | T-P0-08/09、T-P1-09、T-P2-08 | 已覆盖 |
| DFX-C | C-01 ~ C-05 | T-P0-23、T-P1-01/30、T-P0-21 | 已覆盖 |

> 覆盖结论：**170 条 REQ + 38 条 DFX 全部映射到至少一个任务**；无未覆盖模块。

---

# 附录 A 常用验收命令速查

```bash
# 编译与依赖边界
go build ./...
go run ./scripts/depcheck

# proto
make proto-harnessloop
go build ./pkg/pb/harnessloop/...

# P0 核心单测
go test -race ./pkg/harnessloop/fsm/...
go test ./pkg/harnessloop/acceptance/... ./pkg/harnessloop/retry/...
go test ./pkg/harnessloop/policy/... ./pkg/harnessloop/modelregistry/...
go test ./pkg/harnessloop/event/... ./pkg/harnessloop/checkpoint/...

# P0 集成 / E2E
docker compose --profile test up -d
go test -tags=integration ./services/harnessloop-service/... ./internal/cli/...

# P1
go test ./pkg/memoryfile/... ./pkg/codeindex/... ./pkg/rrf/...
go test ./pkg/harnessloop/compaction/... ./pkg/harnessloop/orchestrator/... ./pkg/harnessloop/subtask/...
go test -tags=integration ./services/memory-service/... ./services/knowledge-service/...

# P2
go test -tags=treesitter ./pkg/codeindex/parser/...
go test -fuzz=FuzzPathEscape -fuzztime=60s ./pkg/harnessloop/policy/sandbox/...
go run ./bench/retrieval

# 前端
cd frontend && npm run build && npm test
```

---

# 附录 B 阶段出口检查单

| 阶段 | 检查项 | 判定 |
|---|---|---|
| P0 | 五阶段可完整跑通；c 失败回 b；d 失败回 c/b；abort 幂等；kill 后 30s 恢复；越权工具全拒且有 `rule_id`；非工具模型不能启动 c 阶段 | 全绿才进入 P1 |
| P1 | 31 主题淘汰正确；md 改后 10s 可检索；压缩五要素齐全且默认不产 degraded；单文件索引 P95 ≤ 2s；三子任务并行且父阶段不变；模型三策略可复现；审批拒绝不重试 | 全绿才进入 P2 |
| P2 | 三语言准确率 ≥ 90%；100 文件批量 P95 ≤ 60s；50 卡片单帧 ≤ 16ms；混沌无数据损坏；逃逸模糊测试零放行；公平调度无独占 | 全绿方可发布 |