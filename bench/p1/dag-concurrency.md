# T-P1-T5 · DAG 并发与多 Agent 协作回归报告（design 9.9 / 8.4 P1）

- 套件：`pkg/harnessloop/orchestrator/multiagent_suite_test.go`
- 用例：**29 个 `TestMultiAgent*`**（9.9 主表 26 行 + 调度性能 1 个 + 辅助断言）
- 真实依赖：PostgreSQL 55432（`hlp_task.task_runs`/`subtasks`）；事件全部走
  `event.Publisher` + `orchestrator.PublisherEmitter`，按 `task_id` 断言 `seq = 1..N` 连续无缺号。
- 执行命令：

```bash
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
HARNESS_REQUIRE_PG=1 \
go test ./pkg/harnessloop/orchestrator/ -run 'TestMultiAgent' -count=1 -v
```

- 结论：**26 行中 23 PASS / 3 PARTIAL / 0 FAIL**。PARTIAL 均为“拒绝路径已实测，
  但 design 要求的审计/租户仓储能力未实现”，详见文末边界。

## 逐条对照（编号取自 design 9.9 原表的「用例组 / 用例」原文，不自拟序号）

| 编号（9.9 用例组 / 用例） | 断言 | 结论 | 备注 |
|---|---|---|---|
| 父子层级 / 父 b 阶段产出 3 子任务 | 父 `current_stage` 仍为 b；3 个子任务 id 独立、`attempt_no=1`、可持久化重读 | PASS | `TestMultiAgentParentChildHierarchy`，真实 PG 写入 + 读回 |
| 层级 / 任一子任务失败 | 父 `current_stage` 不变，仅汇聚视图回写 | PASS | `TestMultiAgentChildFailureKeepsParentStage`；Aggregate 对 failed 终态返回完整报告并把该项记为 fail |
| 子任务状态机 / 强制未验收置 accepted | 拒绝 + `E_SUBTASK_ACCEPTANCE_REQUIRED`；DB CHECK 亦拒绝 | PASS | Go 状态机与 `Repo.Transition` 均拒绝；DB SQLSTATE 23514（**约束名实为自动生成 `subtasks_check`，非 design 的 `ck_subtask_accepted`**） |
| DAG 完整性 / 计划缺 edges（>1 节点） | b 验收 fail，`failed_criteria` 指出缺失 | PASS | 返回 `E_DAG_INCOMPLETE` |
| DAG / 构造环 a→b→a | `E_DAG_CYCLE` | PASS | |
| DAG / 有依赖两节点放同一并行集 | `E_DAG_PARALLEL_CONFLICT` | PASS | |
| 上限 / 21 个直接子任务 | `E_SUBTASK_LIMIT_EXCEEDED` | PASS | |
| 上限 / 第 3 层拆分 | `E_SUBTASK_DEPTH_EXCEEDED` | PASS | |
| 不可拆 / 含外部审批点的单元发起拆分 | `E_SUBTASK_NOT_SPLITTABLE` | PASS | `RequiresApproval=true` |
| 并发 / 可并行集 12 节点 | 同时 running ≤ 8，其余 pending 且发 `subtask.queued` | PASS | 8 running / 4 pending |
| 并发 / 租户已有 18 并发 Agent | 新子任务排队，不失败 | PASS | TenantGate(20)，2 running / 2 pending |
| 超时 / 超过 `min(父剩余,15min)` | `subtask.failed(category=timeout)`，父按传播策略推进 | PASS | 事件 payload 实测 `category=timeout`；Propagator 回退 d→c |
| 熔断 / 4 个同类子任务 3 失败 | 剩余同类不调度 + `subtask.circuit_open` | PASS | 按 `ClassKey(role|paths|acceptance)` 构造同类 |
| 传播 / `fail-fast` 且 1 个失败 | 并行集其余 cancelled；父按 `d→c` 回退 | PASS | Propagator.Apply 真实取消 4 个 |
| 传播 / `tolerate(0.2)` 且 5 中 1 失败 | 父继续；汇聚报告标注失败项 | PASS | `ActionContinue` + `Tolerated=true` + `Failures` 保留 |
| 传播 / `retry` 重试耗尽 | 降级为 fail-fast | PASS | `AttemptNo=2`（RetryMaxAttempts=2）→ cancel_set + d→c |
| handoff / 契约缺未完成项 | `E_HANDOFF_INCOMPLETE`，交接被拒 | PASS | |
| handoff / A→B 后检查 B 的输入快照 | 不含 A 原始消息序列；`injected_from` 不含 `executor_history` | PASS | 快照仅 `contract.*` 与 `runtime.injection:*`；transport 收到 1 个 envelope |
| handoff / 尝试以共享会话方式读取 A 历史 | 拒绝 + `security.violation` | PARTIAL | 注入源 `executor_history/history/conversation/workspace_snapshot` 全部 `E_HANDOFF_HISTORY_FORBIDDEN` 且 4 条 `security.violation`；但**没有真实 a2a 共享会话读取 API**，只覆盖契约层白名单 |
| handoff / 跨租户 A→B | `E_CROSS_TENANT_DENIED` + 审计 | PARTIAL | 拒绝与 `security.violation`（payload 含 `E_CROSS_TENANT_DENIED`）已实测；**orchestrator 没有 `hlp_event.audit` 写入端口**，“审计”仅体现为事件 |
| 汇聚 / 10 个子任务完成 | 报告含 10 项产物引用 + 哈希 + 结论；d 验收可读 | PASS | `RequireForAcceptance` 通过，`AcceptanceInput` 含 `aggregation_report/items/summary` |
| 汇聚 / 缺汇聚报告即启动 d 验收 | `E_AGGREGATION_MISSING` | PASS | nil 与空报告均拒绝 |
| 汇聚 / 未 accepted 产物计入汇聚 | 拒绝 + `security.violation` | PASS | `E_SUBTASK_ACCEPTANCE_REQUIRED` + `security.violation` |
| 隔离 / 查询他租户父任务的子任务 | 返回空（不泄露存在性） | PARTIAL | 以他租户 parent id 查询返回 0 行；scheduler 跨租户 Enqueue 拒绝；但 **`subtask.Repo.ListByParent(parentTaskID,dagID)` 没有 tenant/project 参数**，知道 parent id 即可读到行（仓储层缺口） |
| 预算 / 子任务消耗 | 计入父预算池，无独立账户（表无 `budget_*` 列） | PASS | `Repo.AddConsumed` 回写 `task_runs.consumed_*`；`information_schema` 确认 subtasks 无 `budget_*`；ParentQuota 唯一池 |
| 事件 / 按 `parent_task_id` 查询 | 可还原 DAG 顺序与并行区间；`handoff.*` 带 `handoff_id` | PASS | 真实 Publisher 分配 seq，`1..N` 连续；started 事件含 `dag_id`/`dag_node_id`/`parent_task_id`；handoff.created 含 `handoff_id` |

## 8.4 P1 门槛对照

| 8.4 条目 | 断言 | 结论 | 备注 |
|---|---|---|---|
| 多 Agent 并发 / 失败传播 / handoff / 汇聚 | 上述 9.9 行全量回归 | PASS（PARTIAL 见上） | 并发上限、三种传播、handoff 契约、10 项汇聚均已实测 |
| 必须留档产物：DAG 并发与熔断报告 | 本文件 | PASS | 性能与熔断数据见下 |

## 性能口径与实测区间

**DAG 调度（并发上限 8，12 节点并行集）**
- 口径：构造 12 节点并行集，`Enqueue` 后单次 `Dispatch` 的墙钟耗时；50 次采样取 P95。
- 宿主（Windows，4 次运行）：P95 **1.68–3.05ms**，min 0ms，max 4.74–7.50ms。
- 容器（`golang:1.22-bookworm`，4 次运行）：P95 **0.73–1.84ms**，min 0.31ms，max 2.31–6.22ms。
- 阈值：design 9.1 调度 P95 ≤ 300ms → **PASS（余量 >100×）**。
- 说明：该数字是**调度判定**耗时，不含子任务实际执行；design 9.9 的“并发”是确定性上限断言，不是吞吐基准。

**熔断**
- 口径：同类（同 role / 同 output paths / 同主验收项）4 个样本、3 个失败；
  `CircuitMinSamples=4`、`CircuitFailureRatio=0.5`，第 4 个样本落库后开环。
- 实测：恰好 1 条 `subtask.circuit_open`，第 5 个同类节点保持 pending，`ResetCircuit()` 后重新可调度。

## 发现的问题与边界（不得当作“全部通过”引用）

1. **租户仓储缺口**：`subtask.Repo.ListByParent` 只有 `parentTaskID/dagID`，没有
   tenant/project 条件；跨租户隔离目前依赖 scheduler/handoff 的上层校验，不依赖查询层。
2. **跨租户 handoff 无审计落库**：只有 `security.violation` 事件，`hlp_event.audit` 无写入。
3. **子任务检查点未在编排层验证**：本套件只证明子任务行可持久化重读；“各自可恢复检查点”由
   runtime/checkpoint 负责，未纳入 9.9 套件。
4. **DB 约束名偏差**：design 写 `ck_subtask_accepted`，010 DDL 的 CHECK 未命名，
   PostgreSQL 自动命名为 `subtasks_check`；行为一致，名称不可按 design 检索。
5. **汇聚完整性语义**：`failed/rejected/cancelled` 终态不会让
   `AggregationReport.Complete=false`（只有非终态、跨租户、产物校验失败、冲突会）；
   design 9.9 未要求“失败即 incomplete”，但若下游以 `Complete` 判断全绿需注意。
6. **未验证**：`SubtaskExecutor` 的真实执行链路（本套件使用调度器状态迁移，不启动 agent）。
   因此本报告证明的是**编排器契约与事件链**，不等于“多 Agent 运行时端到端已跑通”。