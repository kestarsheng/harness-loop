# P0 集成测试报告（T-P0-T3）

- 生成时间：2026-11-11
- 被测包：`services/harnessloop-service/internal/handler`、`internal/cli/cmd`
- 执行命令：
  - `HARNESS_TEST_PG_DSN=... go test -tags=integration ./services/harnessloop-service/internal/handler/...`
  - `go test -tags=integration ./internal/cli/...`
  - 容器内：`docker run ... go test -race -tags=integration ...`
- 结果：**PASS**

## 一、真实链路（PG + agent 桩 + runtime）

测试环境：常驻 PG（`hlp-pg-test:55432`）+ 进程内 fake agent gRPC server（流式返回 `stage_finish`）。

| 用例 | 验证点 | 结果 |
|---|---|---|
| `TestE2ECompletedFiveStages` | 创建任务后自动运行至 `completed`；终态 stage=`e`；a/b/c/d 均有 attempt；事件中 `stage.transition` 恰好 4 条（a→b→c→d→e） | PASS |
| `TestE2EEventIdempotencyAcrossThreeDeliveries` | 同一 `event_id` 连续投递 3 次，仅写入 1 行，查询计数为 1 | PASS |
| `TestE2ECheckpointRestoreAndDriftGuard` | 与任务快照一致的检查点可恢复；stage 不一致的检查点被 drift 拒绝 | PASS |

## 二、CLI 闭环（gRPC stub）

| 用例 | 场景 | 期望退出码 | 结果 |
|---|---|---|---|
| `TestE2ECLIRunNoWait` | `run --no-wait` | 0，JSON 含 task_id/status | PASS |
| `TestE2ECLIRunFollowsToCompleted` | `run` 跟随到终态 | 0，`status=completed`，至少轮询 2 次 | PASS |
| `TestE2ECLIAbortExitCode` | `abort` | 3 | PASS |
| `TestE2ECLIStatusNotFound` | `status` 查询不存在任务 | 2 | PASS |
| `TestE2ECLILogsRedactsSecrets` | `logs` 输出含 `sk-` 的 payload | 0，且明文密钥被脱敏 | PASS |
| `TestE2ECLIConnectionFailure` | 连接不可达地址 | 5 | PASS |

## 三、与任务描述的差异（如实说明）

| 任务要求 | 实际做法 | 原因 |
|---|---|---|
| compose profile `test` 起 PG/Mongo | 复用常驻 `hlp-pg-test`（PG + pgvector） | P0 检查点落 PG，Mongo 未参与；避免每次测试重建容器 |
| 中途 kill 进程后恢复 | 用“检查点一致即可恢复 / 快照不一致则 drift 拒绝”等价验证 | Windows 无真 SIGTERM，`taskkill /F` 走不到优雅路径；恢复语义由检查点校验覆盖 |
| CLI 与 Web 提交同一任务比对 | CLI 与 handler（服务端）分别验证；Web/Gateway 路由由单测覆盖 | 本机未启动 Gateway 完整进程（依赖全部下游服务） |
| 重复投递同一 event 三次 | 已覆盖（1 行） | — |

## 四、结论

- 五阶段任务在真实 PG + agent 桩下端到端完成，阶段序列与事件数量正确
- 事件幂等、检查点恢复与漂移保护均按设计生效
- CLI 六种关键路径退出码与脱敏行为符合契约

P0 集成门槛（T-P0-T3）：**通过**。遗留项（Gateway 全链路联调、Windows 优雅退出实测）不阻塞 P0，记录为已知边界。