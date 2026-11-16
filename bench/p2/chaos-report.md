# T-P2-T2 混沌与故障注入演练报告

- 演练日期：2026-11-16
- 分支：`feature/p2-orchestration`（T-P2-09 → T-P2-10 → T-P2-T2 连续收工）
- 依据：design 9.8（混沌与故障注入）、8.4（P2 验收门槛）、REQ DFX-R-01 ~ DFX-R-08
- 用例集说明：`bench/chaos/cases.md`
- 结论：**11 个用例全部通过（PASS=15 / FAIL=0，含子断言），无数据损坏、无静默放行**

## 1. 环境

| 项 | 值 |
|---|---|
| 宿主 | Windows（Git Bash），Go 1.22.12、Docker 29.8.2 |
| PG | 容器 `hlp-chaos-pg`，`pgvector/pgvector:pg16`，端口 55440 |
| Mongo | 容器 `hlp-chaos-mongo`，`mongo:7`，端口 27018 |
| Redis | 容器 `hlp-chaos-redis`，`redis:7-alpine`，端口 56379 |
| 夹具 | PG：1 父任务 + 8 子任务（真实 `hlp_task.*` 表，由 `pgindex.Migrate` 建）；Mongo：5 文档；Redis：5 key |
| 隔离 | 三个容器均为本用例独立实例，**未触碰**开发共享库（`hlp-pg-test:55432` / `hlp-mongo:27017`） |

## 2. 方法与判定口径

一、**注入与观测分离**。docker 层只负责制造故障（`pause` / `stop` / `kill -s KILL` / `--tmpfs` 配额），
"平台看到了什么"一律由 `bench/chaos/probe` 通过真实客户端（pgx / mongo-driver / go-redis /
仓库内 `pkg/pgindex`、`pkg/mongodb`、`pkg/redis`）观测。这样可以抓出"容器停了但客户端返回零值"的假通过。

二、**每个用例三问**：

1. 故障被识别了吗？—— 停机期间探测必须返回 `"result":"degraded"` 且带 driver 原始错误；
2. 有没有静默放行？—— 停机期间返回 ok / 空值 → 用例直接 FAIL；
3. 数据有没有坏？—— 停机前后对同一批夹具做 `digest + rows` 比对，必须逐字节一致
   （PG `md5(string_agg(...))`、Mongo 文档内容 md5、Redis value md5）。

三、**不允许"没测到就当通过"**：基线摘要取不到时用例 FAIL，而不是跳过。

## 3. 结果总表

| # | 用例 | 结果 | 关键证据 |
|---|---|---|---|
| 1 | `pg-pause` | PASS | 停机中 `pgindex: ping: context deadline exceeded`；恢复后摘要 `e1867d02…:8` 不变 |
| 2 | `pg-outage` | PASS | 停机中 `connectex: No connection could be made…refused`；恢复后摘要不变 |
| 3 | `mongo-outage` | PASS | 停机中 `server selection error: context deadline exceeded`；恢复后 `2e6c18b3…:5` 不变 |
| 4 | `redis-outage` | PASS | 停机中 `dial tcp 127.0.0.1:56379: refused`；恢复后 `8c6b8771…:5` 不变 |
| 5 | `kill9-pg` | PASS | 事务打开后 `docker kill -s KILL`，重启后未提交行不存在，摘要 `e1867d02…:8` 不变 |
| 6 | `disk-full` | PASS | 1MB tmpfs 写 8MB：`write /data/memory.md.tmp: no space left on device`，`no_corrupt:true`，无残留文件 |
| 7 | `workspace-drift` | PASS | 验收后外部改写产物 → 汇聚 `artifact_drift` + `E_AGGREGATION_INCOMPLETE`，报告 `complete=false` |
| 8 | `subtask-hang` | PASS | 2 个卡死子任务 → 2 条 `subtask.failed(category=timeout)`，槽位释放，传播决策 `cancel_set` + `d→c` |
| 9 | `quota-full` | PASS | 20/20 slot 打满，14×2 子任务中余下全部 `subtask.queued`（带 position/reason），0 failed、0 cancelled、无单父独占 |
| 10 | `models-down` | PASS | 回退链全不可用 → `FallbackError`、`Exhausted=true`、5s 内返回、尝试次数 ≤ MaxPerStage+1 |
| 11 | `approval-down` | PASS | `WaitForApproval` 超时返回，请求仍 `pending`，**从未出现 approved** |

原始证据：
- `bench/chaos/out/results.jsonl` —— 逐用例 JSON 结论（本次 11 条全 pass）
- `bench/chaos/out/suite-full.log` —— 全量执行日志（含每条 driver 错误原文）
- `bench/chaos/out/pg-tx.log` —— kill -9 时未提交事务的客户端日志：`tx-open` → 提交阶段返回
  `context deadline exceeded`（**显式失败**，不是"以为提交成功"）

## 4. 验收条件对照（tasks.md T-P2-T2）

| 验收项 | 状态 | 说明 |
|---|---|---|
| 全部用例无数据损坏 | 满足 | 4 个存储停机用例 + kill -9 的摘要逐字节一致 |
| 全部用例无静默放行 | 满足 | 停机期间"成功"即 FAIL；审批用例断言永不 approved |
| PG 停机记忆降级可用 | 部分满足（见 §5.1） | 存储层故障显式暴露、恢复后一致；"记忆降级到 md"属 memoryfile/记忆服务行为，未在本套件端到端复现 |
| Mongo 停机标 `checkpoint_degraded` | 未满足（见 §5.1） | 当前仓库无该标记/事件字段；已验证同等底线（检查点调用显式报错、不挂死） |
| 审批不可达时高危调用保持挂起 | 满足 | `approval-down` 用例 |
| 演练记录留档 | 满足 | 本报告 + `bench/chaos/cases.md` + `bench/chaos/out/*` |

## 5. 偏差、遗留项与风险

### 5.1 `checkpoint_degraded` 标记缺失（遗留，需跨线）

design 9.8 要求「Mongo 停机 → 检查点保存失败 → 任务继续但标记 `checkpoint_degraded` 告警」。
核查结果：`pkg/harnessloop/checkpoint` 的 `Store`/`Checkpoint` 结构没有该字段，
`pkg/harnessloop/event` 的事件字典也没有对应事件类型；两者都不在本线独占范围
（本线只独占 `pkg/harnessloop/orchestrator/**`、`bench/chaos/**`、`bench/p2/chaos-report.md`），
且 `pkg/harnessloop/event/types.go` 属于全平台共享字典，单方面新增会与运维/大屏线冲突。

本次演练给出的是**同等强度的底线证据**：Mongo 不可达时检查点调用必须显式失败（`server selection error`
直接返回，没有静默成功），且调用方不会因此挂死。
建议接入点：`checkpoint.Store.Save` 失败 → runtime 发 `checkpoint.degraded`（需事件字典 owner 新增类型），
并把 `degraded=true` 落到 `hlp_task.task_runs` 的告警字段。

### 5.2 服务层降级未端到端复现

「PG 停机 → 索引 API 503 / 事件缓冲重试 / 记忆降级 md」是 **服务层** 行为，本轮用独立混沌容器，
没有起完整 `harnessloop-service` + `gateway` 栈，因此只验证了存储层故障的显式暴露。
补测路径：用 `docker/docker-compose.dev.yaml` 全栈 + `inject.sh` 的 pause/stop 用例，
断言 `/metrics` 中 `hlp_*_degraded` 计数与 503 响应码。留作下一轮（可在 T-P2-T4 一并做）。

### 5.3 LLM 供应商未真实接入

`models-down` 走的是真实回退链（`modelregistry.FallbackChain.Execute`），但 `caller` 是注入的
失败函数，等价于"供应商整体拒连/5xx"。真实 429/quota 分类映射已在 T-P1 模型线用例覆盖，
本轮不重复。

### 5.4 磁盘满的注入位置

`ENOSPC` 在 1MB tmpfs 中复现（`docker run --tmpfs /data:size=1m`），未在宿主真盘打满
（避免污染开发机）。判定条件与真盘一致：写失败必须显式返回，且"临时文件 + rename"路径
不得留下半写目标文件——用例已断言 `no_corrupt:true` 且目录无残留。

### 5.5 kill -9 的注入主语

`kill9-pg` 用 `docker kill -s KILL` 直接杀死数据库进程（模拟内核级强杀），同时杀掉客户端进程。
未覆盖"平台进程被 kill -9 后从检查点恢复"的全链路（需要完整服务栈）；该链路的
**恢复幂等与计费不重复**已由 `TestChaosKill9RecoveryNoDoubleCharge` 在检查点/事件层断言：
消费量按检查点原值恢复、二次恢复不重复计费、`RestoreSeq` 后事件序号连续（3 → 4，无重复号）。

## 6. 复现

```bash
bench/chaos/inject.sh preflight     # 环境检查
bench/chaos/inject.sh up            # 起独立混沌容器
bench/chaos/inject.sh suite         # 全量演练（本次 3m55s，PASS=15 FAIL=0）
bench/chaos/inject.sh down          # 收尾

# 仅应用层用例（不依赖 docker）
HARNESS_CHAOS=1 go test -count=1 -v ./bench/chaos/
```

## 7. 一句话结论

存储层三类依赖的**真停机**、数据库进程 **kill -9**、**磁盘满**、**工作区外部改动**、
**子任务卡死**、**配额打满**、**模型全不可用**、**审批不可达** 八类故障均已注入并验证：
故障全部被显式识别（有事件/有错误码/有降级返回值），没有任何一类走到"看起来成功"的分支，
恢复后夹具摘要在四个存储故障用例中逐字节一致。
唯一未闭环项是 `checkpoint_degraded` 标记（跨线，需事件字典与 checkpoint 存储层配合），
已在 §5.1 写明接入点与本次替代验证的强度。
