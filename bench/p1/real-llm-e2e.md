# P1 真实 LLM 五阶段 E2E 报告（最后一公里）

- 日期：2026-11-11
- 链路：CLI → `harnessloop-service`(:50013) → `agent-service`(:50006) → LLM 网关 → MongoDB / PostgreSQL(:55432)
- 模型：`qwen-plus`（网关共 9 个可用模型）
- 结果：**completed**，exit code 0

## 一、实测结论

| 项 | 结果 |
|---|---|
| 终态 | `completed`，`current_stage=e` |
| 阶段尝试 | a/b/c/d 各 **1 次**，全部 `accepted=true`、`finish_received=true`、`entry_reason=initial` |
| 事件 | 4 条 `stage.transition`（a→b→c→d→e） |
| 检查点 | 4 条 `stage_entry`（a/b/c/d） |
| 耗时 | 约 1 分钟内（含 4 次真实模型调用） |
| 密钥 | 仅环境变量/本地 gitignored 配置，测试后已重置占位符；提交 diff 扫描 0 命中 |

## 二、打通前的三个真实阻塞（均已修复）

### 1. 网关不支持 function calling
`tools` / `functions` 参数一律 400「Required body invalid」（qwen3.7-plus、qwen-plus、deepseek-v4.1-flash 均如此）。

**解法**：实现严格文本协议兜底（不使用工具调用能力）：

```
[stage_finish] {"attempt_id":"...","stage":"a","summary":"..."}
```

- 解析要求：必须出现 `[stage_finish]` 标记 + 合法 JSON + `attempt_id`/`stage`/`summary` 非空 + stage ∈ a~e
- 解析成功 → 合成为 `stage_finish` 工具事件，走与结构化工具一致的路径
- 普通文本「我完成了」**仍然不结束阶段**（no_finish 重试），fail-closed 语义不变
- 协议要求同时写入 system prompt 与 user 消息（后者对模型更有效）

### 2. 推理型模型输出不稳定
`qwen3.7-plus` 的 reasoning 占据大量 token，content 为空或不含协议行；切换到 `qwen-plus` 后稳定输出协议。

### 3. 工具结果事件被误映射为 tool_call
agent-service 把 `EventToolResult` 也映射成 `chunk.ToolCall`（`Arguments` 为空），覆盖了前一条带参数的 `tool_start`，导致 runtime 报 `invalid stage_finish payload`。

**修复**：仅当 `event.Type == EventToolStart` 且 `len(ToolArgs) > 0` 时才映射 `ToolCall`。

## 三、边界说明

- 文本协议是**兜底通道**，不是 function calling；网关具备工具能力后可直接切回结构化调用，协议解析可保留为降级路径。
- 模型填写的 `attempt_id` 当前不在 runtime 侧做一致性校验（`stage_runner` 只校验非空），后续可接入 `FinishGuard` 严格比对。
- 本轮消息很短，未触发上下文压缩；压缩链路的真实 LLM 验证另需构造超阈值上下文。

## 四、结论

**真实 LLM + 真实 agent-service + FSM + 验收 + 检查点 + 事件 + CLI 已端到端跑通**，不再是 agent 桩验证。