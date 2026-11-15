# B-5 可行性评估：压缩范围扩到 Agent 对话历史

- 评估日期：2026-11-15
- 评估方式：只读代码核查，未修改任何实现文件
- 基线：`develop = e4bae23`（B-4 独立验收通过后）
- 关联：发布版 8 条已知差距第 5 条「压缩仅作用于阶段输入」；CHANGELOG.md:45 的自述

## 1. 结论先讲

1. **可行，但主战场不在 `services/harnessloop-service/internal/service/runtime.go`**，而在 `pkg/agent`（对话历史真正产生的地方）。与 B-4 改动面基本不重叠，冲突风险低。
2. **不需要改 proto**。现有 `ExecuteStreamChunk.Type` 是字符串，可新增事件类型透传。
3. **有一个硬阻塞**：`agent.Message` 不携带 tool_calls / tool_call_id，现有 `compaction.ValidatePairing` 无法直接使用，必须先补消息结构。这是 B5-2 的前置改造。

## 2. 现状链路（实测证据）

| 环节 | 代码位置 | 事实 |
|---|---|---|
| 阶段入口压缩 | `runtime.go:279` | `stageInput.Message = r.maybeCompact(...)`，唯一触发点 |
| 被压缩的输入 | `runtime.go:243` | 默认消息只有 `"stage=<stage> attempt=<n>"`，仅在记忆注入后才变长 |
| 「历史」组成 | `runtime.go:780-785` | `history.Messages` 只塞入 **1 条** user 消息，没有真正的历史 |
| 阶段到 Agent | `stage_runner.go:181-190` | 一次性 gRPC `ExecuteStreamRequest`，无 ContextID、无历史字段 |
| Agent 执行上下文 | `engine_stream.go:85,105` | 每次 stage 都 `NewExecutionContext`，仅追加一条 user 消息；跨阶段不续历史 |
| Agent 内部历史 | `context.go:72` | 真正的对话历史在 `execCtx.Messages`，循环内累积 |
| 现有等效手段 | `engine.go:1004-1011` | `MaxHistoryLength=50` **硬截断**（砍头），不是压缩 |

即：REQ-038「下一次模型调用前触发压缩」目前只覆盖阶段入口，压缩对象是提示片段而非对话历史；Agent 内部超长历史靠滑窗丢弃。

## 3. 触发点评估

**正确落点**：`executeLoopStream` 每步 `llmReq` 构建前（`engine_stream.go:155-174`），对 `execCtx.Messages` 触发压缩。符合「下一次模型调用前」语义，且能覆盖 tool_call/tool_result 堆积。

**不成立的落点**：
- harnessloop 侧：gRPC 流只回 `token`/`tool_call`/`tool_result`，拿不到完整消息序列；runtime 无法重建历史。
- `buildAgentMessages` 内部替换截断：会同时影响非流式 `executeLoop`（`engine.go:649` 与流式共用该函数），行为面扩大，非必要不选。

## 4. proto 结论：零变更

- `ExecuteStreamChunk.Type` 为 `string`（`agent.pb.go:2032`），无枚举约束。
- `agent_service.go:398` 对 engine 事件**全量转发**，无 type 白名单。
- `stage_runner.go:201-209` 的 switch 只处理 `response`/`token`/`error`，未知 type 自然忽略。

因此新增 `compaction` 类型 chunk 可在不改 proto 的前提下透传；`harnessloop` 侧加一个 case 即可把 `compaction.triggered/completed/degraded` 落到任务事件流。

## 5. runtime.go 改动面与回滚点

- 改动面**很小**：`279` 行（B-4 未触碰）之前/之后均无 B-4 逻辑；`evaluate` 在 `329` 行，两者不重叠。
- 实际需要：给 `StageRunInput` 透传 `CompactionDir`（可选字段），在 `stage_runner.go` 增 `compaction` chunk 分支发事件。
- 回滚点：现有开关 `HARNESSLOOP_COMPACTION_ENABLED`（`main.go:194`，**非 false 即开**）；B5-2 必须**新增独立开关且默认关闭**，关闭即回到当前行为，不影响 B-4 验收通道。

## 6. 风险与前置改造

### R1（硬阻塞）消息结构缺 tool_calls / tool_call_id

- `agent.Message` 只有 `Role/Content/Name/ToolID`（`context.go:9-14`）。
- assistant 的 tool_calls 从不进入 `Messages`（`engine_stream.go:446` 只追加 tool 结果）。
- `llm.Message` 仅有 `Role/Content`（`client.go:35-38`）。
- 后果：直接对 `execCtx.Messages` 套 `compaction.ValidatePairing`（要求 `ChatMessage.ToolCalls/ToolCallID`，`pairing.go:16-21`）会把每条 tool 结果判为 `orphan_result`，压缩器输入也不合法。

**处置**：先做 B5-1——补全 `Message.ToolCalls` / `ToolCallID` 透传（含 checkpoint 序列化、`engine.go:1667/1684` 转换、回归测试），再谈压缩。

### R2 嵌套 LLM 调用

`TemplateSummarizer` 会在 stage 内再发一次模型调用（`runtime.go:788`），需独立超时与预算，否则撞 watchdog 静默阈值与 10 分钟 stage timeout（`stage_runner.go:167-171`）。

### R3 默认开关语义

`HARNESSLOOP_COMPACTION_ENABLED` 当前默认开启。新增 agent 侧压缩必须独立开关 + 默认关闭，否则默认行为改变，并与差距 6「开关默认关闭」冲突。

### R4 事件归属

`compaction.*` 事件由 harnessloop 发布（`runtime.go:833-854`）。Agent 侧触发只能经 chunk 回传，不能让 agent-service 直发任务事件（服务无事件总线）。

### R5 五要素来源（可行）

goal 取 `execCtx.Goal`；约束取 `Variables[__policy_snapshot__]`；done/todo 取 `AgentHistory` + `Plan`；背景取 `Messages` 摘要。字段可得，但需明确取值优先级并写测试。

### R6 测试面

现有 `runtime_stage_verifier_test.go` 不覆盖 agent 内部压缩；需新增 `pkg/agent` 单测（触发、降级、配对、开关关闭零行为）+ `stage_runner` 事件透传测试。

## 7. 建议拆分与验收口径

### B5-1 消息结构补全（前置，独立可验收）

- 范围：`pkg/agent/{context.go,engine.go,engine_stream.go}`、`pkg/agent/checkpoint/*`、对应测试。
- 内容：assistant 的 tool_calls 入 Messages，tool 消息带 `ToolCallID`，`llm.Message` 透传；checkpoint 往返保真。
- 验收：`go test ./pkg/agent/...` 全绿；新增「压缩后配对合法」预置用例（此时可跑通 `ValidatePairing`）；`go build ./...`；depcheck 0；容器 `-race`。
- 零行为变化要求：不改变发给 LLM 的语义（若网关不接受新字段，需要适配层剥离并写测试）。

### B5-2 Agent loop 内接入压缩（本体）

- 范围：`pkg/agent` 压缩钩子 + `services/agent-service` 装配 + `stage_runner.go` 事件分支 + `StageRunInput.CompactionDir`。
- 内容：每步 LLM 调用前 `Trigger.Evaluate`（按 `ModelWindow`）；超阈值走 `TemplateSummarizer`，失败降级 L1/L2；chunk 回传 `compaction.triggered/completed/degraded`。
- 开关：新增（建议 `AGENT_COMPACTION_ENABLED` 或 features.yaml flag），**默认关闭**。
- 验收：触发/未触发两条路径；降级路径五要素可读；配对合法；事件字段齐全；开关关闭时行为与 `e4bae23` 完全一致（对比测试）；容器 `-race`。

## 8. 已拍板（2026-11-15）

1. **同步改 `pkg/llm` 请求编码**：`Message` 补 `ToolCalls/ToolCallID` 的同时，`pkg/llm` 的请求体按要求输出 `tool_calls`（assistant）与 `tool_call_id`（tool）。需补网关兼容性测试；若网关拒绝新字段，必须在编码层做显式降级并写测试，不得静默丢弃。
2. **压缩产物沿用 `<harness_home>/tmp`**：与现有 `CompactionDir` 一致，不新开目录。
3. **B5-1 单窗**：B5-1 独立交付并验收，B5-2 随后另开。

对应执行口径：

- B5-1 文件域：`pkg/agent/{context.go,engine.go,engine_stream.go}`、`pkg/agent/checkpoint/**`、`pkg/llm/**`、`pkg/agent/*_test.go`、`pkg/llm/*_test.go`。
- 禁止改：`services/**`、`proto/**`、`frontend/**`、`pkg/harnessloop/**`。
- 零行为变化前提：B5-1 只补全消息结构，不启用压缩；关闭状态下发给网关的语义不变。
- 验收：`go test ./pkg/agent/... ./pkg/llm/...` 全绿；`go build ./...`；depcheck 0；容器 `-race`（`golang:1.22-bookworm`）；新增「配对校验可通过」用例与「网关请求体字段正确」用例。
- 收工：`git rebase develop` → `--no-ff` 合 develop → **`git push origin develop`**，回报 hash + 文件清单 + 测试输出。