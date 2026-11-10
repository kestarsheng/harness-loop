# P0 FSM 转移矩阵测试报告（T-P0-T1）

- 生成时间：2026-11-11
- 被测包：`pkg/harnessloop/fsm`、`services/harnessloop-service/internal/service`
- 执行命令：`go test -race ./pkg/harnessloop/fsm/... ./services/harnessloop-service/internal/service/...`
- 结果：**PASS**（含 `-race`，Docker `golang:1.22` 容器内执行）

## 1. 转移矩阵（8 条合法边）

| ID | From | To | Trigger | 用例 |
|---|---|---|---|---|
| T1 | a 需求理解 | b 计划 | acceptance_pass | `TestExhaustiveLegalEdges/a_to_b` |
| T2 | b 计划 | a 需求理解 | acceptance_fail | `TestExhaustiveLegalEdges/b_to_a` |
| T3 | b 计划 | c 执行编码 | acceptance_pass | `TestExhaustiveLegalEdges/b_to_c` |
| T4 | c 执行编码 | b 计划 | acceptance_fail | `TestExhaustiveLegalEdges/c_to_b` |
| T5 | c 执行编码 | d 验收测试 | acceptance_pass | `TestExhaustiveLegalEdges/c_to_d` |
| T6 | d 验收测试 | c 执行编码 | acceptance_fail (impl_gap/test_gap) | `TestExhaustiveLegalEdges/d_to_c` |
| T7 | d 验收测试 | b 计划 | acceptance_fail (plan_gap/requirement_gap) | `TestExhaustiveLegalEdges/d_to_b` |
| T8 | d 验收测试 | e 结束 | acceptance_pass | `TestExhaustiveLegalEdges/d_to_e` |

## 2. 拒绝矩阵（17 条非法边，100% 拒绝且状态不变）

5×5 = 25 种组合，扣除 8 条合法边，其余 **17 条**全部返回 `E_ILLEGAL_TRANSITION`，且 `m.Stage()` 保持原值。
用例：`TestExhaustiveIllegalEdges`。

## 3. 跳过阶段（6 条，全部拒绝）

a→c、a→d、a→e、b→d、b→e、c→e。用例：`TestExhaustiveSkipStages`。

## 4. 终态出边（5 条，全部拒绝）

e→a/b/c/d/e 全部拒绝，`LegalTargets(e)` 为空。用例：`TestExhaustiveTerminalHasNoOutbound`。

## 5. 并发竞争

10 个 goroutine 同时对 a→b 发起转移：**1 成功 / 9 冲突**，最终 stage=b。用例：`TestExhaustiveTenGoroutinesSingleWinner`。

## 6. 迁移不变量（崩溃点等价验证）

对每个源阶段枚举全部目标阶段，仅在合法时推进；断言推进后 `Stage` 仍是合法枚举值且等于目标。
等价于“转移前后任一时刻进程崩溃后重启，读到的阶段值必然属于 {a,b,c,d,e}”。用例：`TestExhaustiveStageInvariants`。

## 7. 终态唯一出口

- 只有 `d→e`（T8）能到达终态；a/b/c 直达 e 全部拒绝（`TestTerminalOnlyReachableFromVerify`）
- 终态原因恰好三种：`completed` / `aborted` / `failed`（`TestTerminalReasonSet`）
- 终态无出边（`TestTerminalMatrix`）

## 8. 中止幂等

| 场景 | 结果 |
|---|---|
| 同 epoch 连续 3 次中止 | 仅 1 条 `task.aborted`，状态只写 1 次，时间戳三次一致 |
| 不同 epoch 连续中止 | 仍仅 1 条终态事件 |

用例：`TestAbortIdempotentTriple`、`TestAbortDifferentEpochsStillSingleTerminalEvent`。

## 9. 结论

- 8 条合法边全部可走通并产生事件
- 17 条非法边 + 6 条跳过 + 5 条终态出边 **零放行**
- 并发下同一转移仅一次成功
- 终态只经 T8 到达，原因集合封闭
- 中止满足 `task_id + abort_epoch` 幂等

P0 FSM 门槛：**通过**。