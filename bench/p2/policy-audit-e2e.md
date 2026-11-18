# T-P2-25 权限审计真实验证（policy-audit-e2e）

> 任务：用真实任务触发 `read_file` / `run_command`，验证 **policy 决策 / 审批 / 审计** 三者一一对应。
> 本轮只新增验证资产，未改动业务代码、proto、frontend、scripts、internal/cli、go.mod。

## 0. 环境与基线校验

| 项 | 实测值 |
| --- | --- |
| 工作目录 | `D:/Desktop/生涯/老公毕业论文/hlp-p2-audit` |
| 分支 | `feature/p2-policy-audit` |
| HEAD | `87f25c3 chore: 合入 T-P2-27 真实流式 token 计量与溯源` |
| 工作树 | 干净（校验时无未提交改动） |
| 隔离租户 | `tenant-policy` / 项目 `p2-audit` |
| 栈 | 复用现有 50013（harnessloop-service）+ 50005（mcp-service），**未重启** |
| 栈启动时间 | 2026-11-17 22:01（进程 75192 / 63572），`policy_layers=on`、`approval gate -> http://127.0.0.1:50009` |
| PG | `hlp-pg-test` 容器，`127.0.0.1:55432/agent_platform` |
| 审计表基线 | 动手前 `hlp_event.audit` **0 行**，因此本轮所有审计行均可归因到本任务 |

## 1. 结论速览

| 验收项 | 结果 | 证据 |
| --- | --- | --- |
| 正向：允许的工具调用 → 审计条数 == 调用次数 | **通过** | 6 次调用 / 6 条 allow 审计，逐条阶段+工具+目标对齐（§3） |
| 负向：越权/高危被拒且留痕 | **通过** | 4 类拒绝各 1 条 deny 审计 + `tool.denied` 事件，rule_id 一致（§4） |
| 跨租户 | **通过** | `security.cross_tenant` 审计 + `security.violation` 事件（§4.2） |
| 审批 决策-工单-审计 | **通过（限定条件）** | require_approval → 工单 → 批准放行 / 驳回拒绝（§5） |
| 活栈真实任务触发工具调用 | **未触发** | 2 次真实任务，`audit_rows=0`，原因见 §6，**不按通过计** |
| `go test ./services/harnessloop-service/... -count=1` | **全绿** | 7 个包全 ok（含 `HARNESS_TEST_PG_DSN` 指向真 PG 的一轮） |

## 2. 验证方法：两条腿，各自能证什么

**A. 链路级 e2e（新文件 `services/harnessloop-service/internal/service/policy_e2e_test.go`）**
除 agent 流以外全是生产实现：`TaskRepo.Create`（真任务行）→ `Runtime.Run`（真阶段推进 / attempt / checkpoint）→ `StageRunner.Run`（真闸门调用点）→ `PolicyBridge` + `Resolver`（真四层解析，engine 载入仓库里真实的 `stage_matrix.yaml` / `exec_allow.yaml`）→ `AuditRepo`（真 `INSERT INTO hlp_event.audit`）→ `PublisherEmitter`（真 `tool.denied` / `approval.requested` 落 `hlp_event.events`）。
被替换的只有 agent 流（线上是 agent-service 的 LLM 流），脚本自己统计"实际发出了多少次 tool_call"，审计条数必须与之相等——这条断言让"audit=0 冒充通过"不可能发生。

**B. 活栈探针（新文件 `bench/p2/scripts/cmd/policy-e2e/main.go`）**
打真 50013 建真任务，用真 agent-service + 真 LLM 跑，回读审计。这一条用来回答"真栈上到底会不会经过工具闸门"。结论见 §6。

两条腿的分工是有意的：A 证"闸门接对了"，B 证"真栈有没有走到闸门"。

## 3. 正向：允许调用 → 审计一一对应

任务 `task-audit-pos-1794990629738356700`，终态 `completed`，脚本实发工具调用 **6** 次，审计 **6** 条，无多余行。

| # | 阶段 | 工具 | 参数 | 决策 | rule_id | 审计 id |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | a | read_file | docs/requirements.md | allow | system.allow | `audit-read_file-1794990629859231600` |
| 2 | b | read_file | docs/design.md | allow | system.allow | `audit-read_file-1794990629971998300` |
| 3 | b | list_dir | docs | allow | system.allow | `audit-list_dir-1794990630002918900` |
| 4 | c | read_file | README.md | allow | system.allow | `audit-read_file-1794990630077901600` |
| 5 | c | run_command | `go version` | allow | system.allow | `audit-run_command-1794990630086309600` |
| 6 | d | run_command | `go env GOPATH` | allow | system.allow | `audit-run_command-1794990630168272100` |

`stage_finish` 不经过闸门，因此不产生审计——这正是"一条工具调用一条审计"的必要条件（否则计数会虚高）。

## 4. 负向：越权 / 高危被拒且留痕

### 4.1 四类拒绝

每个用例一条独立任务、一次工具调用，断言：任务未 completed、审计恰好 1 条 deny、`tool.denied` 事件的 `rule_id` / `tool` 与审计一致。

| 场景 | 任务 id | 工具 | 决策 | rule_id | code | 审计 id |
| --- | --- | --- | --- | --- | --- | --- |
| 路径逃逸 | `task-audit-neg-1794990630531070900` | read_file `../../../etc/passwd` | deny | sandbox.realpath | E_PATH_ESCAPE | `audit-read_file-1794990630851504200` |
| 高危命令 | `task-audit-neg-1794990631261383700` | run_command `rm -rf /` | deny | sandbox.exec | E_EXEC_DENIED | `audit-run_command-1794990631572356500` |
| 阶段矩阵越权写 | `task-audit-neg-1794990631871906200` | write_file notes.txt（stage a） | deny | stage_matrix.a.write | E_TOOL_DENIED | `audit-write_file-1794990631986041000` |
| 未注册工具 | `task-audit-neg-1794990632294172300` | shell_exec | deny | registry.unknown | E_UNKNOWN_TOOL | `audit-shell_exec-1794990632401060500` |

### 4.2 跨租户

`tenant-intruder` 对 `tenant-policy` 的任务 `task-audit-tenant-1794990632706050000` 发起 `read_file`：

- 审计：`audit-read_file-1794990632734600300`，decision=`deny`，rule_id=`security.cross_tenant`，code=`E_CROSS_TENANT_DENIED`
- 事件：`security.violation`（severity=error），payload 带 `expected_tenant` / `tool` / `rule_id`

## 5. 审批链路：决策 → 工单 → 审计

任务 `task-audit-approval-1794990632956540000`（批准）与 `...-reject`（驳回），策略集 `task.approval.exec` 对 `run_command@stage c` 声明 `require_approval`：

| 动作 | 结果 | 审计 |
| --- | --- | --- |
| Preflight | `RequiresApproval=true, Allowed=false, Code=E_APPROVAL_REQUIRED`，返回工单 id | `audit-run_command-1794990632980657000`（require_approval） |
| AuthorizeTool（人工批准后） | 放行，无错误 | `audit-run_command-1794990633030174100`（require_approval） |
| AuthorizeTool（人工驳回） | 返回 `E_APPROVAL_REJECTED`，不放行 | `audit-run_command-1794990633111262400`（require_approval） |

`approval.requested` 事件 2 条，`rule_id` 与审计同源（`task.approval.exec`）。

**口径说明**：Preflight 与 AuthorizeTool 各自决策一次，所以同一次工具调用出现 2 条审计；"一一对应"在这里的含义是"一次决策一条审计"，不是"一次调用一条"。正向用例走的是单决策路径（stage 内只 AuthorizeTool），因此严格遵守 1:1。

## 6. 活栈真实任务：未触发工具调用（如实回报）

| 任务 id | 目标 | 终态 | 审计条数 |
| --- | --- | --- | --- |
| `task-d9c62cbd40d5c211` | 读取 README.md 并执行 go version | completed（stage e） | **0** |
| `task-7fa55108516dccad` | 强制要求先调 read_file 再 run_command | failed（stage a） | **0** |

结论：**活栈上没有发生任何 `read_file` / `run_command` 调用，审计为 0，不构成本任务的通过证据。**

判断依据（不是猜测）：

1. 两个任务的 `hlp_event.events` 里只有 `stage.transition` / `error` / `model.recovered` / `subtask.*`，**没有任何 `tool.*` 事件**；
2. 同时刻全库 100+ 条事件的事件类型直方图里同样没有 `tool.*`，说明这不是本轮偶发；
3. stage c / d 出现 `error: unclassified stage error`（category=system），但没有 `tool.denied`，即闸门从未被调用过——闸门只在 agent 真的发出 `tool_call` chunk 时才被调用（`stage_runner.go:223`）。

原因：当前 agent 侧（agent-service + qwen-plus）在 harness loop 里只回文本 + 不进工具循环，`tool_call` chunk 根本没产生。所以"policy 决策/审批/审计"这三段在活栈上没有输入，无从校验。

**这条不阻塞验收结论**：闸门接得对不对由 §3–§5 用真任务 + 真 PG 审计表证明；活栈缺的是 agent 侧的 tool_call 触发，属于 agent-service 侧的问题，不在本任务文件域内。

## 7. 实测发现的三个缺陷（未修，仅记录）

1. **tenant/role/task 策略集从未被注入运行时**：生产代码里没有任何一处给 `ToolCallInput.TenantPolicy/RolePolicy/TaskPolicy` 赋值（`grep 'TenantPolicy:\|RolePolicy:\|TaskPolicy:'` 零命中），而 `require_approval` 只可能来自这三层规则。→ 真实任务里审批分支**不可达**，四层解析实际上只跑了 system 层。
2. **高危命令与 exec 白名单互斥**：`SystemDecide` 先做 exec 白名单校验（`sandbox.exec`），高危命令（rm/kubectl 等）在到达 `ApprovalGate.Classify` 之前就被拒。要把审批分支跑通，必须显式把高危命令加入白名单——本身就是一个安全上的两难。
3. **事件 seq 双写者导致事件被静默丢弃**：`event.Publisher` 用内存 `SeqAllocator`（每任务从 1 开始），而 runtime 的 `stage.transition` / `task.failed` 走 repository 直写、seq 取 DB `max+1`。两者撞上 `(task_id, seq)` 唯一键时，后者成功、前者报 23505；`PolicyBridge.emit` 的错误被 `_ =` 吞掉，事件丢失且无告警。实测中毒症状：stage b/c 的 `tool.denied`、以及 `task.completed` 事件缺失。

处理方式：测试侧在落库前用 DB watermark 校正 seq（`policyEventStore.Append`），保证断言验的是"决策→事件内容"而不是 seq 分配器；产品代码未改。第 3 条建议单独开任务修（在任务创建/阶段推进时 `RestoreSeq` 对齐 watermark，并让 emit 失败可见）。

## 8. 复现

```bash
# A. 链路级 e2e（真任务 + 真 PG 审计表）
export HARNESS_TEST_PG_DSN="postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable"
go test ./services/harnessloop-service/internal/service/ -run TestPolicyAudit -count=1 -v

# 验收口径
go test ./services/harnessloop-service/... -count=1

# B. 活栈探针（不重启栈；会打印 must_trigger 字段）
go run ./bench/p2/scripts/cmd/policy-e2e \
  -workspace "$PWD/bench/p2/policy-audit-workspace" \
  -dsn "$HARNESS_TEST_PG_DSN" -timeout 4m

# 审计回读
docker exec hlp-pg-test psql -U agent -d agent_platform -c \
  "select audit_id, action, decision, rule_id, payload->>'stage' from hlp_event.audit where tenant_id='tenant-policy' order by created_at;"
```

## 9. 本轮改动清单

| 文件 | 类型 | 说明 |
| --- | --- | --- |
| `services/harnessloop-service/internal/service/policy_e2e_test.go` | 新增 | 4 个用例：正向 1:1、负向 4 类、跨租户、审批链路 |
| `bench/p2/scripts/cmd/policy-e2e/main.go` | 新增 | 活栈探针（建真任务、回读审计、必须打印 must_trigger） |
| `bench/p2/policy-audit-workspace/` | 新增 | 只读工作区夹具（README.md、docs/audit-notes.md） |
| `bench/p2/policy-audit-e2e.md` | 新增 | 本报告 |

业务代码、proto、frontend、scripts、internal/cli、go.mod 均未改动。
