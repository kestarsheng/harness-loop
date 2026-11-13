# T-P1-T5 · 权限决策与审计报告（design 9.11 / 8.4 P1）

- 套件：`pkg/harnessloop/policy/policy_suite_test.go`
- 用例：**20 个 `TestPolicySuite*`**（9.11 主表 33 行，按用例组聚合；含决策/审批性能 2 个）
- 真实依赖：PostgreSQL 55432（`hlp_event.audit`，含 011 `ck_audit_tool_decision`）；
  事件经 `orchestrator.PublisherEmitter` → 真实 `event.Publisher`，按 `task_id` 断言 `seq=1..N`。
- 审计隔离：所有写入使用独立 tenant 前缀 **`ps-suite-`**，每个用例收尾
  `DELETE FROM hlp_event.audit WHERE tenant_id LIKE 'ps-suite-%'` 并复查残留为 0。
- 执行命令：

```bash
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
HARNESS_REQUIRE_PG=1 \
go test ./pkg/harnessloop/policy/ -run 'TestPolicySuite' -count=1 -v
```

- 结论：**33 行中 26 PASS / 6 PARTIAL / 1 FAIL**。

## 逐条对照（编号取自 design 9.11 原表的「用例组 / 用例」原文）

| 编号（9.11 用例组 / 用例） | 断言 | 结论 | 备注 |
|---|---|---|---|
| 标签 / 登记 MCP 工具不给标签 | `E_TOOL_TAG_MISSING` | PARTIAL | `subtask.Registry.ValidateToolTags` 拒绝并返回该码；**`policy.Engine` 不调用标签校验**（`SystemDecide` 对空 tags 的循环直接跳过），Engine 层是缺口 |
| 矩阵 / a 阶段 `write_file` | `E_STAGE_TOOL_FORBIDDEN` | PASS | 实际码 `E_TOOL_DENIED`（rule_id `stage_matrix.a.write`） |
| 矩阵 / c 阶段 `write_file`（白名单路径内） | 允许 + `tool.allowed` 审计 | PARTIAL | 允许 + 审计 `decision=allow` 已实测；**实现无 `tool.allowed` 事件**，允许路径的事件是 `tool.started`/`tool.result` |
| 矩阵 / d 阶段 `write_file` | 拒绝 | PASS | |
| 矩阵 / d 阶段 `go test ./...` | 允许 | PASS | `run_command {"go","test","./..."}` 通过 exec 白名单 |
| 矩阵 / e 阶段任意工具 | 拒绝 | PASS | 读与 exec 均拒绝（`DenyAll`） |
| 双层名单 / 任务白名单内但阶段黑名单命中 | 拒绝（黑名单优先） | PASS | 四层 Allow 仍被 stage matrix 拒绝，`layer=system` |
| 双层名单 / 不在任何白名单 | `E_TOOL_NOT_ALLOWED` | PASS | 实际码 `E_TOOL_DENIED`（`policy.role.no_match`） |
| 角色 / coder 调用部署工具 | `E_TOOL_NOT_PERMITTED` + 审计 | PARTIAL | `subtask.Registry.CheckTool` 返回 `E_TOOL_NOT_PERMITTED`；**无角色审计写入** |
| 路径 `..` / `src/../../etc/passwd` 及变体 | `E_PATH_OUT_OF_SANDBOX`，**执行前**拒绝（无副作用） | PASS | `..\`、`%2e%2e/`、`%2E%2E%2f` 均拒绝；Guard 回调未执行；实际码 `E_PATH_ESCAPE`。`.../` **不是逃逸**（普通目录名，放行符合语义） |
| 路径 - 绝对 / `/etc/passwd`、`C:\Windows\win.ini` | 拒绝 | PASS | `C:\` 与大小写变体仅在 `GOOS=windows` 断言；Linux 容器用 `/etc/passwd` 与 URL 编码变体覆盖 |
| 路径 - 符号链接 / 沙箱内 `link -> /tmp/out` | 拒绝 + 审计 | PASS | Windows 用 junction、Linux 用 symlink；审计 deny 行落库 1 条 |
| 路径 - ToCToU / 校验后执行前替换为链接 | 写类工具重算 realpath，拒绝 | PASS | 首次允许；`artifacts` 目录替换为外部链接后 Guard 拒绝且未执行 |
| 路径 - UNC/前缀 / `\?\C:\...`、`file://` | 拒绝 | PARTIAL | `\\?\C:\...`（双反斜杠）与 `file://` 拒绝；**design 字面的单反斜杠 `\?\C:\...` 在 Windows 下不被识别为 UNC，未拒绝** |
| 写入白名单 / 写 `.harness/policy/x.yaml` | `E_WRITE_PATH_DENIED` + `E_POLICY_SELF_MODIFY` | PASS | 实际码 `E_SELF_ELEVATION`；拒绝发生在路径落盘前 |
| 写入白名单 / 写 `artifacts/report.json` | 允许 | PASS | |
| exec / `go test ./...` | 允许 | PASS | |
| exec / `go test ./... && curl http://evil` | `E_EXEC_NOT_ALLOWED`（复合命令） | PASS | 实际码 `E_EXEC_DENIED` |
| exec / `sh -c "go test"` | 拒绝（非白名单可执行 + 元字符） | PASS | `sh` 不在 allowlist |
| exec / `go test; rm -rf /` | 拒绝 | PASS | `;` 元字符 |
| exec / `go build -o /tmp/x` | 拒绝（绝对路径参数） | PASS | |
| exec / `npm install evil-pkg` | 拒绝（安装动作） | PASS | `npm` 不在 allowlist |
| 自我提权 / Agent 写 `config/harness_loop/stage_allow.yaml` | 拒绝 + `security.violation`；任务结束 `policy_hash` 不变 | PARTIAL | 拒绝（`E_SELF_ELEVATION`）+ 无文件副作用已实测；**`security.violation` 由 `PolicyBridge` 发出，policy 包内无事件端口；`policy_hash` 无对应实现/断言** |
| 自我提权 / 提示注入要求“忽略权限” | 权限不受影响（`Decide()` 输入不含上下文文本） | PASS | 把注入文本写入 `RoleName`/`AgentID`，决策码与 rule_id 完全不变 |
| 优先级 / 任务层 allow 但租户层 deny | 拒绝，`layer=tenant`，记录命中高优先规则 | PASS | `rule_id=tenant.deny` |
| 优先级 / 系统硬限制（密钥文件）在四层全 allow 时 | 仍拒绝 | **FAIL** | **未实现**：`isSelfElevationPath` 只匹配 `config/harness_loop/`、`.harness/policy/`、`.harness/config`，密钥文件路径不在其中；套件没有对应通过项（若补测会得到放行，代码核查结论） |
| 审计 / 任一拒绝场景 | 响应含 `error_code/rule_id/layer/tool/stage/requested_path`；审计有 deny 记录 | PASS | `Decision` 字段齐全；真实 PG 有 deny 行；矩阵类拒绝 `requested_path` 为空（尚未进入路径检查），路径类拒绝有值 |
| 审计 / 同一调用两次（配置未变） | 均拒绝且 `rule_id` 一致 | PASS | 两次 `E_TOOL_DENIED`，rule_id 相同 |
| 事件一致 / deny 调用 | 只有 `tool.denied`，无 `tool.result`；事件与审计 `rule_id` 相同 | PASS | 真实 Publisher：1 条 `tool.denied`、0 条 `tool.result`，payload `rule_id` == 审计 `rule_id`，seq 连续 |
| 审批 / 部署类工具触发 | 挂起 + 审批请求；通过则继续，拒绝则 `E_APPROVAL_REJECTED` 不重试 | PASS | `EffectRequireApproval` + pending ticket；approve 放行；reject 后再次 Begin 被拒且 `RequestCount` 不增加 |
| 审批 / 审批超时 | `E_APPROVAL_TIMEOUT`，**不放行** | PASS | 假时钟推进，`Allowed=false` |
| 审批 / 配置尝试设“超时自动通过” | 配置校验拒绝该取值 | PASS | `timeout_auto_approve=true` 加载失败 |
| 性能 / 1000 次调用（70% 缓存命中） | 命中 P95 ≤ 5ms、未命中 P95 ≤ 20ms；整体时延增幅 ≤ 10% | PARTIAL | P95 两项达标（见下）；**“整体增幅 ≤10%”在容器 4 次运行中 2 次超阈值**，区间 −5.9%～+132.3% |

## 8.4 P1 门槛对照

| 8.4 条目 | 断言 | 结论 | 备注 |
|---|---|---|---|
| 权限审计一致性 | deny 事件与审计 `rule_id` 一致、真实 PG 落库、011 CHECK 生效 | PASS | 见 9.11「审计 / 事件一致」行 |
| 必须留档产物：权限决策聚合报告 | 本文件 | PASS | 决策 P95、审批时延、规则聚合见下 |

## 性能口径与实测区间

**权限决策（1000 次，700 命中 / 300 未命中）**
- 口径：`Resolver.Decide` 单次耗时（`Decision.DurationUS`）；P95 用 nearest-rank 四舍五入；
  “整体增幅”= 启用缓存总耗时相对关闭缓存（`SetCache(nil)`）总耗时的百分比。
- 宿主（Windows，4 次，每轮 1000 次）：
  - 命中 P95 **0s（<1µs，`DurationUS` 微秒精度取整为 0）**；
  - 未命中 P95 **1.59–1.88ms**；
  - 增幅 **−68.6%～−77.7%**（缓存显著更快）。
- 容器（`golang:1.22-bookworm`，4 次）：
  - 命中 P95 **7–10µs**；未命中 P95 **25–51µs**；
  - 增幅 **−5.9% / +54.6% / +118.2% / +132.3%**（4 次中 2 次 >10%）。
- 阈值：命中 ≤5ms、未命中 ≤20ms → **PASS**；“整体增幅 ≤10%” → **不达标（容器波动，2/4 超阈值）**。
- 原因：决策本身极轻（数十微秒），缓存 key 需两次 sha256 + policy 指纹 JSON 序列化，
  命中路径的 key 计算成本可能高于直接决策；这是实现特征，不是测量噪声可完全解释。
  报告如实记为未达标，未通过放宽断言“凑绿”。

**审批**
- 口径：`SubmitDecision(approved)` → `Await` 返回放行的耗时（50 次采样）。
  由于 `pkg/agent/approval` 存在既有无锁读写竞态（见边界 10），套件改为**顺序驱动**：
  先提交决策再调用 `Await`，测量的是“决策已落库后的处理延迟”，**不含人工审批员等待时间**。
- 宿主（4 次）：P95 **0s（<1µs）**，max 0–1.02ms。
- 容器（4 次）：P95 **5.4–113.7µs**，min 1.0–2.5µs，max 64–160µs。
- 挂起语义（Begin 后 Status=pending、approve 放行、reject 不重试）由同一组用例断言。

**审计落库**
- 每次拒绝 1 条 `hlp_event.audit(kind=tool_decision, decision=deny, rule_id≠null, payload.layer)`；
  011 CHECK 实测：`rule_id=NULL` → SQLSTATE 23514。
- 独立 tenant `ps-suite-*` 收尾清理并复查 0 残留。

## 发现的问题与边界（不得引用为“权限全量达标”）

1. **系统密钥文件硬限制缺失（FAIL）**：四层全 allow 时没有额外的密钥文件拒绝规则。
2. **Engine 不校验工具标签（PARTIAL）**：空标签只在 `subtask.Registry` 层被拒，
   直接调用 `policy.Engine.Decide` 不会触发 `E_TOOL_TAG_MISSING`。
3. **错误码与 design 不一致**：`E_STAGE_TOOL_FORBIDDEN`→`E_TOOL_DENIED`、
   `E_TOOL_NOT_ALLOWED`→`E_TOOL_DENIED`、`E_PATH_OUT_OF_SANDBOX`→`E_PATH_ESCAPE`、
   `E_WRITE_PATH_DENIED`/`E_POLICY_SELF_MODIFY`→`E_SELF_ELEVATION`、
   `E_EXEC_NOT_ALLOWED`→`E_EXEC_DENIED`。行为（拒绝）一致，码不可按 design 检索。
4. **`tool.allowed` 事件不存在**：允许路径产生 `tool.started`/`tool.result` + 审计 allow 行。
5. **单反斜杠 `\?\C:\...` 未拒绝**：双反斜杠 UNC 前缀会拒绝；design 字面变体存在绕过面。
6. **`policy_hash` 未实现**：只有 resolver 内部的租户策略 loosening hash，未导出、
   也未与任务创建快照比较。
7. **角色/自我提权无审计落库**：这两类拒绝在 policy 包内没有 audit sink；
   生产 `PolicyBridge` 会写，但本套件覆盖的是库层。
8. **`.../` 不是逃逸**：design 把它列为 `..` 变体，但操作系统语义上它是普通目录名，
   实现放行是合理的；不应当作“应拒未拒”。
9. 本报告证明的是**权限库与审计 SQL 契约**；生产 `MCP_TOOL_POLICY_ENFORCED`
   默认关闭，未验证开启后的 MCP 全链路。10. **`pkg/agent/approval` 存在真实数据竞争（容器 `-race` 已复现）**：
    `flow.go:293 applyDecision` 写 `ApprovalRequest.Status`，而 `policy.ApprovalGate.status`
    （`approval.go:384`）经 `GetRequest` 返回的同一指针无锁读取该字段。
    改前的自定义并发审批用例已捕获完整堆栈；T5 边界不允许改实现，因此新增审批用例改为
    顺序驱动。改后验收：三包 `-race` **连续 10 次通过**、policy 包单独 **11 次通过**；
    期间有 1 次三包连跑在 policy 包报 `DATA RACE`（未捕获堆栈，疑为既有
    `TestApprovalDeployPendingAndStatus` 的并发 `Await`+`SubmitDecision`）。
    **真实运行时“Await 挂起 + 外部提交决策”仍会触发该竞态，需单独立项修复；本报告不宣称
    审批并发路径已无竞态。**