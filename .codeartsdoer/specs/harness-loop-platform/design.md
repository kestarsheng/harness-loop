# Harness Loop 任务循环平台 — 技术设计说明书

| 项 | 内容 |
|---|---|
| 文档编号 | DES-HLP-001 |
| 版本 | **v1.3**（2026-11-11 增量更正：长期记忆不参与 LRU） |
| 状态 | 设计已定稿（U1/U3/U13/U17/U18/U19 已由用户确认） |
| 输入需求 | `.codeartsdoer/specs/harness-loop-platform/spec.md`（**SPEC-HLP-001 v1.2，1433 行，170 条 REQ + 38 条 DFX**） |
| 代码基线 | `agent-platform` @ `4a05ccb`（2026-08-03），Go 1.22 + gRPC + React 19 |
| 本文档范围 | 技术设计与代码规划（HOW），不含实现代码；DDL / proto / JSON Schema 草案除外 |
| 交付物位置 | `D:\Desktop\生涯\老公毕业论文\design.md`（仓库父目录，不在 `agent-platform` 内） |
| 修订记录 | **v1.3（2026-11-11 修订）**：依据 spec v1.2 与 2026-11-11 用户更正，纠正"长期记忆参与 30 主题 LRU"的表述——**仅项目级记忆 `project.md` 执行 30 主题上限、LRU 淘汰、读取/更新提升到队首、pinned 不淘汰；长期记忆 `global.md` 不参与 LRU、不重排、不淘汰，仅软上限 50 + `memory.capacity_warning` 告警，主题顺序为稳定写入顺序**。同步修订文档头、1.x、2.1（新增 D17）、2.3 Q10、3.1、4.6.1~4.6.8、4.7.1/4.7.3/4.7.4、4.12.2、5.6、6.2、9.6、11.1（R17/R25）、11.2（U3 关闭）、12.1/12.2/12.3；未改动 A 方案、PG 索引、压缩 C 方案、RAG 用 Go 等已确认决策。<br>**v1.2（2026-11-10）**：用户确认 5 项 P1 决策——U1-A（遗忘只降权不删 md）、U13-A（8/20/20/2 + 并行集均分预算）、U17-A（审批留 agent-service + 薄代理，二期转共享存储）、U18-A（阶段模型默认继承任务级）、U19-A（子任务复用父验收器 + 确定性优先分流）；11.2 表相应标为已确认，P1 范围锁定。<br>**v1.1（2026-11-10）**：① 4.5 上下文压缩改为 **C 方案**（自研模板摘要为唯一通道，引擎仅预留），改写 4.5.1~4.5.8 并新增 4.5.9 答辩口径；② 新增 **4.13 多 Agent 协作层**（REQ-123~141）；③ 新增 **4.14 模型选择**（REQ-142~155）；④ 新增 **4.15 工具权限与工作目录沙箱**（REQ-156~170）；⑤ 联动修订 2.1（新增 ADR **D13~D16**）、三章（组件图/清单/端口）、4.1.5、五章（5.1.2~5.1.5 新表）、六章（proto/REST/错误码）、七章（组件与图表）、八章（目录/P0-P2 切片/门槛）、九章（9.9~9.11 新测试集）、十章（指标/告警/Trace）、十一章（R19~R24、U13~U20）、十二章（术语/映射/自检）。 |
| 修订纪律 | 既有已确认决策**未改动**：A 方案 md 事实源（D1）、memory-service 内部实现不改（D1-C）、PG 仅索引层（D2）、RAG 用 Go knowledge-service（D3）、CLI 用 cobra+Bubble Tea（D4）、harnessloop-service :50008（D6）、压缩 C 方案（D13）。所有修订处均以"（2026-11-10 修订/新增）"或"（2026-11-11 修订）"标注。 |

> 阅读约定：本文所有"现状"结论均标注了仓库内真实文件路径；未在仓库中出现的文件一律标注为"新增"。REQ 编号与 `spec.md` 一一对应。

---

# 一、需求与存量功能关系分析

本章回答三个问题：需求中哪些已被现网代码覆盖、哪些需要扩展、哪些必须新建；以及存量代码的约束会在多大程度上限制新增设计。

## 1.1 需求功能与存量功能对比

### 1.1.1 已实现功能

判定口径：存量实现与 REQ 的语义**一致**，无需改造即可复用（可增加开关或薄适配层）。

| 需求功能 | 存量功能 | 代码位置 | 匹配度 |
|---|---|---|---|
| REQ-096 runtime 用 Go + OpenAI 兼容接口 | `llm.Client` 接口 + OpenAI 兼容实现 + DashScope 实现 | `pkg/llm/client.go:27-32`、`pkg/llm/dashscope.go:135` | 100% |
| REQ-024 读取模型结束原因 | `ChatResponse.FinishReason` 已解析 | `pkg/llm/client.go:83,283,383` | 100% |
| REQ-081 RRF 融合公式（k=60） | `hybridSearch` 已实现向量 + BM25 的 RRF | `services/knowledge-service/internal/service/knowledge_service.go:231-296` | 100%（公式一致，排序实现需替换） |
| REQ-087 文档格式解析（PDF/DOCX/MD/TXT/JSON/CSV） | `parser.Parse` 按扩展名分派 | `services/knowledge-service/internal/parser/parser.go:19-35` | 100% |
| REQ-030/DFX-M-02 结构化事件与 trace 关联 | Span/SpanEvent 含 parent_id、events、metadata | `services/harness-service/internal/trace/trace.go:38-72` | 75%（结构可用，缺统一 schema 与 seq） |
| REQ-031/032 检查点保存（消息/变量/工具结果/目标/验收标准） | `checkpoint.Checkpoint` + MongoDB/内存两种 store | `pkg/agent/checkpoint/checkpoint.go:45-100` | 75%（缺 FSM 字段） |
| REQ-116 复用 agent-service 注册/执行 | `RegisterAgent`/`Execute`/`ExecuteStream`/`Resume` 契约稳定 | `proto/agent/agent.proto:252-291`、`services/agent-service/internal/service/agent_service.go:227-410` | 100% |
| REQ-064/DFX-C-03 存量数据可读 | SQLite(GORM)/MongoDB/Qdrant 客户端均在用 | `services/memory-service/internal/repository/memory_repository.go:38-49` | 100% |
| DFX-S-05 审计能力骨架 | `audit.AuditLogger` 支持 hook/exporter | `services/harness-service/internal/audit/audit.go:93-146` | 75%（缺 Harness Loop 操作类型） |
| REQ-122 特性开关基础设施 | FeatureFlag 引擎（规则/百分比/租户） | `services/harness-service/internal/featureflag/engine.go:62-549` | 100%（可直接承载 FSM 开关） |
| **REQ-140 Agent 注册与工具集查询**（2026-11-10 新增） | `Registry.ValidateHandoff(from,to)`、`Registry.GetAgentTools(agentID)`、`Agent{Tools,Handoffs,Skills,Model}` | `pkg/agent/registry.go:301,322`、`pkg/agent/agent.go:9-40` | 100%（可直接作为角色−工具集的事实源） |
| **REQ-131/140 handoff 运行时**（新增） | `HandoffTool`、`HandoffResult`、`BuildHandoffTools`、`ValidateHandoff` | `pkg/agent/handoff.go:12-160` | 75%（有工具级交接，缺"契约记录 + 五要素摘要 + 落库"） |
| **REQ-163 审批状态机**（新增） | `ApprovalFlowManager`（CreateRequest/SubmitDecision/WaitForApproval/Escalate/超时/自动批准）+ `RuleEngine` | `pkg/agent/approval/flow.go`（`ApprovalFlowManager` / `ApprovalRule`）、`pkg/agent/engine_stream.go:265-296` | 100%（模型与状态机可复用；缺口是跨进程可见性，见 U17） |
| **REQ-156 工具清单**（新增） | MCP `ListTools`（built-in + remote 工具聚合） | `services/mcp-service/internal/service/mcp_service.go:478-519` | 75%（有工具清单，无权限标签字段） |
| **REQ-157 权限规则骨架**（新增） | `PermissionRule{ToolPattern,Effect,MaxCalls,RequiresApproval}` + `PermissionMatrix`（按 agentType 的 deny 规则） | `services/harness-service/internal/rule/rule.go:209-240` | 50%（有 deny 语义与审批标记，无路径/阶段/优先级维度） |
| **REQ-142 模型配置骨架**（新增） | `LLM.Model`、`LLM.EmbeddingModel`、`LLM.Models map[string]string`；`Agent.Model` | `pkg/config/config.go:48-51`、`pkg/agent/agent.go:38` | 25%（只有模型名，无窗口/单价/能力位） |

### 1.1.2 需要扩展的功能

| 需求功能 | 存量功能 | 差异说明 | 扩展方向 |
|---|---|---|---|
| REQ-011~020 阶段验收 Agent | `LLMVerifier.Verify`（任务级、单次、fail-open） | 输入输出差异：现有输入是对话摘要 + SuccessCriteria，输出 `{passed,evidence}`；需求要求独立干净上下文、6 类注入、结构化 verdict、程序检查优先、只读。边界差异：verifier 失败当前"放行"，需求要求"未验证项不得计为通过" | 新增 `pkg/harnessloop/acceptance`；保留 `Verifier` 接口不动，新增 `StageVerifier` 独立接口；LLMVerifier 仅作为"模型判断"通道被新验收器调用 |
| REQ-038~046 上下文压缩 | `pkg/llm/compress.go` 按字符预算截断；`memory-service/working` 有 summary/selective/hybrid 策略 | 输入输出差异：现有按字符截断，无 JSONL、无五要素、无阈值触发；需求要求 80% 窗口触发 + 五要素模板摘要（**C 方案：单通道**）+ A/B | 新增 `pkg/harnessloop/compaction`；把 `pkg/llm/compress.go` 降级为"保结构截断"兜底；复用 `working.go:305-441` 的算法思想但不跨服务调用 |
| REQ-021~029 异常分类与重试 | 现有 `ErrMaxStepsReached`、LLM 错误直返、intervention stop | 业务逻辑差异：现有"无工具调用即完成"、错误即终止；需求要求 8 类异常 + 分类重试 + 仅三种停止条件 | 新增 `pkg/harnessloop/retry`；`pkg/agent/engine.go:723-745` 的完成判定仅在 `fsm.enabled=false` 时保留 |
| REQ-058~065 记忆检索 | Qdrant 向量召回 + SQLite 元数据过滤 + 时间衰减 | 输入输出差异：现有只有向量单路、无全文、无 RRF、无注入预算；需求要求 PG 全文 + pgvector + RRF + 15% 预算 | memory-service 新增 `pgindex` 子包与 `RecallForStage` RPC；保留 `Recall` 语义 |
| REQ-066~080 代码索引 | **完全缺失**（无 AST、无 watchdog、无 code_chunks） | — | 全新建（见 4.8） |
| REQ-103~114 事件协议与大屏 | 8 类流式事件 + trace + session replay + ECharts 页面 | 结构差异：现有 `StreamEvent{Type,...}` 无 schema_version/seq/severity；大屏缺实时通道 | 新增 `pkg/harnessevent`；Gateway 新增 SSE；前端新增 `/harness-loop/monitor` |
| REQ-017/087/089/091 知识广场 | 同步上传、`status="ready"` 硬编码、无 reranker | 边界差异：现有失败无状态可查、无引用页码、无隔离维度 kb_id | knowledge-service 扩展 Document 状态机、metadata、kb_id、rerank 适配 |
| REQ-110 健康度 | 无 | — | 新增 `pkg/harnessevent/health` 纯函数 |

### 1.1.3 需要新增的功能或接口

**A. FSM 运行时（REQ-001~010、031~037）**
- 输入：任务目标、workspace_root、阶段验收标准、预算、tenant/project。
- 输出：阶段流转、attempt、检查点、终态报告。
- 依赖：agent-service（阶段内执行）、MongoDB（检查点）、PG（事件/索引）、LLM。

**B. 独立验收 Agent（REQ-011~020）**
- 输入：原始需求不可变副本、阶段验收标准、产物清单、确定性检查结果、任务约束、上一轮证据。
- 输出：`AcceptanceVerdict` 结构化对象。
- 依赖：程序检查器（本地命令执行）、LLM（仅补足不可程序化项）、只读工作区视图。

**C. 代码索引子系统（REQ-066~080）**
- 输入：workspace 文件事件、文件内容。
- 输出：`code_chunks`/`code_edges`/`indexed_files`。
- 依赖：fsnotify、AST 解析、embedding、PG+pgvector。

**D. Markdown 记忆事实源（REQ-047~057）**
- 输入：主题 upsert/read/delete。
- 输出：md 文件变更 + 变更事件。
- 依赖：文件锁、原子写、memory-service 索引同步。

**E. PG 混合检索（REQ-058~065、081~086）**
- 输入：query、tenant/project、top_k。
- 输出：带来源引用与 RRF 分数的结果。

**F. CLI（REQ-097~102）**
- 输入：命令行参数 / TUI 交互。
- 输出：TUI 视图、JSON、退出码。
- 依赖：cobra、bubbletea、runtime gRPC。

**G. 事件总线与大屏（REQ-103~114）**
- 输入：runtime 事件。
- 输出：持久化事件、SSE 推送、ECharts 视图。

**H. 知识广场增强（REQ-087~095）**
- 输入：文档、kb_id、metadata。
- 输出：状态机、混合召回、rerank、引用。

**I. 多 Agent 协作编排（REQ-123~141，2026-11-10 新增）**
- 输入：b 阶段计划产物（子任务 DAG、契约、并行集、传播策略）、父 goal 与预算、角色定义。
- 输出：子任务状态机、调度决策、handoff 契约记录、d 阶段汇聚报告。
- 依赖：`agent-service`（Agent 注册、工具集、执行、审批）、`a2a-service`（交接传输）、`pkg/agent/{registry,handoff,approval}`、PG `hlp_task.*`。
- **明确不新建**：Agent 注册表、消息传输、审批队列（D14）。

**J. 模型选择（REQ-142~155，2026-11-10 新增）**
- 输入：阶段、能力要求（工具调用/结构化输出）、阶段 token 预算、策略、显式指定。
- 输出：生效 `model_id` + 候选打分 + 回退链状态 + 选择审计。
- 依赖：`config/harness_loop/models.yaml`、`pkg/llm`（现有客户端）、`hlp_task.model_selections`。

**K. 工具权限与工作目录沙箱（REQ-156~170，2026-11-10 新增）**
- 输入：工具名与标签、阶段、角色、任务策略快照、请求路径、exec 参数。
- 输出：`allow / deny / require_approval` 决策 + `rule_id` + 审计记录。
- 依赖：`pkg/harnessloop/policy` + `policy/sandbox`、`pkg/agent/approval`（审批）、`hlp_event.audit`（审计）。

## 1.2 存量功能详细分析（约束识别）

| 存量能力 | 接口契约 | 业务规则 | 扩展点 | 约束（设计必须遵守） |
|---|---|---|---|---|
| `pkg/agent/engine.go` `executeLoop` | 入参 `ExecutionContext`（含 Goal/SuccessCriteria）；出参 `ExecutionResult`；错误 `ErrMaxStepsReached` | **无工具调用即视为完成**（`engine.go:723-745`），verifier 失败可继续循环，verifier 错误 fail-open | `Verifier` 接口、`MemoryClient` 接口、`checkpoint.CheckpointStore` 接口、`EngineConfig` | 该循环不得被直接改写为 FSM，否则破坏 `/agents/execute` 契约（DFX-C-02）；MaxSteps 默认 10（`engine.go:26-31`），对长时任务明显不足，必须由 FSM 在外层接管 |
| `pkg/agent/verifier.go` | `Verify(ctx, execCtx) (bool, string, error)` | 无 criteria 直接通过；client 为 nil 通过；解析失败 fail-open | 接口可另实现 | fail-open 与 REQ-015/017 冲突，新验收器**不复用**该行为，只复用其"LLM 判定"能力 |
| `pkg/agent/checkpoint` | `Save/Get/List/Delete`，MongoDB 文档 `agent_checkpoints` | ObjectID 主键，按 step 排序 | `CheckpointStore` 接口可扩展实现 | 现结构无 stage/attempt/artifact_hash/event_watermark（`checkpoint.go:45-84`），需新 collection 而非改旧结构（DFX-C-04 向前兼容） |
| `services/memory-service` | `Save/SaveBatch/Recall/GetSessionMemory/DeleteSessionMemory/GetAllMemories/DeleteMemory` | 写入 SQLite + Qdrant；`Recall` 先 embedding 再 Qdrant 过滤，按时间衰减排序 | `MemoryRepository`、`MemoryServiceWithForgetting` | 衰减公式 `exp(-0.1*ageDay)`、阈值 0.3、720h（`memory_service_with_forgetting.go:29-37`）属于"内部实现保留"范围；Qdrant 是唯一向量通道，PG 不可用时的降级要回退到它 |
| `services/knowledge-service` | `Upload/GetDocument/ListDocuments/DeleteDocument/Search/SearchStream` | 上传同步完成解析 + 切分 + embedding；`Status` 固定 `"ready"`（`document_repository.go:128`）；BM25 为进程内懒加载索引（`document_repository.go:216-231`） | `DocumentRepository`、`search.BM25` | BM25 索引非持久、重启冷启动 O(N) 全量加载；`hybridSearch` 用 O(n²) 冒泡排序（`knowledge_service.go:274-281`），top_k 增大后延迟不可控；无 kb_id 维度，仅 tenant 隔离 |
| `services/gateway` | `/api/v2/*` + `middleware.Tenant()` | **租户为空时默认 `"default"`**（`middleware.go:51-57`），并非强制鉴权 | Gin 路由组、handler 代理模式 | DFX-S-01 要求鉴权，现状是"软租户"；新增路由必须复用同一中间件，并额外要求显式租户（见 4.12/6.3） |
| `services/harness-service` | 90+ gRPC 方法（`harness.proto:2055-2255`） | SQLite 单库 `HarnessRepository`；trace/session/abtest/eval 各自内存 + 落库 | 可新增 RPC 与子包 | 事件持久化不能塞进 harness-service 的 SQLite（容量与查询维度不匹配）；trace 已能承载父子 span，事件走 trace 桥接而非替换 |
| `pkg/llm` | `Chat/ChatStream/Embed/EmbedBatch` + `CompressionConfig` | 压缩按角色分档截断（system 12000 / recent 6000 / old 1000 字符） | `Client` 装饰器链（compression/metrics） | 装饰器模式可直接被 FSM runtime 复用；`EstimateTokens` 是 CJK 感知的粗估（`compress.go:196-206`），可用于注入预算但不能用于计费 |
| `pkg/agent/registry.go`（2026-11-10 新增分析） | `Register/Get/List/Exists/ValidateHandoff(301)/GetAgentTools(322)` | `Agent` 携带 `Tools/Handoffs/Skills/Model`；handoff 可达性按 `Agent.Handoffs` 白名单校验 | `AgentStore` 接口（可持久化到 Mongo） | **这是角色−工具集的唯一事实源**；`GetAgentTools` 返回的是"名字列表"，**不含权限标签与路径约束**——权限维度必须由 `policy` 补充（REQ-156），不可复用该返回值直接放行 |
| `pkg/agent/handoff.go`（新增分析） | `HandoffTool.ToToolDefinition`、`HandoffResult{TargetAgentID,Reason,Variables}`、`ValidateHandoff` | handoff 以"工具调用"形式触发，结果是"切换到目标 Agent"；`Variables` 可携带少量键值 | `BuildHandoffTools`/`BuildAllTools` | 是**工具级交接**，不是**契约级交接**：无五要素摘要、无未完成项、无产物哈希、不落库。本设计在其之上加契约层（4.13.4），不修改该文件 |
| `pkg/agent/approval/flow.go`（新增分析） | `ApprovalFlowManager.CreateRequest/SubmitDecision/WaitForApproval/CancelRequest/EscalateRequest`；`RuleEngine`+`ApprovalRule` | 状态机含 pending/approved/rejected/timeout；支持超时（`SetTimeout`）与自动批准（`SetAutoApprove`） | `ApprovalHooks`、`ApprovalNotifier` | **状态在进程内内存**：harnessloop-service 与 agent-service 是两个进程，跨进程读取审批结果没有现成通道。这是 U17 的成因；本设计采用"审批留在 agent-service + 薄代理查询"，而不是自建队列 |
| `services/harness-service/internal/rule`（新增分析） | `Engine.CheckTool(agentType, toolName)`、`PermissionMatrix`、`Guardrail.IsSensitiveTool` | deny 规则按 agentType 分组，支持 `ToolPattern`（前缀/通配）、`Effect`、`MaxCalls`、`RequiresApproval` | `RegisterSpec`/`GetSpec` | 只有"工具名模式"一个维度，**无阶段、无路径、无优先级层**；本设计扩其结构（追加 `PermissionTags/Stages/AllowedPaths/AllowedCommands/Scope`），并在 `pkg/harnessloop/policy` 实现真正的解析器，避免把三类维度硬塞进单层 map |

**存量缺陷（会影响新设计，必须在实现阶段一并修复）**

1. `memory_repository.DeleteBySession` 走 `sessionID==""` 分支时，最终删除条件仍拼了 `session_id = ?`（`memory_repository.go:207-212`），与"删除租户全部记忆"的语义不符；新索引层不得复制该实现。
2. `knowledge_service.Search` 的 `bm25` 分支用 `float64(topK-i)` 作为分数（`knowledge_service.go:174-181`），丢失真实 BM25 分数，无法参与 RRF 可比排序。
3. `knowledge-service` 的 BM25 索引在 chunk 新增/删除后不会失效（`document_repository.go:266-281` 只有手动 `RefreshBM25Index`），与 REQ-093 更新一致性冲突。

---

# 二、关键决策与假设

本章记录所有已拍板决策、待确认问题（spec 第 8 章 Q1~Q12）的处置结论、以及设计所依赖的假设。**凡与用户原话存在字面冲突的，单独标注，不静默取舍。**

## 2.1 决策总表（ADR）

| ID | 决策 | 备选 | 理由 | 影响 | 风险 |
|---|---|---|---|---|---|
| D1 | 记忆事实源 = 工作区 markdown；memory-service 降级为只读索引/检索层 | B：memory-service 为主、md 为导出；C：双向双写 | 用户已拍板；REQ-047/048/061 要求 md 为唯一事实源；双向双写会产生无法收敛的冲突 | 新增 `pkg/memoryfile`；memory-service 新增 `mdsync` 与 `memory.source` 开关；新增 PG 记忆索引 | 写放大、最终一致窗口、多人并发编辑 |
| D2 | PG+pgvector 仅承载"记忆索引 + 代码索引 + Harness 事件"，Qdrant 保留 RAG 文档向量与 legacy 记忆向量 | 替换 Qdrant；或 PG 只做关系表、向量仍留 Qdrant | 用户已拍板；双库并存符合 DFX-C-03；Qdrant 现有 collection 不做数据迁移 | 新增 3 个 schema；新增连接池与迁移；运维多一个有状态依赖 | 职责重叠导致误写；PG 成为新单点 |
| D3 | RAG/知识广场沿用 Go `knowledge-service`，不引入 Python+langchain | Python + langchain sidecar | 用户已拍板；现有解析/切分/BM25/Qdrant 链路已具备，重写成本高且会产生两套文档库 | 在 knowledge-service 内补 RRF 稳定排序、reranker、引用、状态机、kb 隔离 | reranker 质量弱于 cross-encoder |
| D4 | runtime 复用 Go + `pkg/llm`；CLI 用 cobra + Bubble Tea，位于仓库内 `cmd/harness-cli` | 引入 openai-go；CLI 独立 module | 用户已拍板"不强制 openai-go"；现有 client 已等价（含 FinishReason/Embed/装饰器）；独立 module 会为复用 pb 引入 `replace` 补丁 | 新增 `pkg/harnessloop`、`services/harnessloop-service`、`cmd/harness-cli` | CLI 与 runtime 版本耦合 |
| D5 | 新增大屏独立页面 `/harness-loop/monitor`，复用 Ant Design + ECharts + TanStack Query | 改造现有 Observability/Harness Dashboard | 用户已拍板；现有页面是统计卡 + 回放，交互模型不同，改造会污染现有信息架构 | 新增 `frontend/src/pages/HarnessLoop/`；新增 SSE 客户端 | 前端包体增大 |
| D6 | 新增独立服务 `harnessloop-service`（:50008），阶段内执行**委托** agent-service | 把 FSM 塞进 agent-service | 隔离 FSM 生命周期与现有 Agent 执行；独立伸缩与开关；agent-service 单文件已 981 行 | 新增服务、Dockerfile、compose 条目、网关代理 | 多一跳网络与一个部署单元 |
| D7 | FSM 检查点写 MongoDB（新 collection `harness_loop_checkpoints`），事件与索引写 PG | 检查点也写 PG | 复用 `checkpoint.CheckpointStore` 接口与 MongoDB 基建；检查点是大 blob，不宜与向量索引争 PG 缓冲 | 跨库各司其职；恢复以检查点为准、事件按 seq 校验 | 跨库一致性需显式校验（见 4.4） |
| D8 | 事件持久化 PG（`hlp_event` schema）+ Redis pub/sub 扇出 + Gateway SSE 推送 | Kafka；纯 Redis Stream | 事件量级（50 agent × 1s）PG 完全可承载；Redis 已存在，可做多副本广播；不新增中间件 | 新增事件表与索引；Gateway 新增 SSE handler | 高频写入；断线补拉依赖 seq 单调 |
| D9 | 代码索引解析：Go 用标准库 `go/ast`，TS/Python 用 tree-sitter（cgo，可选构建标签），无 cgo 时降级正则 | 全量 cgo tree-sitter；Python 侧车；纯正则 | 避免 cgo 成为全仓库构建硬依赖；Go 用标准库精度最高；TS/Python 正则精度不足但可降级 | 解析器抽象为 `Parser` 接口 + 能力标注 | 降级模式符号提取不全 |
| D10 | 中文全文检索用"应用层分词 + `to_tsvector('simple', tokens)`"，不装 zhparser/pg_jieba | 安装 PG 中文扩展 | 扩展需定制镜像、跨环境不稳定；应用层可控 camelCase/snake_case 拆分 | 写入前预处理 tokens；tsvector 列由应用层构造 | 分词质量取决于词典 |
| D11 | 知识文档索引改为异步状态机（上传返回 pending） | 保持同步上传 | REQ-089 要求四态且失败可重试；大文件同步阻塞网关 | Upload RPC 语义变更（返回 status=pending），需兼容旧调用 | 现网调用方假设 ready，需双读兼容 |
| D12 | CLI 与 runtime 走 gRPC streaming（复用 pb 风格），Gateway 对外用 REST + SSE | CLI 直连 PG/文件 | 单一状态机语义（REQ-102）；CLI 不重复实现完成判定 | 新增 `proto/harnessloop`；CLI 依赖 pb | 断线重连需事件补拉 |
| **D13**（2026-11-10 新增） | **上下文压缩采用 C 方案**：自研五要素模板摘要为唯一实际压缩通道；`ContextEngine` 仅保留为预留扩展点，一期不接入任何开源压缩引擎、无外部进程、无 Python 依赖 | A：双通道（模板 + 引擎适配，引擎一期 Noop）；B：纯截断（放弃五要素结构）；C：单通道模板摘要（选定） | 用户 2026-11-10 拍板 C；五要素需按字段强约束，模板摘要可直接满足；单通道降级判定唯一；主流引擎多为 Python 运行时，与 D3/D4 冲突（详见 4.5.2、4.5.9） | 删除 `EngineAdapter`/`NoopEngine` 装配；4.5.2/4.5.4/4.5.6/4.5.7 改写；新增 4.5.9 答辩口径 | 二期若接入引擎需重新评估延迟与压缩比 |
| **D14**（2026-11-10 新增） | **多 Agent 协作复用 `a2a-service`（通信/交接传输）与 `agent-service`（Agent 注册、技能、工具集、审批、handoff 工具）**；本组件只承担任务拆分、协作编排、契约校验与结果汇聚 | A：本组件自建 Agent 注册与传输；B：完全交给 a2a/agent-service 编排，本组件不介入；C：复用下层能力 + 本组件只做编排（选定） | 用户已关闭 spec Q「多 Agent 协作范围边界」；reuse 现状已具备（`pkg/agent/registry.go` 的 `ValidateHandoff/GetAgentTools`、`pkg/agent/handoff.go`、`pkg/agent/approval/flow.go`、`proto/a2a/a2a.proto`）；重建会产生两套注册中心与两套传输语义（REQ-140 明令禁止） | 新增 4.13；新增 `pkg/harnessloop/orchestrator`、`subtask`、`handoff`；不新增 Agent 注册存储 | 跨进程审批状态归属需明确（见 4.13.9、U17） |
| **D15**（2026-11-10 新增） | **模型选择由"模型注册表 + 任务级策略 + 阶段级覆盖"三层决定**，注册表为窗口/单价/能力的唯一事实源；运行时切换默认禁止 | A：只保留全局默认模型（现状 `pkg/config` 的 `models` map）；B：每阶段硬编码模型；C：注册表 + 策略 + 阶段覆盖（选定） | REQ-142~155 要求能力匹配校验、阶段级覆盖、回退链与成本联动；现状 `Agent.Model` 与 `pkg/config.LLM.Models` 只支持"单模型 + 简单映射"，无法表达窗口/单价/工具调用能力 | 新增 `pkg/modelregistry` + `config/harness_loop/models.yaml`；4.5.1 模型窗口表改为引用注册表 | 注册表与 `pkg/config` 的字段重复需收敛（4.14.1） |
| **D16**（2026-11-10 新增） | **工具权限采用 deny-by-default + 双层名单 + 工作目录沙箱**，权限来源优先级 system > tenant > role > task | A：仅靠提示词约束；B：仅任务级白名单；C：分层权限 + 沙箱 + 审计（选定） | REQ-156~170 与 DFX-S-07 要求默认拒绝、阶段矩阵、路径规范化、可审计；现状 `harness-service/internal/rule` 的 `PermissionMatrix` 只有 `ToolPattern/Effect/MaxCalls/RequiresApproval`，无路径与阶段维度 | 新增 `pkg/harnessloop/policy` + `policy/sandbox`；扩展 `rule.PermissionRule`；复用 `pkg/agent/approval` | 双层名单与角色工具集三处过滤易产生配置漂移（见 R22） |
| **D17**（2026-11-11 新增） | **记忆容量治理双轨**：项目 md 执行 30 主题上限 + LRU 淘汰 + 读取/更新提升到队首 + pinned 不淘汰；长期 md **不参与 LRU、不重排、不淘汰**，仅软上限 50 + `memory.capacity_warning` 告警，主题顺序为稳定写入顺序（新增追加末尾） | A：两类 md 统一 LRU（原表述，已否决）；B：长期 md 完全不限也不告警；C：项目 LRU + 长期软上限告警（选定） | 2026-11-11 用户更正；与 spec v1.2 的 REQ-050/051/057/062/104 一致；长期约束需永久保留，统一 LRU 会将其淘汰 | 4.6.1/4.6.3/4.6.4/4.6.7/4.6.8、2.3 Q10、11.2 U3 同步修订 | 长期 md 无限增长（见 R25），以告警 + 用户手动清理缓解 |

> **编号说明（2026-11-10）**：任务书原意是新增"D6~D9"，但本设计 v1.0 已占用 **D6~D12**（D6 harnessloop-service、D7 检查点 Mongo、D8 事件 PG、D9 代码索引解析器、D10 中文分词、D11 知识异步索引、D12 CLI gRPC）。为避免改写既有决策编号，本次新增决策**顺延为 D13~D16**，与任务书的 D6/D7/D8/D9 一一对应：压缩 C 方案 → **D13**、多 Agent 复用 → **D14**、模型选择 → **D15**、权限沙箱 → **D16**。

## 2.2 决策详述（拍板项）

### D1 记忆事实源 = markdown

**背景**：现状 `memory-service` 把长期记忆写在 SQLite + Qdrant，用户不可直接编辑，`git diff` 不可见（G6 判定为冲突）。需求要求 md 存于工作区、长期/项目各一文件；容量治理为双轨：**项目 md 执行 30 主题 LRU，长期 md 软上限 50 且不淘汰**（2026-11-11 修订）。

**选定方案**：
- 事实源：`<HARNESS_HOME>/memory/global.md`（跨项目）+ `<workspace_root>/.harness/memory/project.md`（每项目一个）。
- 写入路径：runtime 内 `pkg/memoryfile` 持有文件锁并做原子写；写成功后发布 `memory.written/merged` 事件（`memory.evicted` 仅由项目 md 产生；长期 md 超过软上限仅发 `memory.capacity_warning`，2026-11-11 修订）。
- 索引路径：memory-service 订阅变更（或由 runtime 调 `SyncFromMarkdown`），解析主题、切分、embedding、upsert 到 PG。
- 检索路径：memory-service `RecallForStage` 走 PG 全文 + pgvector + RRF；PG 不可用时直接读 md 做关键词匹配（REQ-064）。
- 现有的 working/episodic/semantic/cases 四层、遗忘算法、内部实现**不改**。

**影响**：`memory-service` 从"写入方"变为"索引方"，`Save` RPC 的语义需要重新定义（见 6.3 兼容策略）。

**风险与缓解**：
- 写放大：**项目 md** 的 LRU 提升若重排正文块会导致整文件 diff（长期 md 不重排、无此风险）。缓解：顺序清单与正文分离（见 4.6）。
- 一致性窗口：md 写入到索引可见默认 ≤5s（debounce）+ 同步失败重试；期间检索可能漏新主题，但注入有"最近写入主题直读"兜底。
- 并发编辑：用户手工编辑与 Agent diff 更新可能冲突。缓解：文件锁 + 写入前重读校验 content_hash，冲突时以磁盘为准并产生 `memory.resync` 事件。

**回滚**：`memory.source=legacy` 时完全走现状 SQLite+Qdrant 路径，md 文件不参与。

### D1-C｜与用户原话的显式冲突标注

用户原话："长期记忆的不用改，按照原项目的来"，同时拍板"事实源 = 工作区 markdown（方案 A）"与"memory-service 降级为只读索引/检索层"。

**冲突点**：若"长期记忆完全按原项目"字面执行，长期记忆的**写入路径**仍应是 `memory-service.Save → SQLite+Qdrant`，这与方案 A 的"md 为唯一事实源"直接矛盾。

**处理结论（最小改动解法）**：
1. **保留** memory-service 全部分层结构与内部实现：`working`（541 行）、`episodic`（420 行）、`semantic`（468 行）、`cases`（885 行）与 `MemoryServiceWithForgetting` 的衰减/阈值/720h 逻辑一行不改。
2. **仅改变长期记忆的入口与落点**：把"用户偏好 / 长期约束 / 项目事实 / 阶段任务状态"四类（对应 REQ-054）的写入改由 `pkg/memoryfile` 落到 md；episodic 与 working 仍走原 RPC。
3. **遗忘算法只作用于索引，不删除事实源**：原 720h/0.3 阈值继续用于"是否注入、是否降权"，但**不物理删除 md 主题**。物理删除只允许两种来源：用户显式删除、**项目 md** 的 LRU 淘汰；**长期 md 不因容量淘汰**（2026-11-11 修订）。否则会与"用户可直接读写记忆"冲突（用户写下的长期约束会被服务端静默删除）。
4. **开关与回滚**：`memory.source`（`markdown|legacy`，默认 `markdown`）、`memory.dual_write`（默认 `false`）。置 `legacy` 即回到基线行为，用于灰度期对比。
5. **可审计**：每次因遗忘算法被"降权/不再注入"的主题写 `memory.decayed` 事件，便于用户理解"为什么没被用到"。

> 若用户认为"长期记忆写入路径也不能改"，则方案 A 无法成立（md 无法成为事实源）。此处按"用户拍板项优先于概括性表述"处理，并在第 11 章列为待确认项 U1。

### D2 PostgreSQL 与 Qdrant 的职责边界

| 数据 | 事实源 | 索引/存储 | 归属服务 | 禁止 |
|---|---|---|---|---|
| 记忆主题正文 | markdown 文件 | PG `hlp_mem.memory_topics` | memory-service | 禁止写入 Qdrant `memories`（`source=markdown` 时） |
| 记忆向量 | 由主题正文派生 | PG `hlp_mem.memory_chunks.embedding` | memory-service | 禁止跨项目共享向量 |
| 代码分块/边 | 源码文件 | PG `hlp_code.code_chunks` / `code_edges` | codeindex（harnessloop-service 内） | 禁止写入 Qdrant |
| 知识文档分块 | 上传文件 | MongoDB（文档）+ Qdrant（向量） | knowledge-service | 禁止写入 PG（第一阶段） |
| legacy 记忆 | `memory.source=legacy` 时的写入 | SQLite + Qdrant | memory-service | 与 markdown 模式互斥 |
| Harness 事件 | runtime 产生 | PG `hlp_event.events` | harnessloop-service | 禁止写入 harness-service 的 SQLite |

**判定规则（防职责重叠）**：任何向量写入前必须声明 `vector_owner`（`memory|code|knowledge`）。`memory/code` 只能写 PG；`knowledge` 只能写 Qdrant。由一个配置常量表约束，代码评审与单测校验。

### D3 RAG 复用与增强点

现有不足（均已在上文给出代码位置）：RRF 排序 O(n²)、bm25 分支伪造分数、无 reranker、无页码引用、无 kb 维度、索引状态固定 ready、BM25 索引不随增删失效。

补齐项：
- RRF 改为 `sort.SliceStable`，去重键 = chunk_id，同分按 `document_id, chunk_index` 稳定排序（REQ-083）。
- bm25 分支返回真实分数（`search.BM25Result.Score`）。
- Reranker 适配接口 `Reranker`；一期实现 `LLMReranker`（复用 `pkg/llm`，一次调用给 top-20 打分，控制 token）；二期可选外部 HTTP cross-encoder（独立进程，非 langchain 依赖）。
- 引用结构 `SourceRef{document_id, doc_name, chunk_id, page_no, section, score}`。
- kb 维度：Document/Chunk 增加 `kb_id`，检索强制 `tenant_id + kb_id` 过滤，未授权返回空（REQ-094）。
- 索引状态机：`pending → processing → ready|failed`，失败可重试，`failed` 不参与检索。

### D4 runtime 与 CLI 技术选型

**复用 `pkg/llm` 的理由（不引入 openai-go）**：
1. `llm.Client` 已是 OpenAI 兼容（`client.go:27-32`），支持 Chat/ChatStream/Embed/EmbedBatch，覆盖执行、验收、摘要、embedding 四类调用。
2. `FinishReason` 已解析（`client.go:83`），满足 REQ-024。
3. 已有装饰器链（compression / metrics），FSM 可直接叠加预算与指标装饰器，无需重写。
4. 引入 openai-go 会产生两套客户端与两套错误语义，破坏 DFX-M-01 的统一指标。

**CLI 形态**：`cmd/harness-cli`（根 module）+ `internal/cli/{cmd,tui}`（cobra 命令树 + bubbletea 视图）。不建独立 module 的原因：需要复用 `pkg/pb` 与 `pkg/client`，独立 module 只能通过 `replace ../..` 或复制 proto 代码，二者都会破坏可发布性。用户给出的"`cmd/harness-cli` 或 `cli/`"两个位置中，选择前者。

**通信**：CLI → harnessloop-service gRPC（unary + server streaming）。CLI 不做本地状态机，只做展示与指令转发，保证 REQ-102"同一状态机语义"。

### D5 大屏

新页面 `/harness-loop/monitor`，复用 `frontend/src/main.tsx` 的 QueryClient 与 AntD ConfigProvider；ECharts 按需引入（现有 `echarts` 5.5.0 已存在，`Memory/MemoryManager.tsx`、`Observability/CostDashboard.tsx` 可参考封装方式）。不修改现有 `pages/Harness/*` 与 `pages/Observability/*`。

## 2.3 spec 第 8 章 Q1~Q17 处置结论（2026-11-10 扩至 Q17）

| # | 结论 | 说明 |
|---|---|---|
| Q1 | **采纳默认值** | PG 仅承载记忆索引 + 代码索引 + 事件；Qdrant 保留 RAG 与 legacy 记忆。理由见 D2。 |
| Q2 | **采纳默认值** | markdown 为唯一事实源，memory-service 为派生索引。冲突时以 md 为准并发 `memory.resync`（REQ-117）。 |
| Q3 | **采纳默认值，并收紧判定权** | 默认 d→c；仅当 `failed_criteria` 的类别属于 `plan_gap`/`requirement_gap` 时 d→b。**回退目标由 runtime 依据规则表裁决，模型只能给 `suggested_rollback_target`，且必须通过合法出边校验**（REQ-002/REQ-018）。这样避免验收模型随意改变状态机语义（REQ-010 禁止项）。 |
| Q4 | **采纳默认值，补充两项** | network/runtime/system 3 次、no_finish 2 次、quota 1 次、静默 120s；补充 invalid_tool_result 2 次、verification_error 2 次、timeout 2 次（均按阶段内计数）；任务总超时默认 8h（待确认 U2）。 |
| Q5 | **采纳默认值** | 按模型窗口 80% 触发；窗口由配置表提供（`qwen-max: 32768`、`qwen-plus: 131072`、`gpt-4o: 128000` 等，未知模型回退绝对阈值 60000 token）。 |
| Q6 | **采纳默认值** | 首批 Go / TypeScript / Python；Go 用标准库，TS/Python 用 tree-sitter（可选 cgo）或正则降级。 |
| Q7 | **采纳默认值** | 复用 Go knowledge-service，不引入 langchain；仅允许"实验性解析器"以独立可选服务形式存在，默认不部署。 |
| Q8 | **采纳默认值** | 复用 `pkg/llm`，见 D4。 |
| Q9 | **采纳默认值** | 新建 `/harness-loop/monitor`。 |
| Q10 | **已确认（2026-11-11 用户更正）** | 项目 = git 仓库根（无 `.git` 时取 `workspace_root`）。**项目 md 严格执行 30 主题 + LRU + 读取/更新提升 + pinned 不淘汰；长期 md 不做 LRU、不重排、不淘汰**，仅软上限 50 + `memory.capacity_warning` 告警且不删除任何主题；长期 md 主题顺序为稳定写入顺序（新增追加末尾），pinned 仅影响注入优先级（REQ-050/051/057/062，2026-11-11 修订）。 |
| Q11 | **采纳默认值** | 本地模式（CLI 直连 localhost runtime）免租户 token，但 runtime 启动时校验 `workspace_root` 位于白名单根目录内；远程模式必须携带 `X-Tenant-ID` 与 `Authorization`。 |
| Q12 | **采纳默认值** | FSM 是"长时任务"唯一入口；DAG（`services/harness-service/internal/workflow`）只用于阶段内部或独立批处理，禁止同一任务双编排。 |
| Q13 | **已确认 A（对应 U13，2026-11-10）** | 单父 8 并行 / 单租户 20 并发 / 直接子任务 20 / 深度 2；预算按并行集均分，父可回收剩余额度。按预估权重校准留至 P2 增强（U13-A）。 |
| Q14 | **采纳默认值（对应 U14）** | 每阶段 2 个回退候选；限流或连续 3 次超时触发回退；单阶段回退上限 3 次（REQ-149）。 |
| Q15 | **采纳默认值并加严（对应 U15）** | 租户策略**只能收紧不能放宽**阶段权限矩阵；任何放宽必须走平台级审批 + 审计（4.15.7 的 system 层不可覆盖）。 |
| Q16 | **采纳默认值（对应 U16）** | 一期只定义 `ContextEngine` 接口与开关（默认关闭），**不接入任何引擎**（C 方案，D13）；二期选型按压缩比与延迟评估后另定。 |
| Q17 | **已确认 A（对应 U18，2026-11-10）** | 阶段模型默认继承任务级；`quality_first` 策略下 d 阶段推荐使用更强模型（4.14.3 模板），不设为全局默认。 |

## 2.4 假设清单

| ID | 假设 | 不成立时的影响 |
|---|---|---|
| A1 | embedding 模型维度为 1024（与现有 Qdrant collection 一致，`knowledge-service/cmd/main.go:65`） | PG 向量列维度需改，且历史索引需重建 |
| A2 | PG 版本 ≥ 15，pgvector ≥ 0.7（支持 HNSW） | 只能用 ivfflat，召回质量下降 |
| A3 | 工作区可通过 volume 挂载进 runtime 容器（平台模式） | 平台模式无法索引/写记忆，只能退化为 CLI 本地模式 |
| A4 | 单任务事件量 ≤ 1 万条/小时 | 事件表分片与保留策略需调整 |
| A5 | 用户接受 md 文件位于项目目录（会出现在 `git status`） | 需改为 `~/.harness/projects/<id>/project.md` 外置布局 |
| A6 | 程序检查器可在 runtime 宿主机执行 `go test`/`go build`/`eslint` 等命令 | 验收只能走模型判断，REQ-016 的"确定性优先"无法兑现 |
| A7 | 现有 `/api/v2/agents/*` 调用方不依赖 `status` 字段的枚举细节 | agent-service 返回结构扩展会影响调用方 |
| A8 | 单 PG 实例可承载索引写入与事件写入（不引入读写分离） | 需拆库或加只读副本 |
| A9 | tree-sitter 的 cgo 构建在 CI 镜像内可完成（Linux + gcc） | TS/Python 索引降级为正则模式 |
| A10 | 前端可通过 SSE（而非 WebSocket）满足 50 卡片 1s 刷新 | 需改用 WebSocket，Gateway 需支持 upgrade |

---

# 三、总体架构

## 3.1 组件视图

```mermaid
flowchart TB
  subgraph Client["客户端"]
    CLI["harness-cli<br/>cobra + bubbletea"]
    WEB["React 19 前端<br/>/harness-loop/monitor"]
  end

  subgraph Edge["接入层"]
    GW["Gateway :9000<br/>/api/v2/harness-loop/*<br/>Tenant 中间件 + SSE"]
  end

  subgraph Runtime["FSM 运行时（新增）"]
    HLS["harnessloop-service :50008<br/>FSM · 验收 · 重试 · 检查点 · 压缩 · 事件"]
    IDX["codeindex 角色<br/>fsnotify + 解析 + 增量事务"]
    MEMF["memoryfile<br/>项目 md：LRU + 原子写<br/>长期 md：稳定顺序 + 软上限告警<br/>（2026-11-11 修订）"]
    SCHED["orchestrator/scheduler（同进程子模块）<br/>子任务 DAG · 并发 8 · 双配额 · 熔断"]
    MREG["modelregistry（同进程子模块）<br/>窗口/单价/能力 · 策略选择 · 回退链"]
    POL["policy + sandbox（同进程子模块）<br/>权限解析 · 路径 realpath · 阶段矩阵"]
  end

  subgraph Existing["存量服务（复用/增强）"]
    AS["agent-service :50006<br/>阶段内执行器"]
    MS["memory-service :50003<br/>索引/检索层"]
    KS["knowledge-service :50002<br/>知识广场"]
    HS["harness-service :50007<br/>trace/session/eval/flag"]
    A2A["a2a-service :50004"]
    MCP["mcp-service :50005"]
  end

  subgraph Data["数据层"]
    PG[("PostgreSQL 16 + pgvector<br/>hlp_mem / hlp_code / hlp_event")]
    MONGO[("MongoDB<br/>检查点 / Agent / 文档")]
    SQLITE[("SQLite<br/>各服务元数据")]
    QD[("Qdrant<br/>RAG 向量 / legacy 记忆")]
    REDIS[("Redis<br/>扇出 / 缓存")]
  end

  LLM["LLM 提供方<br/>OpenAI 兼容"]

  CLI -->|gRPC stream| HLS
  WEB -->|REST + SSE| GW
  GW -->|gRPC| HLS
  GW --> AS & MS & KS & HS
  HLS -->|ExecuteStream| AS
  HLS --> MEMF
  HLS --> IDX
  HLS --> SCHED
  HLS --> MREG
  HLS --> POL
  SCHED -->|SendTask / GetTask<br/>复用，不重建传输| A2A
  SCHED -->|Resume / 工具集查询| AS
  POL -->|高危时委托审批| AS
  HLS -->|SyncFromMarkdown| MS
  HLS -->|span 上报| HS
  MEMF -->|md 文件| WS[("项目工作区 / Git")]
  IDX -->|读取源码| WS
  HLS --> PG
  HLS --> MONGO
  MS --> PG
  MS --> QD
  MS --> SQLITE
  KS --> MONGO
  KS --> QD
  HS --> SQLITE
  HLS --> REDIS
  HLS --> LLM
  MS --> LLM
  KS --> LLM
```

## 3.2 服务边界

| 服务 | 归属 | 职责 | 明确不做 |
|---|---|---|---|
| `harnessloop-service`（新增） | 本设计 | FSM 状态机、attempt、验收编排、异常重试、检查点、压缩、事件生产、md 记忆写入（项目 md LRU / 长期 md 稳定顺序 + 软上限告警，2026-11-11 修订）、代码索引；**子任务拆分与调度（orchestrator）、模型选择（modelregistry）、工具权限与沙箱（policy/sandbox）三者为同进程子模块**（2026-11-10 修订） | 不做 Agent 注册（复用 agent-service）、不做跨进程 A2A 传输（复用 a2a-service）、不做通用 DAG（复用 harness-service workflow）、不做审批队列本体（复用 `pkg/agent/approval`） |
| `codeindex`（同二进制，`--role=indexer`） | 本设计 | watch + 哈希比对 + AST + 增量事务 | 不做检索 API（检索由 runtime 暴露） |
| `agent-service`（改造） | 存量 | 阶段内 Agent 执行、注册中心、技能 | 不再负责"任务完成判定"（仅在 `fsm.enabled=false` 时保留） |
| `memory-service`（改造） | 存量 | 记忆索引与检索、legacy 路径、分层记忆 | 不再是 md 事实源的写入方 |
| `knowledge-service`（改造） | 存量 | 文档上传解析、状态机、混合召回、rerank、引用 | 不承载代码/记忆索引 |
| `harness-service`（复用） | 存量 | trace、session replay、eval、abtest、feature flag、cost | 不承载 FSM 状态与事件持久化 |
| `gateway`（改造） | 存量 | REST 代理、租户、SSE 转发 | 不做状态计算 |

## 3.3 新增 / 改造 / 复用清单

**新增（文件级）**

```
proto/harnessloop/harnessloop.proto          # FSM、验收、检查点、事件、记忆、索引 API
proto/codeindex/codeindex.proto              # 代码索引查询与状态
pkg/harnessloop/                             # FSM 核心库（纯逻辑，可单测）
  fsm/{machine.go,transitions.go,attempt.go,terminal.go}
  acceptance/{verifier.go,context.go,checks/registry.go,checks/{go_test.go,go_build.go,lint.go,file_exists.go,custom.go}}
  retry/{classifier.go,policy.go,backoff.go,budget.go}
  checkpoint/{store.go,fsm_snapshot.go,restore.go,drift.go}
  compaction/{trigger.go,template.go,jsonl.go,engine.go,pairing.go,abtest.go}
  orchestrator/{planner.go,dag.go,scheduler.go,quota.go,fairness.go,circuit.go,aggregate.go,propagation.go}   # 新增：多 Agent 协作（4.13）
  subtask/{model.go,state.go,registry.go,repo.go}                                                          # 新增：子任务独立 FSM 与仓储（4.13）
  handoff/{contract.go,validate.go,transfer.go,record.go}                                                   # 新增：handoff 契约（4.13.4）
  modelregistry/{registry.go,selector.go,capability.go,fallback.go,hotreload.go,secret.go}                  # 新增：模型选择（4.14）
  policy/{engine.go,resolver.go,matrix.go,registry.go,cache.go,audit.go}                                     # 新增：权限解析（4.15）；规则 YAML 见 config/harness_loop/
  policy/sandbox/{realpath.go,pathguard.go,execguard.go,escape.go}                                           # 新增：工作目录沙箱（4.15.3/4.15.5/4.15.10）
  event/{schema.go,seq.go,publisher.go,idempotency.go,health.go}
pkg/memoryfile/                              # md 事实源
  {layout.go,format.go,parse.go,render.go,lru.go,capacity.go,writer.go,lock_unix.go,lock_windows.go,recover.go,events.go}   # lru.go 仅项目 md；capacity.go 双轨容量判定（2026-11-11 修订）
pkg/pgindex/                                 # PG 连接、迁移、advisory lock
  {conn.go,migrate.go,ddl/{mem.sql,code.sql,event.sql},advisory.go}
pkg/codeindex/                               # 代码索引（被 service 与 role=indexer 共用）
  {watcher.go,debounce.go,hash.go,chunk.go,parser/{parser.go,go_ast.go,treesitter.go,treesitter_cgo.go,regex.go},writer.go,queue.go,metrics.go}
pkg/rrf/                                     # 通用 RRF（记忆/代码/知识共用）
  {fuse.go,dedup.go,stable_sort.go}
pkg/harnessclient/                           # CLI 与 GUI 共用 gRPC 封装
  {client.go,stream.go}
services/harnessloop-service/
  cmd/main.go
  internal/{handler/grpc_handler.go,service/runtime.go,service/index_service.go,repository/{checkpoint_repo.go,event_repo.go,code_repo.go},watchdog/watchdog.go,config/config.go}
  Dockerfile
  config.example.yaml
cmd/harness-cli/main.go
internal/cli/
  cmd/{root.go,init.go,run.go,resume.go,status.go,logs.go,abort.go,memory.go,index.go,doctor.go}
  tui/{app.go,model.go,views/{stage.go,stream.go,events.go,help.go},theme.go}
  output/{json.go,exitcode.go}
frontend/src/pages/HarnessLoop/
  {index.tsx,Monitor.tsx,components/{AgentCardGrid.tsx,LifecycleSwimlane.tsx,HealthGauge.tsx,TerminalStream.tsx,ArtifactPanel.tsx,AnomalyDrawer.tsx,ControlBar.tsx},hooks/{useEventStream.ts,useTaskEvents.ts},api/harnessLoop.ts,types.ts}
docker/postgres/{init.sql,Dockerfile}
```

**改造（文件级）**

```
services/memory-service/internal/service/memory_service_with_forgetting.go   # 增加 source 分流（不删原逻辑）
services/memory-service/internal/service/mdsync.go                          # 新增：md -> PG 索引同步
services/memory-service/internal/pgindex/{memory_writer.go,recall.go}       # 新增：PG 读写
services/memory-service/internal/handler/grpc_handler.go                    # 新增 RPC 实现
services/memory-service/cmd/main.go                                         # 装配 source 开关与 PG
proto/memory/memory.proto                                                   # 追加 RecallForStage/SyncFromMarkdown（不删旧字段）
services/knowledge-service/internal/repository/document_repository.go       # 状态机、kb_id、BM25 失效、真实分数
services/knowledge-service/internal/service/knowledge_service.go            # RRF 稳定排序、rerank、引用
services/knowledge-service/internal/search/bm25.go                          # 增量维护与锁
services/knowledge-service/internal/rerank/{reranker.go,llm_reranker.go}    # 新增
proto/knowledge/knowledge.proto                                             # 追加 kb_id、state、sources
services/gateway/internal/router/router.go                                  # 新增 /harness-loop/* 路由
services/gateway/internal/handler/harness_loop_handler.go                   # 新增
services/gateway/internal/middleware/middleware.go                          # 增加"必须显式租户"的严格模式
services/agent-service/internal/service/agent_service.go                   # 允许注入阶段 system prompt/约束（小改）
docker/docker-compose.yaml                                                  # 新增 postgres、harnessloop-service、codeindex
Makefile                                                                    # 新增 proto-harnessloop、proto-codeindex、build-cli
frontend/src/App.tsx                                                        # 新增路由
frontend/src/components/Layout/...                                          # 新增菜单项
services/agent-service/internal/service/agent_service.go                   # stage_mode 透传 + 工具集/角色查询 RPC（供 4.13 调度、4.15 权限使用）（2026-11-10 修订）
services/agent-service/internal/handler/grpc_handler.go                     # 追加 ListAgentTools/GetAgentRoles 处理器（2026-11-10 修订）
services/mcp-service/internal/service/mcp_service.go                        # ListTools 返回值追加 permission_tags（REQ-156）（2026-11-10 修订）
proto/mcp/mcp.proto                                                         # Tool 追加 repeated string permission_tags（追加式）（2026-11-10 修订）
services/harness-service/internal/rule/rule.go                              # PermissionRule 追加 PermissionTags/Stages/AllowedPaths/AllowedCommands/Scope（4.15.7）（2026-11-10 修订）
```

**复用（不改动）**：`pkg/llm/*`、`pkg/agent/checkpoint/*`（接口与 MongoDB 实现）、`pkg/agent/verifier.go`（作为 LLM 判定通道）、`pkg/agent/engine.go`（`fsm.enabled=false` 时的执行路径）、`pkg/client/grpc.go`、`pkg/config/config.go`、`services/harness-service/internal/{trace,session,abtest,evaluate,featureflag,audit}`。

## 3.4 部署拓扑变化

`docker/docker-compose.yaml` 增量（现有 284 行基础上）：

| 新增项 | 镜像/构建 | 端口 | 卷 | 依赖 |
|---|---|---|---|---|
| `postgres` | `pgvector/pgvector:pg16`（或本地 `docker/postgres/Dockerfile` 预装 pgvector + 可选 pg_trgm） | 5432 | `pg-data:/var/lib/postgresql/data`、`./postgres/init.sql` | — |
| `harnessloop-service` | `services/harnessloop-service/Dockerfile` | 50008 | `hlp-workspace:/workspace`（只读+写记忆）、config | postgres, mongo, redis, agent-service, memory-service, harness-service |
| `codeindex`（可选独立副本） | 同一镜像，`command: ["--role=indexer"]` | 无对外端口 | `hlp-workspace:/workspace:ro` | postgres |

`gateway.depends_on` 追加 `harnessloop-service`；`frontend` 不变（通过 gateway 访问）。

**工作区挂载是部署关键**：平台模式下 `hlp-workspace` 必须同时挂载到 `harnessloop-service`（读写，用于 md 与源码）与 `codeindex`（只读）。CLI 本地模式下 runtime 进程直接跑在用户机器上，`workspace_root` 就是本地路径，PG 通过 `localhost:5432` 连接（compose 暴露端口）。两种模式共用同一套代码路径，仅"工作区来源"不同。

## 3.5 端口与依赖矩阵

| 端口 | 服务 | 依赖（启动期） | 依赖（运行期） |
|---|---|---|---|
| 9000 | gateway | 全部后端 | harnessloop, agent, memory, knowledge, harness |
| 50001 | chat-service | — | agent, memory |
| 50002 | knowledge-service | mongo, qdrant | llm |
| 50003 | memory-service | sqlite, qdrant, **postgres** | llm, harnessloop（同步事件） |
| 50004 | a2a-service | — | — |
| 50005 | mcp-service | chrome/desktop | — |
| 50006 | agent-service | mongo, mcp, memory | llm |
| 50007 | harness-service | sqlite | — |
| **50008** | **harnessloop-service** | postgres, mongo, redis, agent | llm, memory, harness |
| 5432 | **postgres** | — | — |
| 6333/6334 | qdrant | — | — |
| 27017 | mongo | — | — |
| 6379 | redis | — | — |

> **端口结论（2026-11-10 修订）**：多 Agent 协作层（4.13）、模型选择（4.14）、工具权限与沙箱（4.15）**均不新增服务、不新增监听端口**——三者以 `harnessloop-service`（:50008）内的 Go 包形式实现（`pkg/harnessloop/orchestrator`、`modelregistry`、`policy`），通过既有 gRPC 面暴露查询/预检方法（见 6.2）。子任务调度复用 `a2a-service`（:50004）与 `agent-service`（:50006）的既有端口，不扩建新通道。

## 3.6 两种运行形态

| 维度 | 平台模式（Web/容器） | 本地模式（CLI） |
|---|---|---|
| 任务提交 | `POST /api/v2/harness-loop/tasks` | `harness-cli run`（gRPC） |
| runtime 位置 | 容器 `harnessloop-service` | 本机进程（`harnessloop-service --local`）或远程容器 |
| 工作区 | volume `hlp-workspace` | 本地绝对路径（白名单校验） |
| 鉴权 | 必须显式 tenant + token | 免 token，工作区白名单 |
| 索引 | 容器内 codeindex | 本机 codeindex（fsnotify 本机路径） |
| 记忆 md | 容器内工作区 | 本地项目目录 |
| 大屏 | `/harness-loop/monitor` | 无（TUI 替代） |

两种形态共享 `pkg/harnessloop` 与同一 FSM 定义，保证 REQ-102。

## 3.7 核心链路

```mermaid
sequenceDiagram
  participant U as 用户/CLI
  participant GW as Gateway
  participant RT as harnessloop-service
  participant AS as agent-service
  participant V as 验收 Agent
  participant DS as 程序检查器
  participant EV as 事件(PG+SSE)
  participant CP as 检查点(Mongo)

  U->>GW: POST /harness-loop/tasks (goal, workspace, criteria, budget)
  GW->>RT: CreateTask
  RT->>EV: task.started(seq=1)
  RT->>CP: checkpoint(init)
  RT->>RT: 注入记忆(RecallForStage) + 构造阶段a上下文
  RT->>AS: ExecuteStream(stage=a, 注入约束)
  AS-->>RT: finish 信号 / 无 finish
  RT->>CP: checkpoint(stage完成)
  RT->>V: 启动独立验收(干净上下文 + 6类注入)
  V->>DS: go test / build / lint / 自定义断言
  DS-->>V: 程序检查结果
  V-->>RT: AcceptanceVerdict{pass/fail, evidence, failed_criteria}
  alt pass
    RT->>EV: acceptance.passed + stage.transition
    RT->>RT: next stage
  else fail
    RT->>EV: acceptance.failed(evidence)
    RT->>RT: rollback(a/b/c 按映射)
  end
  RT-->>GW: 事件流(stream)
  GW-->>U: SSE
```

---
# 四、核心模块详细设计

## 4.1 FSM Harness Loop

### 4.1.1 状态语义与进入/退出条件

| 状态 | 语义 | 进入条件 | 退出条件 | 该阶段产物 |
|---|---|---|---|---|
| `a` 需求理解 | 把原始需求转成可验收的需求条目与边界 | 任务创建（`initial`）；或 b 验收失败回退（`acceptance_failed`）；或 a 自身失败重试 | 收到 `finish` 且验收 pass → b | 需求条目清单、术语/歧义澄清、验收标准草案 |
| `b` 计划 | 产出可执行的分步计划与验收切分 | a 验收 pass；c 验收失败（计划缺陷）；d 验收失败且判定为计划缺陷 | 收到 `finish` 且验收 pass → c；验收 fail → a | 分步计划、影响文件清单、每步验收点 |
| `c` 执行编码 | 按计划改代码/改文档/跑工具 | b 验收 pass；d 验收失败（实现缺陷） | 收到 `finish` 且验收 pass → d；验收 fail → b | 变更文件、命令输出、产物哈希 |
| `d` 验收测试 | 对产物做测试与自检 | c 验收 pass | 收到 `finish` 且验收 pass → e；验收 fail → c 或 b | 测试报告、构建/lint 结果、未覆盖项 |
| `e` 结束 | 终态 | d 验收 pass | 无出边 | 终止报告 |

**关键语义（不可简化）**：
1. `e` 只能由 `d` 的"finish + 验收 pass"到达；`a→e`、`b→e`、`c→e` 一律非法（REQ-005）。
2. 每个非终态阶段进入时立即创建 attempt，`attempt_no` 连续递增（REQ-004）。
3. `finish` 是唯一合法完成入口；流关闭、消息结束、自然语言"完成了"都不算（REQ-006）。实现上 finish 必须是**结构化工具调用**（`stage_finish` 工具或带 `__finish__` 标记的 JSON），不接受文本匹配。

### 4.1.2 转移表

| # | From | To | 触发条件 | 前置校验 | 副作用 | 事件 |
|---|---|---|---|---|---|---|
| T1 | `a` | `b` | `finish(a)` + verdict=pass | 4.2.3 注入完整 | attempt(a).ended；创建 attempt(b);save checkpoint；清理阶段产物索引 | `acceptance.passed`→`stage.exited`→`stage.entered` |
| T2 | `b` | `a` | `finish(b)` + verdict=fail | evidence 非空 | attempt(b).ended；attempt(a) attempt_no+1；注入 evidence 到 a | `acceptance.failed`→`stage.transition` |
| T3 | `b` | `c` | `finish(b)` + verdict=pass | — | attempt(c) 创建；启用写权限 | 同上 pass 序列 |
| T4 | `c` | `b` | `finish(c)` + verdict=fail 且 category≠plan_gap | evidence 非空 | 回退 b；注入 evidence | fail 序列 |
| T5 | `c` | `d` | `finish(c)` + verdict=pass | 产物清单非空 | attempt(d) 创建；验收 Agent 切到只读 | pass 序列 |
| T6 | `d` | `c` | `finish(d)` + verdict=fail 且 category∈{impl_gap,test_gap} | evidence 非空 | 回退 c；保留 d 的测试报告作输入 | fail 序列 |
| T7 | `d` | `b` | `finish(d)` + verdict=fail 且 category∈{plan_gap,requirement_gap} | 规则表命中 | 回退 b；清理 d 产物快照（保留报告） | fail 序列 |
| T8 | `d` | `e` | `finish(d)` + verdict=pass | a/b/c/d 均有 passed 记录 | 终态写库；归档 checkpoint；释放工作区锁 | `task.completed` |

**白名单之外一律拒绝**：返回 `E_ILLEGAL_TRANSITION`，状态不变，写 `severity=error` 的 `stage.illegal_transition` 事件并计数（REQ-003）。

### 4.1.3 状态持久化模型

逻辑模型（物理实现见第 5 章）：

```
TaskRun
  task_id (PK)  tenant_id  project_id  workspace_root
  goal (不可变副本)  goal_hash
  current_stage  status  terminal_reason
  attempt_no_by_stage: {a:2,b:1,c:3,d:1}
  retry_count_by_stage / acceptance_fail_count_by_stage
  budget_tokens  budget_cost  consumed_tokens  consumed_cost
  created_at  updated_at

StageAttempt
  attempt_id (PK)  task_id  stage  attempt_no
  entry_reason: initial|acceptance_failed|retry|resume
  started_at  ended_at  finish_received  acceptance_id
  retry_count  acceptance_fail_count
```

**持久化顺序（REQ-009）**：每次转移采用"先写事件（含预期终态）→ 再写 TaskRun 快照（乐观锁 `version`）→ 再执行副作用（创建 attempt/释放资源）"。若进程在中间崩溃，重启时以 `TaskRun.version` 与事件水位比对：事件中已有 `stage.transition` 但快照未更新，则重放事件（幂等）修复；反之则丢弃未提交事件。

**重建一致性**：`Checkpoint.event_seq_watermark` 与 PG 中 `max(seq)` 比对，差异超过 0 即触发一次 `state.rebuild` 事件，重建结果需与快照 `version` 一致（见 4.4.4）。

### 4.1.4 仅三种停止条件

| 终态 | terminal_reason | 触发 | 终态副作用 |
|---|---|---|---|
| 正常完成 | `completed` | T8 到达 `e` | 归档、写报告、释放锁 |
| 用户中止 | `aborted` | AbortController / `abort` RPC / 大屏停止 | 中止当前 Agent 流、保存 checkpoint、单次副作用 |
| 异常耗尽 | `failed` | 某阶段某类异常 `retry_index > max_retries` | 记录失败阶段/分类/次数/最后错误/checkpoint_id |

**禁止的隐式终态**：
- MaxSteps 达上限：归入 `system` 异常（可重试 3 次），耗尽才 `failed`；不得直接 `failed` 且不得视为完成。
- 模型返回空内容：`no_finish` 类，继续重试。
- 工具全部失败：`invalid_tool_result`，继续重试。
- 上下文超限：先压缩，压缩失败再按 `length` 处理。

**中止幂等（REQ-008）**：`abort` 使用 `task_id + abort_epoch` 做幂等键，首次执行写入 `task.aborted` 与终态快照；后续请求直接返回首次结果（含相同 timestamp）。并发 abort 用 `pg_advisory_xact_lock(hashtext(task_id))` 或 TaskRun 行锁串行化。

### 4.1.5 与现有 `executeLoop` 的隔离方案

**开关**：`harness.loop.fsm.enabled`

| 项 | 取值 |
|---|---|
| 配置来源 | 优先 `harness-service` FeatureFlag 引擎（`internal/featureflag/engine.go`，支持租户/百分比/规则）；本地 `config.yaml` 的 `harness_loop.enabled` 作为兜底与离线模式 |
| 默认值 | `false`（DFX-C-02：关闭时行为与现状完全一致） |
| 求值上下文 | `{tenant_id, project_id, task_type: long_running, user_id}` |
| 生效粒度 | 任务创建时求值一次并写入 `TaskRun.fsm_enabled`；运行中不切换（避免半途语义漂移）；已创建任务不受开关变更影响 |

**隔离方式**：
- `harness.loop.fsm.enabled=false`：`/api/v2/agents/execute*` 与 `/chat*` 完全走 `pkg/agent/engine.go` 现有路径，`harnessloop-service` 不参与。`/api/v2/harness-loop/*` 返回 `E_FSM_DISABLED`。
- `enabled=true`：长时任务由 harnessloop-service 接管；阶段内执行仍调 agent-service `ExecuteStream`，但**强制注入阶段约束**（阶段目标、只读/读写权限、禁止 finish 之外的完成声明），且 `executeLoop` 的"无工具调用即完成"在该调用中被 `stage_mode=true` 抑制——agent-service 需透传该标记（`ExecuteStreamRequest.context_vars["__stage_mode__"]="1"`），引擎在 stage_mode 下遇到"无工具调用"返回 `ErrStageNotFinished` 而不是 `MarkCompleted()`。

**stage_mode 下的两处注入点（2026-11-10 新增）**：

`stage_mode=true` 不只是抑制"无工具调用即完成"，它同时是**权限与模型选择的注入载体**（4.15 / 4.14 的落地位置）：

| 注入点 | 注入内容 | 注入方式 | 生效位置 | 对应章节 |
|---|---|---|---|---|
| 工具权限 | 阶段权限矩阵（a/b 只读、c 读写、d 只读+exec 白名单、e 无工具）、双层名单快照、workspace_root 与允许子路径、exec 命令白名单、`policy_snapshot_hash` | `ExecuteStreamRequest.context_vars` 追加 `__stage_mode__=1`、`__policy_snapshot__`（紧凑 JSON，只含名单与路径，不含密钥）、`__policy_hash__` | `pkg/agent/engine_stream.go` 已有的 rule/approval 检查点（当前 `e.ruleEngine`+`e.approvalManager` 分支）升级为调用 `pkg/harnessloop/policy` 的 `Decide()`；**每次工具调用前同步校验**，拒绝即返回结构化错误码 | 4.15 |
| 模型选择 | 该阶段生效 `model_id`、注册表窗口/单价（供压缩阈值）、能力要求（是否需要 tool_call） | `context_vars` 追加 `__stage_model__`、`__model_window__`、`__requires_tool_call__` | agent-service 侧在构造 LLM 请求时以 `__stage_model__` 覆盖 `Agent.Model`；**若该模型不支持工具调用而阶段需要工具，直接拒绝启动该阶段**（不静默降级） | 4.14 |

> 约束：`context_vars` 只承载**值**，不承载策略定义文件路径——策略由 runtime 在任务创建时冻结为 `policy_snapshot`，agent-service 不得在运行期读取工作区内的策略文件（否则违反 REQ-168 自我提权限制）。stage_mode 关闭（`__stage_mode__` 缺失）时，上述两处注入不生效，行为与现状完全一致（DFX-C-02）。

**回滚策略**：
1. 关开关（秒级生效，仅影响新任务）；
2. 已存在的 FSM 任务保持可读可恢复（REQ-122），但 `resume` 走"只读恢复 + 完成当前阶段后停止"的保守模式；
3. 回滚演练纳入发布清单（见 10.4）。

### 4.1.6 防篡改（REQ-010 禁止项）

| 攻击面 | 防护 |
|---|---|
| 工具写配置文件 | 工具层路径白名单：禁止写 `config*.yaml`、`.harness/policy/`、`proto/`；命中即拒绝 + `security.violation` |
| 提示注入改变验收标准 | 验收标准在 `TaskRun.goal_hash` 中固化，运行期只读；执行 Agent 上下文中不含验收标准原文（仅含"你要满足的阶段目标摘要"） |
| 修改最大重试次数 | 重试策略来自运行期只读配置快照（创建任务时冻结 `policy_snapshot`），不允许 Agent 经工具修改 |
| 修改 FSM 定义 | 转移表编译进二进制（`pkg/harnessloop/fsm/transitions.go`），无运行时注册入口 |
| 伪装 finish | finish 必须是工具调用且携带 `attempt_id` 与 `stage`，runtime 校验两者与当前状态一致，否则丢弃并记 `invalid_finish` |

## 4.2 阶段验收 Agent

### 4.2.1 干净上下文构造

```
newAcceptance(task, stage, attempt):
  acceptance_id = uuid
  trace_id      = task.trace_id
  parent_span   = 当前 stage span（父子关系，不共享消息）
  messages      = []                       # 初始为空，不继承执行 Agent 历史
  system        = acceptance_prompt(v)     # 模板版本化，如 acceptance.v3
  workspace     = read_only_view(task.workspace_root, allowed_paths=产物清单)
  memory_ns     = "acceptance/" + acceptance_id   # 独立命名空间，禁止读写执行侧 memory
  tools         = whitelist{ read_file, list_dir, run_command(受限), grep }
```

**隔离保证（REQ-012/014）**：验收器与执行器的 `ExecutionContext` 是不同对象；验收器的 messages 由 runtime 逐条注入，任何执行侧消息（含"请判定通过"）都无法进入。单测断言：执行侧构造诱导消息后，验收输入快照的 `messages` 哈希与无诱导时一致。

### 4.2.2 注入内容与完整性校验（REQ-013）

| # | 注入项 | 来源 | 缺失处理 |
|---|---|---|---|
| 1 | 原始需求不可变副本 | `TaskRun.goal` + `goal_hash` | 缺失 → `E_ACCEPTANCE_CONTEXT_INCOMPLETE` |
| 2 | 当前阶段验收标准 | `StageCriteria[stage]` | 缺失 → 同上 |
| 3 | 实际产物清单 | attempt 内工具产生的文件路径 + 变更摘要 + 关键片段（≤N 行/文件） | 空清单且阶段为 c/d → 同上 |
| 4 | 确定性检查结果 | 程序检查器（4.2.4）输出 | 未配置检查器 → 允许，但 verdict 不能仅由模型 pass 全量通过时记为 `unverified_criteria` |
| 5 | 任务约束 | `workspace_root`（相对化）、工具权限、预算余额 | 缺失 → 同上 |
| 6 | 上一轮验收证据 | 上次 `AcceptanceVerdict.evidence`（仅 fail 时） | 首轮缺失 → 允许 |
| 3b（2026-11-10 新增） | **汇总报告**（d 阶段必填） | `orchestrator/aggregate.go` 产出的 `AggregationReport`（子任务产物、哈希、结论、冲突） | d 阶段缺失 → `E_AGGREGATION_MISSING`，验收不启动（REQ-133）；非 d 阶段不注入 |

校验在 `acceptance/context.go` 做，任何一项不满足直接拒绝启动，返回 `E_ACCEPTANCE_CONTEXT_INCOMPLETE` 且**不产生** `acceptance.started` 事件。

### 4.2.3 结构化验收结论

```json
{
  "$id": "AcceptanceVerdict/v1",
  "type": "object",
  "required": ["acceptance_id","verdict","reason","evidence","verified_by","confidence","duration_ms"],
  "properties": {
    "acceptance_id": {"type": "string"},
    "verdict": {"enum": ["pass","fail","blocked"]},
    "reason": {"type": "string", "minLength": 1},
    "evidence": {"type": "array", "minItems": 1, "items": {
      "type": "object",
      "required": ["kind","ref"],
      "properties": {
        "kind": {"enum": ["file","line","command","test","snippet"]},
        "ref": {"type": "string"},
        "excerpt": {"type": "string"}
      }}},
    "failed_criteria": {"type": "array", "items": {
      "type": "object",
      "required": ["criterion_id","category"],
      "properties": {
        "criterion_id": {"type": "string"},
        "category": {"enum": ["impl_gap","test_gap","plan_gap","requirement_gap","unknown"]},
        "detail": {"type": "string"}
      }}},
    "unverified_criteria": {"type": "array", "items": {"type": "string"}},
    "verified_by": {"enum": ["program","model","mixed"]},
    "suggested_rollback_target": {"enum": ["a","b","c","e"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "duration_ms": {"type": "integer", "minimum": 0}
  }
}
```

**约束校验**（不合规即视为验收失败并告警，REQ-015）：
- `verdict=pass` → `failed_criteria` 必须为空；`unverified_criteria` 非空时不允许整体 pass（REQ-017）。
- `verdict=fail` → `failed_criteria` 至少 1 条，`suggested_rollback_target` 必须是当前阶段的合法出边（否则丢弃建议，用默认映射）。
- 缺 `verdict` 字段 → 记 `acceptance.invalid`，按 fail 处理（fail-closed，与现有 `LLMVerifier` 的 fail-open 相反）。

### 4.2.4 确定性验证优先（REQ-016/017）

检查器注册表：

```
type Checker interface {
    ID() string
    Supports(criteria Criterion) bool
    Probe(ctx) (available bool, reason string)     // 环境探测：命令是否存在、依赖是否就绪
    Run(ctx, Workspace) (CheckResult, error)
}

type CheckResult struct {
    Status  Status      // pass | fail | unverified | error
    Evidence []Evidence
    Duration time.Duration
}
```

首批注册项：

| ID | 支持的标准类型 | 探测 | 超时 | 失败语义 |
|---|---|---|---|---|
| `go.test` | `tests_pass` | `go` 在 PATH、有 `go.mod` | 300s | fail → 直接 fail，不再调模型 |
| `go.build` | `build_pass` | 同上 | 120s | 同上 |
| `go.vet` | `static_check` | 同上 | 120s | 同上 |
| `lint` | `lint_pass` | `golangci-lint`/`eslint` 存在 | 120s | 同上 |
| `file.exists` | `artifact_exists` | 文件系统 | 1s | 缺失 → fail |
| `hash.match` | `artifact_unchanged` | 记录哈希 | 1s | 不一致 → fail（drift） |
| `cmd.exit0` | `custom`（用户自定义命令） | 白名单内命令 | 可配 | 非 0 → fail |
| `symbol.present` | `code_symbol` | 正则/AST 片段 | 5s | 缺失 → fail |

**执行策略**：
1. 先把验收标准映射到检查器；全部可程序化 → 只跑程序检查，不调用模型（省时省钱，DFX-P-06 的 10s 路径）。
2. 部分可程序化 → 程序检查先跑；任一 `fail` 直接 fail；全 `pass` 后再对剩余标准调模型（`verified_by=mixed`）。
3. 检查器 `Probe` 失败（环境缺失）→ 记 `unverified`，交给模型补足；`verdict` 不得仅因模型判断而定 pass（除非该标准本身不可程序化）。
4. 检查器超时 → `unverified` + `error` 事件，不阻塞（不把超时算作 fail，避免环境问题误判）。

**模型判断通道**：复用 `pkg/llm`，`temperature=0`，输出严格 JSON；只允许对 `unverified` 与不可程序化标准打分；token 上限 512；调用两次仍不合规 → `blocked`。

### 4.2.5 回退映射与"计划缺陷"判定

默认映射（spec Q3）：`a→a`（重试）、`b→a`、`c→b`、`d→c`。

`d→b` 的判定**不交给模型自由裁量**，采用规则表（`acceptance/checks/registry.go` 内的 `RollbackPolicy`）：

| 条件 | 目标 |
|---|---|
| `failed_criteria` 中存在 `category=plan_gap` | b |
| `failed_criteria` 中存在 `category=requirement_gap` 且 b 的产物缺少对应需求条目 | b |
| `failed_criteria` 全部为 `impl_gap`/`test_gap` | c |
| `category=unknown` 且 `confidence < 0.5` | c（保守） |
| 模型建议 b 但规则表判 c | 以规则表为准，写 `acceptance.rollback_overridden` 事件 |

回退时把 `evidence` 与 `failed_criteria` 注入下一轮执行上下文（REQ-018），注入格式为结构化块（非自然语言拼接），并附"上一轮 attempt_id"便于追溯。

### 4.2.6 只读约束与审计（REQ-019/020）

- **权限实现**：验收阶段工作区以只读视图挂载/打开；写类工具（`write_file`/`edit_file`/`delete`/`move`）从工具清单中移除，而非靠提示词约束。若 Agent 仍尝试调用 → 工具层拒绝 + `security.violation` + 审计。
- **临时目录**：`<HARNESS_HOME>/tmp/acceptance/<acceptance_id>/`，验收结束 `defer` 清理并校验为空（REQ-019 验收条件）。
- **审计字段**：`acceptance_prompt_version`、`model`、`input_digest`（6 类注入内容的 canonical JSON 哈希）、`tokens_in/out`、`duration_ms`、`verdict`、`evidence`、`verified_by`、`checker_results`。写入 PG `hlp_mem`? 否——写入 `hlp_event` 的 `acceptance` payload，并在 `harness-service` trace 中挂 span（REQ-115 回放可见）。

## 4.3 异常分类与重试

### 4.3.1 异常枚举与分类器

| category | 典型来源 | 识别方式 | retryable |
|---|---|---|---|
| `network` | LLM/HTTP 超时、连接重置、5xx、Qdrant/PG 短暂不可用 | 错误链中含 `net.Error`/`timeout`/5xx 或 gRPC `Unavailable` | 是 |
| `runtime` | Agent 内部 panic/非法状态、空响应体、JSON 解析失败 | recover 捕获、`ErrStageNotFinished` | 是（有限） |
| `system` | 磁盘满、权限错误、依赖进程缺失、MCP 工具不可用 | 错误码/`os.PathError` | 是（有限） |
| `quota` | 额度不足、429 且 retry_after 极长、预算超限 | 错误体含 `insufficient_quota`/`quota`，或 `consumed > budget` | 否（一次确认） |
| `timeout` | 任务/阶段总超时、工具执行超时 | `DeadlineExceeded` | 是（1 次，且不计入 staging 重试以外的预算） |
| `no_finish` | 静默超阈值、流结束但无 finish、模型空输出 | 心跳判定（4.3.3） | 是 |
| `invalid_tool_result` | 工具返回空/错误码/结构不符 | 4.3.5 校验器 | 是 |
| `verification_error` | 验收器自身崩溃、LLM 判定不可解析 | 验收器返回 `error` | 是 |
| `content_filter` | 模型拒绝生成 | `FinishReason=content_filter` | **否**（不可重试） |

分类器输入必须是**结构化错误**（`ErrorEnvelope{Kind, Code, HTTPStatus, GRPCStatus, Message, RetryAfter}`），禁止用字符串 contains 匹配（现状 `engine.go` 用字符串判断，易误判）。分类失败 → `system` + `classification=fallback` 标记，便于后续调参。

### 4.3.2 分类重试策略表（默认值，Q4）

| category | max_retries | backoff | 计数范围 | 说明 |
|---|---|---|---|---|
| network | 3 | 1s → 2s → 4s（±20% 抖动） | 阶段内 | 不含 LLM 流内重连 |
| runtime | 3 | 500ms → 1s → 2s | 阶段内 | 同一错误重复出现时减半上限 |
| system | 3 | 2s → 4s → 8s | 阶段内 | 依赖不可用可延长到 30s 上限 |
| timeout | 2 | 0（立即） | 阶段内 | 超时重试需从检查点恢复 |
| no_finish | 2 | 0（立即重进阶段） | 阶段内 | 第 3 次触发 → failed |
| invalid_tool_result | 2 | 200ms | 单工具调用内 | 只重发该工具调用，不重跑阶段 |
| verification_error | 2 | 500ms | 阶段内 | 连续 2 次 → 视为验收不可用，`blocked` |
| quota | 1（一次确认） | 无 | 任务级 | 确认后立即 failed，禁长退避 |
| content_filter | 0 | — | — | 立即 failed |

配置化（DFX-M-05）：`config/harness_loop/retry.yaml` 启动时加载为只读快照；变更需重启（明确提示），并写审计。

### 4.3.3 无 finish 静默检测（REQ-023/024）

```
last_activity_at = max(收到任意 token/工具结果/心跳/进度事件的时间)
silent = now - last_activity_at > silent_threshold (默认 120s)
```

**区分"静默"与"心跳"**：
- LLM 流式 token：每次 `delta` 刷新。
- 工具执行：`tool_start` / `tool_result` 刷新；长命令每 15s 发 `tool.progress` 心跳。
- 模型"思考"但无输出：无法区分 → 依赖 LLM 层 keep-alive；若网关无 keep-alive，则心跳来自 runtime 的 `agent.alive`（每 30s，由 runtime 主动发，不代表模型有进展，**不计入 last_activity**）。
- 判定只在 `stage_status=running` 且未收到 finish 时生效；验收阶段不适用。

**finish_reason 映射（REQ-024）**：

| FinishReason | 归一化 | 处理 |
|---|---|---|
| `stop` | `stop` | 正常；检查是否带 finish 工具调用，无则记 `no_finish`（不立即重试，先看是否自然结束且内容完整） |
| `length` | `length` | 视为上下文耗尽风险：先尝试压缩；压缩后仍 length 且无 finish → `runtime` 重试 |
| `tool_calls` | `tool_calls` | 正常继续循环 |
| `content_filter` | `content_filter` | 不可重试，任务 failed，payload 记原始值 |
| 其他/空 | `other` | 记 `system`（可重试 3 次），payload 保留原值 |

所有 `error` 事件必须带 `original_finish_reason` 字段。

### 4.3.4 预算隔离（REQ-028）

两类独立计数器，存储于 `StageAttempt`：

| 计数器 | 递增时机 | 上限来源 | 是否影响另一类 |
|---|---|---|---|
| `retry_count` | 任一 `category` 异常重试 | `retry.yaml` 分类表 | 否 |
| `acceptance_fail_count` | `acceptance.failed` | `acceptance.max_fails`（默认 5） | 否 |

- 验收失败回退产生的"重新进入阶段"不算 retry（`entry_reason=acceptance_failed`）。
- 异常重试产生的重新进入不算验收失败。
- 任务级总预算（token/费用）单独计量，超限 → `quota`，与上述两类都不混用。
- **压缩预算独立**：压缩触发的模型调用计入 `compaction_budget`（默认任务预算的 10%），不得挤占执行预算，也不受异常重试次数影响。

### 4.3.5 工具结果校验（REQ-025）

`ToolResultValidator` 规则：
1. 非空：结果字符串长度 > 0（允许 `"{}"` 等结构化空，但必须能被 JSON 解析）。
2. 结构：若工具声明 `output_schema`，校验必填字段；不匹配 → `invalid_tool_result`。
3. 错误标记：结果中出现 `is_error=true` / `error_code` / 非 0 `exit_code` 且有 `fail_on_nonzero=true` → `invalid_tool_result`（可配置为 `tool_failed` 非异常，供 Agent 自行处理）。
4. 重试粒度：单工具调用；同一 `tool_call_id` 重试 ≤ 2 次；跨工具批量失败不联动重试。

### 4.3.6 额度不足快速失败（REQ-029）

```
if category == quota:
    emit error(category=quota, retryable=false, retry_index=0, max_retries=1)
    terminal_reason = failed
    task.terminated(reason=quota, suggestion="补充额度后 resume")
    deadline: 决策 ≤ 30s（不含已发生的 LLM 超时）
```
禁止进入指数退避；`retry_count` 保持 0 或 1；事件 payload 必须含操作建议文本。

## 4.4 检查点与恢复

### 4.4.1 触发时机（REQ-031）

| 时机 | checkpoint 类型 | 说明 |
|---|---|---|
| 阶段进入 | `stage_entry` | 记录初始上下文、注入的记忆引用 |
| 阶段完成（finish 且验收 pass/fail） | `stage_exit` | 含产物哈希、验收结论 ID |
| 状态转移前 | `pre_transition` | 用于崩溃后回退到"转移前"的一致状态 |
| 每批工具调用完成后 | `tool_batch` | 每批 ≤ 10 个调用；长批次中途也存 |
| 压缩前/后 | `pre_compaction` / `post_compaction` | 用于压缩失败回退 |
| 用户中止 | `abort` | 保证可恢复 |
| 定时兜底 | `interval` | 默认 60s，仅当有新事件时 |

验收条件"检查点数量 ≥ 阶段数 + 转移数"可通过 `stage_entry` + `pre_transition` 保证。

### 4.4.2 检查点内容（REQ-032）

在现有 `checkpoint.Checkpoint`（`pkg/agent/checkpoint/checkpoint.go:45-84`）基础上，新增 collection `harness_loop_checkpoints`，文档结构：

```
CheckpointDoc {
  checkpoint_id, task_id, session_id, trace_id
  stage, attempt_no, attempt_id, step, kind
  subtask_id?, dag_node_id?, depth?        # 2026-11-10 新增，可选；为空表示父任务检查点（4.13.1）
  fsm_snapshot {                          # 无损还原
    current_stage, status, terminal_reason,
    attempt_no_by_stage, retry_count_by_stage, acceptance_fail_count_by_stage,
    policy_snapshot_hash
  }
  messages_ref { store: "mongo", collection, doc_id, count, digest }   # 大消息体独立存放
  plan_ref { plan_id, version }
  memory_refs [ {file, topic_key, content_hash, order_rank?} ]   # order_rank 仅 project 有意义；global 为写入序号（2026-11-11 修订）
  artifacts [ {path, hash, size, mtime} ]
  consumed { tokens, cost, compaction_tokens }
  event_seq_watermark
  created_at
}
```

**为什么 messages 用引用**：一次长任务的 messages 可达数 MB，直接内嵌会让 checkpoint 文档膨胀、拖慢恢复；独立 `harness_loop_messages` 文档按 `digest` 去重（相同历史只存一份）。

**完整性校验**：解析时校验必填字段；缺任一 → 拒绝使用，回退上一检查点（REQ-032 验收条件）。

### 4.4.3 恢复流程与幂等（REQ-033/035）

```
resume(checkpoint_id):
  1. 加载 checkpoint → 校验 schema + digest
  2. 校验 fsm_snapshot 与 TaskRun 一致性（stage/attempt/计数）
  3. 校验工作区 artifact_hash（drift 检测）
  4. 校验 event_seq_watermark ≤ PG max(seq)（否则说明事件丢失，触发 rebuild）
  5. 恢复 ExecutionContext（messages/plan/memory_refs）
  6. 重放未完成工具调用的幂等判定：
       - 查 idempotency 表 task_id+tool_call_id
       - 已存在结果 → 直接使用，不重复执行
       - 不存在 → 执行并写入
  7. 从 step+1 继续，emit checkpoint.restored
```

幂等表：PG `hlp_event.tool_idempotency(task_id, tool_call_id, result_digest, executed_at, status)`，写结果前 `INSERT ... ON CONFLICT DO NOTHING`，执行前查。

**副作用幂等**：
- 写文件：工具自身带 `before_hash` 校验，若文件已是目标内容则跳过。
- 计费：以 `usage_id`（LLM 响应 id + task_id）去重，重复恢复不重复计费。
- 事件：`event_id = hash(task_id, seq)`，消费端幂等（REQ-105）。

### 4.4.4 一致性校验与漂移（REQ-034）

| 校验项 | 不一致时 | 策略 |
|---|---|---|
| `stage/attempt_no/计数` | 快照与事件重建结果不同 | `checkpoint.drift` + 以事件重建结果为准（更接近真实），标记 `rebuilt=true` |
| `artifact_hash` | 文件被外部修改 | `checkpoint.drift`；按 `config.drift_policy`：`warn_continue`（默认）/`rollback_previous`/`terminate` |
| `consumed` 回退 | 计量变小 | 拒绝恢复（可能被人为篡改），`checkpoint.corrupted` |
| `event_seq_watermark` > max(seq) | 事件丢失 | 触发 `state.rebuild`，重建后再恢复 |

**禁止静默继续**：任何 drift 必须有事件 + 日志 + 大屏可见标记（REQ-034 验收条件）。

### 4.4.5 损坏降级与保留（REQ-036/037）

- 损坏：`checkpoint.corrupted` → 回退最近可用 checkpoint；若全部不可用 → 从当前阶段起点重跑（`entry_reason=retry`），并告警"可能重复副作用"。
- 保留：每任务最近 N=20 个 + 每阶段最后一个；终态任务默认保留 30 天；清理写审计（`audit.AuditLogger`），清理后 `resume` 返回 `E_CHECKPOINT_NOT_FOUND`（不得 panic）。
- 存储隔离：`harness_loop_checkpoints` 与现有 `agent_checkpoints` 分开，避免旧数据被新 schema 解析（DFX-C-04）。

## 4.5 上下文智能压缩

### 4.5.1 阈值（REQ-038，Q5，2026-11-10 修订）

```
window = model_registry.context_window[effective_model] ?? fallback_absolute (60000)
threshold_tokens = window * compaction.threshold_ratio (默认 0.80)
if estimate_tokens(conversation) >= threshold_tokens → 下一次模型调用前触发
```

> **与 4.14 模型注册表的关系（重要，消除重复定义）**：模型窗口的**唯一事实源是 4.14 的模型注册表**（`config/harness_loop/models.yaml` → 进程内 `ModelRegistry` 快照）。本节**不再单独维护一张模型窗口表**，下列表格仅作为"注册表默认 Seed 值"的示例，运行时一律 `ModelRegistry.Get(effective_model).ContextWindow` 取值，禁止在压缩模块内硬编码窗口数字。

注册表 Seed 示例（实际以 4.14.1 的注册表字段为准，此处只列与阈值相关的两列）：

| model_id | context_window | 80% 阈值（=window×0.8） |
|---|---|---|
| qwen-max | 32768 | 26214 |
| qwen-plus | 131072 | 104857 |
| qwen-turbo | 8192 | 6553 |
| gpt-4o | 128000 | 102400 |
| 未知模型 | 60000（绝对兜底） | 48000 |

**阈值重算时机（REQ-150 联动）**：每当生效模型发生变化（阶段级覆盖、回退切换、注册表热更新导致下一次调用换模型），压缩阈值必须**按新窗口重算**，不得沿用切换前的 `threshold_tokens`；重算结果写入 `stage.entered` 与后续 `compaction.triggered` 事件 payload，便于核对。

`EstimateTokens` 复用 `pkg/llm/compress.go:196`（CJK 感知），但要加 10% 安全余量（估算偏低估时更危险）。

### 4.5.2 压缩通道：自研模板摘要为唯一实际通道（C 方案）（REQ-039，2026-11-10 修订）

> **修订说明（2026-11-10 修订）**：本节原为"双通道架构"（模板摘要 + `NoopEngine`/`EngineAdapter` 引擎适配）。依据 2026-11-10 设计评审拍板的 **C 方案**，改为：**自研五要素模板摘要为唯一实际压缩通道**，`ContextEngine` 仅保留为**预留扩展点**，一期不接入任何开源压缩引擎、不拉起外部进程、不引入 Python 依赖。原文中"二期可接外部压缩服务"的表述同步收敛为"二期是否启用由 Q16/待确认项 U16 另行决策"。

**结论先行**：PDF 原文"通过 context-compression-engine 开启压缩"，在工程实现上落为**"自研五要素模板摘要"**；`context-compression-engine` 这一通道在本设计中**不存在运行实例**，仅作为接口占位。

接口（签名稳定，一期二期不因是否接入引擎而变化）：

```go
// Summarizer：一期唯一被装配的实现为 TemplateSummarizer（调 pkg/llm，按固定模板产出五要素，非自由摘要）
type Summarizer interface {
    Summarize(ctx context.Context, h History, tmpl TemplateVersion) (Summary, error)
}

// ContextEngine：预留扩展点。一期不装配任何实现（EngineSlot 恒为 nil）。
// 仅当 compaction.engine.enabled=true 且外部适配器已显式注册时，才可能被调用；一期该条件不会成立。
type ContextEngine interface {
    Compress(ctx context.Context, f Fragments) (Compressed, error)
}

type CompactionResult struct {
    JSONL          []byte
    TokensBefore   int
    TokensAfter    int
    Preserved      PreservedElements
    StrategyID     string   // 一期固定为 template_vN
    Degraded       bool
    DegradedReason string
}
```

一期装配表：

| 通道 | 一期实现 | 默认状态 | 通道缺失/失败是否计入 `degraded` |
|---|---|---|---|
| 模板摘要（主通道） | `TemplateSummarizer`（`pkg/harnessloop/compaction/template.go`） | 启用 | 是 |
| 引擎适配（预留位） | 不装配实现（`EngineSlot=nil`，无 `EngineAdapter`） | `compaction.engine.enabled=false` | **否**——引擎位为空是设计态，不构成失败 |

**选择 C 方案的决议理由**：
1. **可控的五要素保留**：REQ-040 要求 goal/done/todo/constraints/background 五类非空且可结构校验。模板摘要按字段约束生成，天然满足；通用压缩引擎输出为自由文本片段，需二次抽取，反而不保证五要素齐备。
2. **失败降级判定唯一**：单通道下"是否降级"只有一个判据（模板摘要是否成功）。双通道需额外定义"哪一路失败才算降级"，原设计在此处含糊，易在验收时产生争议（REQ-045 已按 C 方案修订）。
3. **避免跨语言部署复杂度**：主流开源压缩引擎以 Python 运行时为主，引入即产生"独立进程 + 跨语言 IPC + 额外镜像 + 版本矩阵"，与 D3（不引入 Python+langchain 侧车）、D4（复用 `pkg/llm`）的既定结论冲突。
4. **接口不封死**：`ContextEngine` 签名与 JSONL 的 `type` 枚举保留 `engine_fragment` 预留值，二期评估后可插入，不改调用方、不改存储结构。

**禁用表述（写作纪律）**：本设计全文中不得出现"已接入 / 已开启 context-compression-engine / 双通道压缩"一类表述。需要交代与 PDF 原文的关系时，统一写："PDF 原文的压缩引擎通道，一期降级为自研模板摘要实现，引擎位保留为空"。答辩口径见 4.5.9。

### 4.5.3 五要素保留（REQ-040）

| 要素 | 来源 | 校验 |
|---|---|---|
| `goal` | `TaskRun.goal`（不可变） | 非空且 hash 与任务创建时一致 |
| `done` | 已完成阶段/工具的确定性汇总（事件流派生） | 非空（除首阶段） |
| `todo` | 计划中未完成项 + 当前阶段目标 | 非空 |
| `constraints` | 长期约束（pinned 记忆）+ 阶段约束 | 非空 |
| `background` | 关键上下文（关键文件摘要、错误历史） | 允许"无新增背景"但字段必须存在 |

缺失任一 → `compaction.failed`，走降级（4.5.7）。

### 4.5.4 JSONL 输出 schema（REQ-041）

每行一个独立 JSON（NDJSON），`output_ref` 指向对象存储/文件：

```json
{"type":"goal","phase":"c","summary":"...","source_refs":["task:goal"],"timestamp":"2026-11-10T10:00:00Z"}
{"type":"done","phase":"c","summary":"...","source_refs":["event:1234","file:src/a.go"],"timestamp":"..."}
{"type":"todo","phase":"c","summary":"...","source_refs":["plan:step-4"],"timestamp":"..."}
{"type":"constraints","phase":"c","summary":"...","source_refs":["memory:global/constraint-db"],"timestamp":"..."}
{"type":"background","phase":"c","summary":"...","source_refs":["tool:go_test"],"timestamp":"..."}
```

字段约束：`type ∈ {goal,done,todo,constraints,background,engine_fragment}`；`source_refs` 至少 1 条；禁止出现非 JSON 行；禁止包含密钥与完整文件内容（只允许片段 + 引用）。

**`engine_fragment` 的定位（2026-11-10 修订）**：该枚举值为**预留类型**，一期不产出（C 方案下引擎位为空）。解析端必须容忍其存在（向前兼容），但一期验收口径中 `engine_fragment` 出现次数应为 0；若出现，说明有适配器被非预期装配，属配置异常，记 `compaction.engine_unexpected` 告警。

### 4.5.5 工具调用配对完整性（REQ-042）

压缩后重建消息序列时执行配对校验：

```
pairs = {}
for msg in messages:
  if msg.tool_calls: for tc in msg.tool_calls: pairs[tc.id].call = true
  if msg.role == "tool": pairs[msg.tool_call_id].result = true
assert every pair has both call & result, and no orphan
```

不通过 → 拒绝应用压缩结果，回退到"保结构截断"（保留 system + goal + 最近 N 轮 + 五要素块），并记 `compaction.rejected(reason=pairing)`。

### 4.5.6 A/B 可对比（REQ-043，2026-11-10 修订）

复用 `services/harness-service/internal/abtest/engine.go`：
- 变体维度：`strategy_id`（`template_v1` / `template_v2` / 提示词模板版本差异）。**C 方案取消了 `template+engine` 变体**——一期无引擎通道可对比，A/B 只对比"模板与提示词版本"，这已足以覆盖 REQ-043 的 5 项指标。
- 记录指标：`tokens_before`、`tokens_after`、`ratio`、`preserved_rate`（五要素齐全率）、`downstream_acceptance_rate`（后续阶段验收通过率）。
- 同一任务可用不同变体重跑（`task fork`），报告可对比；采用策略写 `compaction_policy` 配置并审计。
- 二期若启用引擎适配位（U16 决策通过），只需增加一个变体维度，不改本节的指标定义与报告结构。

### 4.5.7 失败降级（REQ-045，2026-11-10 修订）

**降级链（C 方案，三级，顺序固定）**：

| 级别 | 触发条件 | 处理 | 事件 |
|---|---|---|---|
| L0 正常 | 模板摘要成功且五要素齐全、配对校验通过 | 应用 JSONL 压缩结果 | `compaction.completed` |
| L1 保结构截断 | 模板摘要失败（超时/输出不可解析/五要素缺失/配对不合法） | 保留 `system` 全文、`goal` 原文、五要素占位块、最近 8 轮消息；中间消息按 `pkg/llm/compress.go` 分档截断（system 12000 / recent 6000 / old 1000 字符） | `compaction.degraded(reason=template_failed\|pairing\|missing_element)` |
| L2 紧急丢弃 | L1 后仍超阈值 | 丢弃最旧的非工具消息（保持 tool_call/tool_result 配对完整），保留 system + goal + 最近 2 轮 | `compaction.degraded(reason=emergency)` |

**边界（必须写清，避免验收误判）**：
- 引擎位为空（`EngineSlot=nil`、`compaction.engine.enabled=false`）**不是失败**，不得因此产生 `compaction.degraded`（REQ-045 验收条件已按此修订）。
- 仅当 `compaction.engine.enabled=true` 且已装配适配器、且该适配器调用失败时，才允许把引擎失败并入 L1 的 `reason`；一期此路径不可达。
- 任何降级都不得阻塞任务继续执行（REQ-045 核心约束），且 `compaction.degraded` 事件必须带原因。
- L1/L2 产物必须仍满足"五要素可读取"（占位块保留字段名与原值引用），否则视为 L2 失败，直接进入异常分类 `length`（见 4.3.3 finish_reason 映射）。

### 4.5.8 可观测（REQ-046，2026-11-10 修订）

`compaction.completed` / `compaction.degraded` 事件 payload：`trigger_reason(threshold|length_finish|manual)`、`tokens_before/after`、`ratio`、`duration_ms`、`degraded`、`degrade_level(L1|L2)`、`strategy_id`、`prompt_version`、`output_ref`。

指标：`hlp_compaction_total{reason,degraded}`、`hlp_compaction_ratio`（直方图）、`hlp_compaction_saved_tokens_total`、`hlp_compaction_template_failures_total{reason}`（C 方案下替代原"引擎失败"指标）、`hlp_compaction_degrade_level_total{level}`。

### 4.5.9 决议记录与答辩口径（2026-11-10 新增）

本节固定答复方式，供设计评审与论文答辩使用，避免不同章节表述漂移。

| 提问 | 答复口径 |
|---|---|
| PDF 原文要求"通过 context-compression-engine 开启压缩"，你实现了吗？ | **没有直接接入，做了等价替代。** 原文的意图是"在上下文接近窗口阈值时把历史压缩成结构化摘要"，一期用自研五要素模板摘要实现这一意图；`ContextEngine` 接口作为预留扩展点保留，一期不装配引擎、无外部进程、无 Python 依赖。 |
| 为什么不用开源压缩引擎？ | 三条：① 五要素（目标/已完成/待办/约束/背景）需按字段强约束，模板摘要可直接满足，通用引擎输出需二次抽取且不保证齐备；② 单通道降级判定唯一，避免"哪一路失败算降级"的验收争议；③ 主流引擎多为 Python 运行时，与"不引入 Python 侧车、复用 `pkg/llm`"的既定决策冲突（D3/D4）。 |
| 这是不是"缩水"？ | 不是能力缺失而是**通道收敛**：压缩的触发（80% 阈值）、产物形态（五要素 JSONL）、配对校验、A/B 对比、失败降级五条能力全部保留，仅去掉"第二个压缩实现"。接口与 JSONL 类型枚举均保留扩展位，二期可无侵入接入。 |
| 二期会不会推翻现在？ | 不会。`ContextEngine` 签名与 `engine_fragment` 类型一期就位；二期只需注册适配器并打开开关，调用方、存储与事件 schema 不变（见 4.5.2、4.5.4）。 |
| 该决策记录在哪？ | ADR **D13**（2.1 决策总表）；需求侧对应 spec v1.1 的 REQ-039 / REQ-045 修订与已关闭问题"是否接入开源上下文压缩引擎"。 |

## 4.6 Markdown 分层记忆（方案 A 核心）

### 4.6.1 目录布局

```
${HARNESS_HOME:-~/.harness}/
  memory/
    global.md                 # 长期记忆：唯一文件，跨项目
    .lock                     # 全局文件锁
    backup/global-<ts>.md
${workspace_root}/
  .harness/
    memory/
      project.md              # 项目记忆：每个项目一个
      .lock
      backup/project-<ts>.md
    policy/                   # 只读策略（阶段标准、重试、压缩阈值）
```

判定规则：
- "项目"= 含 `.git` 的仓库根；无 `.git` 时取 `workspace_root`（Q10）。
- 禁止按会话/按 Agent 生成独立文件（REQ-048）。
- `.harness/` 默认加入 `.gitignore` 候选但不强制（用户可能想版本化记忆）；设计提供 `harness-cli init` 时询问是否加入版本控制，并把 `.harness/tmp/`、`.harness/backup/` 排除。

**两类文件的治理差异（2026-11-11 修订）**：

| 治理维度 | `<workspace>/.harness/memory/project.md`（项目级） | `<HARNESS_HOME>/memory/global.md`（长期） |
|---|---|---|
| 主题上限 | 30（`MAX_TOPICS_PROJECT`，硬上限） | 50（`MAX_TOPICS_GLOBAL`，**软上限**） |
| 超限行为 | 淘汰 LRU 队尾（pinned 除外），发 `memory.evicted` | **不删除任何主题**，发 `memory.capacity_warning`，写入仍成功 |
| 主题顺序 | LRU：`order[0]` 为最近使用，文件开头即队首 | 稳定写入顺序：新增追加到末尾，已有主题位置不变 |
| 读取 | 提升到队首并持久化，发 `memory.read_promoted` | **不重排、不写盘、不改内容、不改哈希**，不产生事件 |
| 更新 | 合并正文并提升到队首 | 就地合并正文，位置不变 |
| `updated_at` | 内容变化时更新 | 内容变化时更新 |
| `order` 语义 | LRU 名次 | 仅写入序号，不代表 LRU |
| pinned | **不参与淘汰** | **仅影响注入优先级**（不参与淘汰，因为不淘汰） |
| 作用范围 | 单项目（随项目 Git 工作区） | 跨项目（位于 `HARNESS_HOME`） |

### 4.6.2 文件格式

```markdown
---
schema_version: 1
scope: project                # global | project
tenant_id: t-001
project_id: repo-a
updated_at: 2026-11-10T10:00:00Z
order_version: 42
---

<!-- order: ["constraints-api","task-login-flow","pref-commit-style"] -->

## constraints-api
<!-- meta: category=long_term_constraint; pinned=true; updated_at=2026-11-01T08:00:00Z; content_hash=sha256:ab12... -->

### API 约束
所有外部接口必须以 `/api/v2` 开头，错误码使用 `E_*` 前缀。

## task-login-flow
<!-- meta: category=task_state; pinned=false; updated_at=2026-11-10T09:30:00Z; content_hash=sha256:cd34... -->

### 当前阶段
阶段 c 第 2 次尝试：已完成登录接口，待补测试。

## pref-commit-style
<!-- meta: category=user_preference; pinned=false; updated_at=2026-11-09T12:00:00Z; content_hash=sha256:ef56... -->

### 提交规范
使用 conventional commits，英文描述。
```

**格式规则（REQ-049/052）**：
1. `frontmatter` 字段顺序固定，新增字段只能追加到末尾（DFX-C-05）。
2. `order` 区块位于正文之前：**项目 md 中第一个 key 即 LRU 队首**——这既满足术语"文件开头即队首"的直觉，又避免每次提升都重排正文块；**长期 md 中 `order` 仅表示写入顺序**，第一个 key 为最早写入、末尾为最新写入，读取不改变该区块（2026-11-11 修订）。`order` 语义由 frontmatter 的 `scope` 决定（`project` = LRU 名次；`global` = 写入序号）。
3. 主题块固定为 `## <topic_key>` + 紧邻的 `<!-- meta: ... -->` 单行 + `### <title>` + 正文。
4. `meta` 行使用固定键顺序 `category; pinned; updated_at; content_hash`，便于最小 diff。
5. 主题键规则：`^[a-z0-9][a-z0-9._-]{0,63}$`，同文件唯一；重复键按"合并"处理而非新增（REQ-049）。
6. 正文上限：默认 4000 字符/主题（可配），超限按分类策略合并/替换（REQ-055）。

**顺序元数据与正文分离的收益（写放大分析）**：

> 下表仅描述**项目 md**；长期 md 不因读取重排、不因容量删块，新增只追加到末尾，因此不存在"读取提升"与"淘汰"两行的写放大（2026-11-11 修订）。

| 操作 | 分离前（物理重排） | 分离后（设计） |
|---|---|---|
| 更新主题正文 | 改正文块（+ 可能移动位置） | 改正文块 + 改 `order` 一行 |
| 读取提升（仅 project） | 整段移动，可能重排整文件 | 只改 `order` 一行（≤ 200 字节） |
| 新增主题（project） | 插入队首，整文件后移 | 追加正文块 + `order` 前置一个 key |
| 新增主题（global） | — | 追加正文块 + `order` 末尾追加一个 key |
| 淘汰队尾（仅 project） | 删除块 + 整文件移动 | 删除块 + `order` 去掉末尾 key |

`git diff` 的变更行数与主题内容行数同量级（REQ-052 验收口径），**项目 md** 的顺序调整只体现为 `order` 一行的变化；**长期 md 读取不产生任何 diff**（2026-11-11 修订）。

### 4.6.3 四类记忆与分类规则（REQ-054）（2026-11-11 修订）

| category | 语义 | 典型来源 | 合并语义 | 顺序治理（project / global） |
|---|---|---|---|---|
| `user_preference` | 用户偏好（语言、风格、工具偏好） | 用户显式声明、对话归纳 | **替换**（新值覆盖旧值，保留最近） | project：参与 LRU；global：不参与（稳定写入顺序） |
| `long_term_constraint` | 长期约束（合规、技术栈、禁用依赖） | 用户显式声明 | **追加 + 去重**（同类约束合并成条目列表，上限 10 条） | project：参与 LRU 但默认 `pinned=true`（不淘汰）；global：不参与（pinned 仅提升注入优先级） |
| `project_fact` | 项目事实（架构、约定、已知坑） | Agent 归纳、用户确认 | **合并**（同 key 更新正文，保留历史要点 ≤ 5 条） | project：参与 LRU；global：不参与 |
| `task_state` | 阶段任务状态（当前阶段、待办） | runtime 自动写 | **替换**（只保留最新状态） | project：参与 LRU，且每次阶段流转更新；global：不参与 |

分类不可由 Agent 自由决定写错位置：写入接口要求显式 category；agent 侧提供 `memory_write(topic_key, category, title, body, pinned)` 工具，category 枚举校验。

### 4.6.4 容量治理：项目 md 的 LRU 与长期 md 的软上限（REQ-050/051）（2026-11-11 修订）

两类文件**不同轨**，参数与算法不得混用：

| 参数 | 项目级 `project.md` | 长期 `global.md` |
|---|---|---|
| 主题上限 | `MAX_TOPICS_PROJECT = 30`（硬上限） | `MAX_TOPICS_GLOBAL = 50`（**软上限**） |
| 超限行为 | LRU 淘汰队尾（pinned 除外），发 `memory.evicted` | **不删除任何主题**，发 `memory.capacity_warning`，写入仍成功 |
| 主题顺序 | `order[0]` = 最近使用（LRU 队首） | 稳定写入顺序：新增追加到 `order` 末尾，已有主题位置不变 |
| 读取 | 移动到 `order[0]` 并持久化，发 `memory.read_promoted` | **不重排、不写盘、不改内容、不改哈希**，不产生事件 |
| 更新 | 合并正文 + 提升到队首 | 就地合并正文，位置不变 |
| pinned | **不参与淘汰** | **仅影响注入优先级**（REQ-062），不参与淘汰（因为不淘汰） |

项目 md 淘汰算法（**仅 `scope=project` 执行**）：
```
on insert(new_key):
  order = [new_key] + order
  if len(order) > MAX_TOPICS_PROJECT:
      victim = first(k in reversed(order) if not topic[k].pinned)
      if victim is None: 丢弃 new_key 并告警 E_MEMORY_FULL_PINNED
      else: 删除 victim 块与 order 项; emit memory.evicted
```

长期 md 写入算法（**不淘汰**）：
```
on insert(new_key):
  order = order + [new_key]          # 追加到末尾，不前插
  if len(order) > MAX_TOPICS_GLOBAL:
      emit memory.capacity_warning   # 仅告警，不删除，写入成功
on update(existing_key):
  merge body in place                # 位置不变
on read(key):
  return content                     # 不 MoveToFront、不写盘、无事件
```

- **长期 md 永不因容量淘汰**：超过软上限 50 只告警（`memory.capacity_warning`），主题全部保留；pinned 不参与淘汰判定（spec REQ-050）。
- **长期 md 读取无副作用**：不改变主题顺序、文件内容与哈希；`order_version` 不因读取递增（REQ-051）。
- **读取提升仅项目 md**：读取/更新即置 `order[0]`，`updated_at` 只在内容变化时更新（REQ-051 与 6.6 约束 4）。
- **落盘策略**：`memory.lru_flush=immediate`（读取提升立即写盘，满足严格验收）或 `batched`（内存立即生效、≤2s 合并落盘）**仅作用于项目 md**；长期 md 读取不触发任何落盘。

### 4.6.5 原子写与并发（REQ-053）

**写流程**：
```
lock(file)                                  # 进程内 mutex + 跨进程 flock
read + parse + verify(content_hash)         # 检测外部并发修改
render(new content)                         # 稳定序列化
tmp = file + ".tmp." + pid + "." + rand
write(tmp); fsync(tmp)
rename(tmp, file)                           # 原子替换
fsync(dir)                                  # Unix；Windows 跳过
unlock
```

- 平台差异：Windows 上 `os.Rename` 使用 `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`，可覆盖；若被杀软/编辑器占用导致失败，重试 3 次（100ms/300ms/900ms），仍失败则返回 `E_MEMORY_WRITE_BUSY` 并保留 tmp（不损坏原文件）。
- 锁实现：`github.com/gofrs/flock`（新增依赖，跨平台）或自研 `lock_unix.go`/`lock_windows.go`。选 `gofrs/flock`，减少自研风险。
- 幂等：写入前若 `content_hash` 与 `order_version` 均未变化 → 跳过写盘（REQ-053 验收条件"重复写入哈希不变"）。**长期 md 的 `order_version` 仅在写入时递增，读取不改变**（2026-11-11 修订）。
- 冲突：若磁盘 `content_hash` 与内存基线不一致（用户手工改过），以磁盘为准重读后重放本次变更，并记 `memory.resync`。

### 4.6.6 损坏恢复（REQ-056）

| 阶段 | 处理 |
|---|---|
| 解析失败（frontmatter/order/meta 非法） | 复制原文件到 `backup/<name>-<ts>.md`；尝试"部分解析"保留合法主题块 |
| 部分解析仍有主题 | 用 PG 索引中的 body 补齐缺失主题（索引为派生，可回填）；发 `memory.corrupted` 告警 |
| 完全不可解析 | 若有 git 历史 → 提示用户 `git checkout`；否则从 PG 重建（全量索引存在时） |
| 重建后 | 触发全量 `SyncFromMarkdown`，索引按重建内容刷新 |

`memory.corrupted` 事件必须携带：文件、损坏位置、已保留主题数、备份路径。

### 4.6.7 变更事件与索引同步（REQ-057/061/117）（2026-11-11 修订）

**事件产生规则**：`memory.evicted` 与 `memory.read_promoted` **仅由项目 md `project.md` 产生**；长期 md `global.md` 不产生这两类事件，仅在主题数超过软上限 50 时产生 `memory.capacity_warning`（REQ-057/104）。读取长期 md 不产生任何记忆变更事件。

项目 md 事件示例：

```json
{
  "event_type": "memory.merged",
  "payload": {
    "file": "/workspace/.harness/memory/project.md",
    "scope": "project",
    "topics": [{"topic_key":"task-login-flow","action":"updated","content_hash":"sha256:cd34","order_rank":1}],
    "order_version": 43,
    "source": "agent|user|watcher"
  }
}
```

长期 md 超软上限事件示例（**不删除、不重排**）：

```json
{
  "event_type": "memory.capacity_warning",
  "payload": {
    "file": "/home/u/.harness/memory/global.md",
    "scope": "global",
    "topic_count": 51,
    "max_topics": 50,
    "action": "warning_only",
    "order_version": 51
  }
}
```

同步链路：
1. 写成功 → 立即发 `memory.*` 事件（事件流，供大屏/审计）；**长期 md 读取不发事件**。
2. 调 memory-service `SyncFromMarkdown(file, changed_topics, order_version)`：
   - 成功 → 索引 upsert/delete，返回 `indexed_at`。
   - 失败 → 进入重试队列（1s/5s/30s，上限 5 次）→ 仍失败发 `memory.index_degraded`。
3. 文件 watcher（runtime 内，复用 fsnotify）监听 `<workspace>/.harness/memory/*.md` 与 `<HARNESS_HOME>/memory/global.md`，5s debounce，兜底用户手工编辑；发现 `order_version` 或 hash 不一致 → 全量解析该文件并同步。**长期 md 的 `order_version` 变化仅来自写入**。
4. 时效目标：md → 索引可见 ≤ 10s（REQ-061/117 验收条件），其中同步路径 P95 ≤ 1s，watcher 路径 ≤ 5s + 处理时间。

**单向边界**：只允许 md → PG。memory-service 的索引层**不得回写 md**（除用户显式删除/修复操作），避免"索引修正正文"造成事实源被派生数据污染。

**遗忘的边界（D1-C 落地）**：
- memory-service 的衰减/阈值/720h 继续运行，但只把主题标记为 `decayed=true`（不注入），**不删除** `memory_topics` 行与 md。
- 物理删除仅两类：用户删除（`memory deleted`）、**项目 md 的 LRU 淘汰**（`memory.evicted`）；**长期 md 不因容量淘汰**（2026-11-11 修订）。
- 每次因衰减被跳过注入，写 `memory.decayed`（debug 级，避免刷屏），并提供 `harness-cli memory --decayed` 查看。

### 4.6.8 实现影响与迁移兼容（2026-11-11 修订）

> 本节只描述设计对实现的影响与兼容约束，不含实现代码。

**`pkg/memoryfile.Store` 按 `scope` 分流**：

| 能力 | `ScopeProject`（`project.md`） | `ScopeGlobal`（`global.md`） |
|---|---|---|
| `Insert` | 前插到 `order[0]` | **追加到 `order` 末尾**（不前插） |
| `Get` / `Read` | `MoveToFront` 并持久化，发 `read_promoted` | **不 `MoveToFront`、不写盘、不发事件** |
| 容量上限 | 30，超限执行 LRU 淘汰（pinned 不淘汰） | 软上限 50，超限**仅发 `capacity_warning`**，不删除 |
| 事件 | `evicted` / `read_promoted` / `written` / `merged` | `written` / `merged` / `capacity_warning`（容量类事件仅后者，且不产生 `evicted` / `read_promoted`） |

- 存储层不新增对外 RPC；`SyncFromMarkdown` 的 `order_version` 由 `scope` 解释（project = LRU 顺序版本；global = 写入序号版本）。
- **迁移/兼容**：已有 `global.md` 的 `order` 字段**无需重排**，按现值视为写入顺序；升级过程不得重置顺序、不得触发一次性重排写盘，也不得把历史读取记录回灌为顺序调整。
- 观测与运维：长期 md 的主题数与文件大小纳入 `memory.capacity_warning` 告警上下文；项目 md 的 LRU 行为与既有测试口径保持不变。
- 测试口径见 9.6 的"记忆容量治理双轨验证"。

## 4.7 记忆检索与动态注入

### 4.7.1 PG 表结构（DDL 草案）

```sql
CREATE SCHEMA IF NOT EXISTS hlp_mem;

CREATE TABLE hlp_mem.memory_topics (
  topic_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL,
  project_id    text NOT NULL,
  scope         text NOT NULL CHECK (scope IN ('global','project')),
  topic_key     text NOT NULL,
  category      text NOT NULL CHECK (category IN ('user_preference','long_term_constraint','project_fact','task_state')),
  title         text NOT NULL,
  body          text NOT NULL,
  pinned        boolean NOT NULL DEFAULT false,
  order_rank    integer NOT NULL,               -- project：LRU 名次（0=队首）；global：写入序号（不参与 LRU）
  content_hash  text NOT NULL,
  source_file   text NOT NULL,
  updated_at    timestamptz NOT NULL,
  indexed_at    timestamptz NOT NULL DEFAULT now(),
  decayed       boolean NOT NULL DEFAULT false,
  tsv           tsvector,
  UNIQUE (tenant_id, project_id, scope, topic_key)
);

CREATE INDEX idx_mem_topics_tenant ON hlp_mem.memory_topics (tenant_id, project_id, scope, order_rank);
CREATE INDEX idx_mem_topics_cat    ON hlp_mem.memory_topics (tenant_id, project_id, category);
CREATE INDEX idx_mem_topics_tsv    ON hlp_mem.memory_topics USING GIN (tsv);

CREATE TABLE hlp_mem.memory_chunks (
  chunk_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  topic_id      uuid NOT NULL REFERENCES hlp_mem.memory_topics(topic_id) ON DELETE CASCADE,
  chunk_index   integer NOT NULL,
  content       text NOT NULL,
  content_hash  text NOT NULL,
  embedding     vector(1024),
  UNIQUE (topic_id, chunk_index)
);

CREATE INDEX idx_mem_chunks_hnsw ON hlp_mem.memory_chunks
  USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
```

> 设计取舍：spec 6.6 只定义"记忆主题"一个逻辑对象；此处拆为 `memory_topics`（主题级，用于全文/元数据/顺序）与 `memory_chunks`（向量级，用于按需召回），避免"长主题只能整段 embedding"导致召回粒度粗。**不设独立 `memories` 表**，避免与 topics 语义重复。`order_rank` 仅对 `scope='project'` 表示 LRU 名次；对 `scope='global'` 表示写入序号，读取不更新（2026-11-11 修订）。

### 4.7.2 分词方案（REQ-076，D10）

不依赖 zhparser，应用层生成 `tokens` 并写 `tsv = to_tsvector('simple', tokens)`：

| 输入成分 | 处理 | 示例 |
|---|---|---|
| camelCase / PascalCase | 按大写边界拆分 + 小写化 | `getUserProfile` → `get user profile` |
| snake_case / kebab-case | 按下划线/连字符拆分 | `get_user_profile` → `get user profile` |
| 缩写词 | 连续大写视为整体，同时展开 | `HTTPServer` → `http server`、`http`、`server` |
| 中文 | 连续汉字按 2-gram + 3-gram 生成，另加词典词 | `用户偏好设置` → `用户 偏好 设置 用户偏 户偏好 …` |
| 符号/路径 | `a/b.go` → `a b go`；`.` 保留在 `file.ext` 的 `ext` 上 | — |
| 停用词 | `the/a/of/的/了/是` 过滤 | — |

词典来源：`config/harness_loop/tokenizer.dict`（可热更新，加载失败回退内置），包含领域词（如"验收""状态机""上下文压缩"）。同一 query 使用同一分词器，保证可复现。

`pg_trgm` 作为可选增强（`ILIKE` 兜底），若扩展缺失则跳过，不影响主链路。

### 4.7.3 混合召回与 RRF（REQ-058/059/060）

```mermaid
flowchart LR
  Q["query"] --> T["应用层分词"]
  T --> F["全文召回 top_k*2"]
  F --> C{"结果 ≥ top_k 且非语义型?"}
  C -->|是| R["RRF 融合"]
  C -->|否| E["embedding + pgvector top_k*2"] --> R
  R --> D["去重(topic_key) + 稳定排序"]
  D --> H["健康度/衰减加权"]
  H --> O["top_k + 引用元数据"]
```

**按需向量的判定（REQ-060）**：
- 全文结果数 ≥ `top_k` **且** query 长度 ≤ 8 个 token **且** 无疑问/语义标记（`如何/为什么/怎么/类似/相关/similar/how/why`）→ 不调 embedding。
- 否则调 embedding（记录 `vector_trigger_reason=insufficient|semantic`）。

**RRF**：`score = Σ 1/(k + rank)`，`k=60` 可配；重叠条目分数累加、只出现一次。**同分排序键按 scope 区分（2026-11-11 修订）**：项目 md 使用 `(category_priority, order_rank, topic_key)`（`order_rank` 为 LRU 名次）；长期 md **不使用 `order_rank` 表达 LRU**（其 `order_rank` 仅为写入序号），改用 `(category_priority, pinned DESC, updated_at DESC, topic_key)` 作为稳定排序键。

`category_priority`（注入相关）：`long_term_constraint=0`、`project_fact=1`、`user_preference=2`、`task_state=3`。

> 召回与重排**不得写回源文件**：长期 md 无 LRU 副作用，排序只发生在检索结果集内（2026-11-11 修订）。

### 4.7.4 动态注入（REQ-062/063）

**触发点**：每个阶段进入前（`stage.entered` 之后、执行 Agent 第一次 LLM 调用之前）。

**查询构造**：
```
query = 阶段目标 + 计划摘要（b/c/d）+ 当前任务关键词
filters = {tenant_id, project_id, scope in (global, project), category in 阶段相关集}
```

阶段与分类的相关性矩阵：

| 阶段 | 优先分类 | 说明 |
|---|---|---|
| a | `user_preference`,`project_fact` | 理解需求偏好的表达与约束 |
| b | `long_term_constraint`,`project_fact` | 计划必须遵守技术约束 |
| c | `project_fact`,`long_term_constraint`,`task_state` | 编码需要项目约定与当前状态 |
| d | `task_state`,`project_fact` | 验收关注"做到了什么" |

**预算（15%）**：`budget_tokens = min(0.15 * context_budget, compaction.threshold * 0.15)`；单条注入上限 `min(600 tokens, 剩余预算)`；按优先级填充，超出即截断；总注入 token 写入 `stage.entered` 事件便于审计。**`pinned=true` 的主题在同等相关性下优先注入，该规则对项目 md 与长期 md 均适用；对长期 md 仅影响注入顺序，不改变文件顺序与内容**（REQ-062，2026-11-11 修订）。

**注入格式（可追溯，REQ-063）**：

```
[记忆 topic=constraints-api file=.../project.md updated=2026-11-01 score=0.032 rank=1]
所有外部接口必须以 /api/v2 开头...
```

禁止把 md 原文整段注入；只注入命中主题正文（必要时截断并在末尾标注 `…[truncated]`）。

### 4.7.5 降级（REQ-064）

```
if PG unavailable (连接失败/超时 > 500ms):
    emit memory.index_degraded
    fallback: 直接解析 global.md + project.md，做关键词匹配
              （应用层分词 + TF 简单打分，无 embedding，无 RRF）
    至少注入 1 条相关记忆（pinned 优先）
```

降级路径同样受 15% 预算约束，且注入元数据标注 `source=file_fallback`。

### 4.7.6 检索质量指标（REQ-065）

`hlp_memory_recall_latency_ms`（P50/P95）、`hlp_memory_recall_hit_total`、`hlp_memory_inject_tokens_ratio`、`hlp_memory_injected_acceptance_rate`（注入后阶段验收通过率，按任务/阶段聚合）。

## 4.8 代码索引

### 4.8.1 解析器选型（D9）

| 方案 | 精度 | 构建成本 | 跨平台 | 运行时开销 | 结论 |
|---|---|---|---|---|---|
| Go 标准库 `go/ast`（Go 语言） | 高 | 零 | 好 | 低 | **选用**（Go） |
| tree-sitter cgo binding（`smacker/go-tree-sitter`） | 高 | 需 gcc、cgo | 中（Windows 需 MinGW） | 低 | **选用（可选构建标签）**（TS/Python） |
| tree-sitter CLI 子进程 | 高 | 需安装 CLI | 好 | 每文件进程启动 ~50-100ms | 不选（P95 2s 内可但批量差） |
| Python 侧车（tree_sitter 包） | 高 | 需 Python | 好 | 跨进程 IPC | 不选（用户决策 3 精神：不引入 Python） |
| 正则/启发式 | 低 | 零 | 好 | 低 | **降级兜底** |

落地：
- `parser.Parser` 接口：`Parse(file) (Symbols, Imports, Calls, error)` + `Capability() Capability`。
- 构建标签：`//go:build cgo` 启用 tree-sitter；`//go:build !cgo` 走 `regexParser`，能力标注 `degraded=true`。
- 运行时开关 `codeindex.parser=auto|go_ast|tree_sitter|regex`；`auto` 按语言与构建能力选择。
- 索引结果中写入 `parser_id` 与 `degraded`，检索结果可展示"降级索引"标记，避免用户误判。

### 4.8.2 支持语言与切片策略

| 语言 | 解析器 | 符号 | 边 |
|---|---|---|---|
| Go | `go/ast` | func/method/type/const/var | call（同包内 + 跨包 import 路径）、import |
| TypeScript | tree-sitter（tsx/ts） | function/arrow/class/method/interface | call（标识符近似）、import |
| Python | tree-sitter | def/class | call、import |

**切片规则**：
1. 一个函数/方法 = 一个 chunk（含前导 doc/注释与装饰器）。
2. 文件级 import 块 = 一个 `chunk_type=header` chunk（无 symbol）。
3. 超过 1200 token 的函数按 800 token 窗口切分，重叠 100 token，`chunk_type=function_part`，`parent_symbol` 指向原函数。
4. 类中方法独立成 chunk，`symbol=Class.Method`。
5. chunk 必须带 `start_line/end_line`（1-based，含注释），误差 ≤ 2 行（REQ-066 验收口径）。

### 4.8.3 DDL 草案

```sql
CREATE SCHEMA IF NOT EXISTS hlp_code;

CREATE TABLE hlp_code.indexed_files (
  tenant_id     text NOT NULL,
  project_id    text NOT NULL,
  file_path     text NOT NULL,
  language      text NOT NULL,
  content_hash  text NOT NULL,
  mtime         timestamptz NOT NULL,
  indexed_at    timestamptz,
  status        text NOT NULL CHECK (status IN ('pending','indexing','indexed','failed')),
  failure_count integer NOT NULL DEFAULT 0,
  last_error    text,
  parser_id     text,
  degraded      boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, project_id, file_path)
);

CREATE TABLE hlp_code.code_chunks (
  chunk_id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        text NOT NULL,
  project_id       text NOT NULL,
  file_path        text NOT NULL,
  language         text NOT NULL,
  chunk_type       text NOT NULL CHECK (chunk_type IN ('function','method','header','function_part','class')),
  symbol           text,
  parent_symbol    text,
  start_line       integer NOT NULL,
  end_line         integer NOT NULL,
  content          text NOT NULL,
  content_hash     text NOT NULL,
  parser_id        text NOT NULL,
  embedding        vector(1024),
  content_tokens   text,
  content_tsvector tsvector,
  indexed_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_code_chunks_file ON hlp_code.code_chunks (tenant_id, project_id, file_path);
CREATE INDEX idx_code_chunks_tsv  ON hlp_code.code_chunks USING GIN (content_tsvector);
CREATE INDEX idx_code_chunks_vec  ON hlp_code.code_chunks USING hnsw (embedding vector_cosine_ops)
  WITH (m = 16, ef_construction = 64);

CREATE TABLE hlp_code.code_edges (
  edge_id      bigserial PRIMARY KEY,
  tenant_id    text NOT NULL,
  project_id   text NOT NULL,
  edge_type    text NOT NULL CHECK (edge_type IN ('call','import')),
  source_symbol text NOT NULL,
  target_symbol text NOT NULL,
  source_file  text NOT NULL,
  target_file  text,
  start_line   integer,
  UNIQUE (tenant_id, project_id, edge_type, source_symbol, target_symbol, source_file)
);

CREATE INDEX idx_code_edges_source_file ON hlp_code.code_edges (tenant_id, project_id, source_file);
CREATE INDEX idx_code_edges_target_file ON hlp_code.code_edges (tenant_id, project_id, target_file);
CREATE INDEX idx_code_edges_source_sym  ON hlp_code.code_edges (tenant_id, project_id, source_symbol);

CREATE TABLE hlp_code.embedding_cache (
  content_hash text PRIMARY KEY,
  embedding    vector(1024) NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
```

`embedding_cache` 是为解决"单事务内不能长时间外呼 embedding"而设计（见 4.8.5）。

### 4.8.4 watchdog 与 debounce（REQ-068）

**选型**：Go `fsnotify`（新增依赖 `github.com/fsnotify/fsnotify`），不引入 Python watchdog（D3/D9 一致）。

```
watcher:
  pending = set<path>, timer = nil, mu
  on_event(path):
      if ignored(path): return          # .git, node_modules, dist, .harness/tmp, bin, vendor
      pending.add(path)
      timer.Reset(5s)                   # 每次事件重置（REQ-068）
  on_timer():
      batch = drain(pending)
      queue.Submit(batch)
```

**跨平台补强**：
- Linux inotify：可靠。
- macOS FSEvents：可靠，但目录级事件多，需按路径过滤。
- Windows ReadDirectoryChangesW：对"编辑器原子保存（temp+rename）"可能只报 rename 不报 write。补强：每 30s 做一次 `mtime` 扫描（仅比较 `indexed_files` 中已索引文件 + 目录树深度 ≤ 6），差异补入 pending。
- 单目录监听上限：inotify watch 数量可配；超过上限降级为 60s 全量 mtime 扫描。

**忽略规则**：`.git/`、`node_modules/`、`vendor/`、`dist/`、`build/`、`bin/`、`*.min.js`、`*.lock`、`.harness/tmp/`、二进制扩展名（图片/压缩包）。

### 4.8.5 四步增量链路与事务边界（REQ-069~075）

```mermaid
sequenceDiagram
  participant W as watcher(debounce 5s)
  participant Q as 索引队列(并发4)
  participant DB as PG
  participant P as 解析器
  participant E as embedding

  W->>Q: flush(paths)
  Q->>DB: SELECT content_hash,status FROM indexed_files WHERE path
  alt 文件不存在
    Q->>DB: BEGIN; DELETE chunks; DELETE edges; DELETE indexed_files; COMMIT
  else hash 相同
    Q->>Q: skip
  else hash 不同/新增
    Q->>DB: BEGIN; pg_advisory_xact_lock(hashtext(path))
    Q->>P: parse(file)               # Go: 标准库 / TS/Py: tree-sitter
    P-->>Q: chunks + edges
    Q->>DB: 查 embedding_cache(chunk_hash)
    Q->>E: 未命中批量 embedding（≤64/批）
    E-->>Q: vectors
    Q->>DB: DELETE chunks/edges WHERE file_path
    Q->>DB: INSERT chunks(+tsvector,+embedding)
    Q->>DB: INSERT edges
    Q->>DB: UPSERT embedding_cache
    Q->>DB: UPDATE indexed_files(status=indexed,hash,indexed_at)
    Q->>DB: COMMIT
  end
```

**关键取舍：embedding 在事务内还是外？**

spec 的时序图把 `BEGIN → DELETE → 解析 → embedding → INSERT → COMMIT` 放在同一事务（REQ-073 明确要求五步同事务）。若 embedding 走网络（~200-800ms/批），事务持锁时间会显著超过 DFJ-P-01 的 2s 目标。

解法（不破坏"单事务"要求）：
1. 事务内仍然"删除 + 插入 + 状态更新"一体（原子性满足）。
2. embedding **优先命中 `embedding_cache`**（chunk 级 content_hash），增量更新时未变更函数的分块哈希不变 → 直接命中，无需外呼。
3. 未命中部分在事务内批量外呼，但限制 `batch ≤ 64`、`timeout 3s`；超时则回滚，标记 `failed` 重试（旧索引保持可用）。
4. 全量索引模式允许"事务外预计算 + 事务内写入"（REQ-075 允许全量批量提交，语义上仍以文件为单位原子）。

这样既满足一致性与原子性，又把常见增量（改一个函数）的 P95 压到 2s 内。

**按文件路径清理（REQ-070/072）**：`DELETE FROM code_chunks WHERE tenant,project,file_path`；`DELETE FROM code_edges WHERE source_file=path OR target_file=path`（后者覆盖"删除被调用函数后指向它的边消失"）。

### 4.8.6 并发安全（REQ-077）

- 同一文件：`pg_advisory_xact_lock(hashtext(tenant||project||path))` + 进程内 per-path mutex（先本地去重，减少 PG 锁竞争）。
- 跨文件：worker pool 并发 4（可配 1-16），全局队列 FIFO + 同文件去重（`pending` set 保证）。
- 边唯一性：`code_edges` 唯一约束 + `ON CONFLICT DO NOTHING`，防重复边。
- 死锁预防：所有事务按"文件路径字典序"获取 advisory lock（批量多文件时），避免交叉等待。

### 4.8.7 全量与增量（REQ-075）

| 模式 | 触发 | 事务粒度 | 参数 |
|---|---|---|---|
| 全量 `full` | `harness-cli index --full`、首次索引、`index rebuild` | 每 200 文件/批 | `SET LOCAL maintenance_work_mem='256MB'`；结束后 `CREATE INDEX CONCURRENTLY`（若索引不存在） |
| 增量 `incremental` | watcher / `index --paths` | 单文件 | 默认 `maintenance_work_mem` |

全量期间读旧索引可用（REQ-DFX-P-02）：先在临时表/新分区写入，完成后原子切换？——简化：逐文件 upsert + 事务，读路径始终看到"已提交"状态；全量只影响未索引文件，已索引文件保持旧版本直到其事务提交。满足"批量期间不阻塞检索"。

### 4.8.8 性能指标与验证（REQ-080，DFX-P-01/02/03）

| 指标 | 目标 | 测量方法 |
|---|---|---|
| 单文件增量（≤500 行） | P95 ≤ 2s，P99 ≤ 5s | 基准仓库 + 注入 100 次单文件修改，统计端到端（事件→事务提交） |
| 批量 100 文件 | P95 ≤ 60s | 批量 touch+修改，统计全部提交完成 |
| 检索（三路 + RRF，top20） | P95 ≤ 500ms | 固定 50 query 集，不含 rerank |
| 索引后查询新鲜度 | ≤ 5s（debounce）+ 处理时间 | 事件时间戳差 |

基准仓库：`bench/repo-500`（500 文件 / 5 万行，Go 60% / TS 30% / Python 10%），随代码提交基线快照与压测脚本。

## 4.9 RRF 混合检索

### 4.9.1 融合公式与实现

```
score(d) = Σ_r  w_r / (k + rank_r(d))
k = 60（默认，可配）
w_r = 路径权重（默认全 1.0；图扩展路可配 0.8）
```

- 去重键：`chunk_id`（代码）/ `topic_key`（记忆）/ `chunk_id`（知识）。
- 稳定排序：`score desc → 命中路数 desc → 文件路径 asc → start_line asc`（代码），保证 10 次查询顺序一致（REQ-083）。
- 复杂度：`sort.SliceStable` O(n log n)，替换现有 O(n²)（`knowledge_service.go:274-281`）。
- 通用实现放 `pkg/rrf`，记忆/代码/知识三处复用，避免三份公式漂移。

### 4.9.2 多路召回（REQ-082）

| 路 | 数据源 | 召回量 | 启用条件 |
|---|---|---|---|
| 向量 | `code_chunks.embedding`（pgvector） | top_k*2 | 有 embedding 且查询可嵌入 |
| 关键词 | `code_chunks.content_tsvector`（GIN） | top_k*2 | 总是 |
| 图扩展 | `code_edges` 1 跳（caller/callee） | 每个命中符号 ≤ 5 | 关键词/向量命中符号非空 |
| （可选）Qdrant | 仅当 `codeindex.vector_store=qdrant` 的实验开关 | top_k | 默认关闭（避免与 D2 冲突） |

图扩展的实现：命中 chunk 的 `symbol` → 查 `code_edges` 中 `target_symbol=symbol`（调用方）与 `source_symbol=symbol`（被调用方）→ 取对应 chunk（按 `source_file+start_line` 匹配）。

### 4.9.3 隔离（REQ-086）

所有查询强制带 `tenant_id + project_id`；跨项目查询返回空切片（不是 403，避免泄露存在性，REQ-086 验收条件）。SQL 层用参数化 + 行级过滤；禁止"先查再过滤"。

## 4.10 知识广场（复用 knowledge-service）

### 4.10.1 上传、解析、切分与元数据（REQ-087/088）

现状：`knowledge_service.Upload` 同步解析（`internal/parser/parser.go`）→ `chunker` 切分（512/50）→ 批量 embedding → Mongo + Qdrant，`Status` 固定 `"ready"`（`document_repository.go:128`）。

增强后的数据模型（逻辑）：

```
KnowledgeDocument
  document_id, tenant_id, kb_id, filename, source_type, size, content_hash
  status: pending|processing|ready|failed
  failure_reason, retry_count
  metadata { title, source, tags, split_strategy, overlap, page_count }
  created_at, updated_at

KnowledgeChunk
  chunk_id, document_id, kb_id, chunk_index(0..n, 连续)
  page_no?, section?, content, content_hash
  metadata { split_strategy, overlap, lang }
```

- 格式校验：PDF/DOCX/MD/TXT/HTML 允许；其余 `E_UNSUPPORTED_FORMAT`；0 字节 `E_EMPTY_DOCUMENT`；大小上限（默认 50MB，可配）。
- PDF 页码：解析器需按页切分并保留 `page_no`；`parser` 现有实现若丢失页码需扩展（`parser.go` 增加 `ParseWithPages`）。
- 重复上传：`content_hash` 相同且同 `tenant+kb` → 直接返回既有 document_id（幂等），不重复建索引。

### 4.10.2 索引状态机（REQ-089）

```
pending --(worker pick)--> processing --(success)--> ready
                              |                          |
                              +--(error, retry<n)-------+
                              +--(error, retry>=n)--> failed --(manual retry)--> pending
```

| 状态 | 可检索 | 可见 | 可重试 |
|---|---|---|---|
| pending | 否 | 是 | — |
| processing | 否 | 是（进度） | — |
| ready | 是 | 是 | — |
| failed | 否 | 是（含原因） | 是 |

- 上传 RPC 返回 `status=pending`（D11）；兼容策略：`document.status` 字段已有，旧调用方只判断"非空/存在"，不会破坏；若调用方断言 `ready`，需在同一请求内同步等待（配置 `knowledge.upload_mode=sync|async`，默认 async，灰度期 sync）。
- 后台 worker：`internal/indexing/worker.go`，并发 2，失败重试 3 次（1m/5m/15m）。
- `failed` 文档绝不进入检索（查询强制 `status='ready'`）。

### 4.10.3 混合召回与 RRF（REQ-090）

修复三处现状问题：
1. `bm25` 分支返回 `search.BM25Result.Score`，不再用 `topK-i` 伪造（`knowledge_service.go:174-181`）。
2. BM25 索引增量维护：新增 chunk → `bm25.AddDocument`；删除 → `bm25.RemoveDocument`（新增方法）；`RefreshBM25Index` 仅用于启动/修复。索引版本号 `bm25_version` 写入 Redis，多副本时各自构建（不共享内存）。
3. RRF 用 `pkg/rrf`（稳定排序），权重可配（向量 1.0 / BM25 1.0）。

### 4.10.4 Reranker（REQ-091）

```
type Reranker interface {
    ID() string
    Rerank(ctx, query string, candidates []Candidate, topM int) ([]Candidate, error)
}
```

| 实现 | 说明 | 成本 | 默认 |
|---|---|---|---|
| `LLMReranker` | 一次调用给 top-N 打分（0-10），严格 JSON 输出，超时 3s | 1 次 LLM 调用（~1-2k token） | 启用（`knowledge.rerank.enabled=true`） |
| `HTTPReranker` | 外部 cross-encoder（如 bge-reranker）HTTP 服务 | 无 token，需额外部署 | 关闭（可选） |
| `NoopReranker` | 直接返回融合顺序 | 0 | 降级 |

降级规则：reranker 超时/报错 → 返回融合顺序 + `rerank=degraded` 标记（REQ-091 验收条件）；`top 20 → top 5`。

### 4.10.5 来源引用（REQ-092）

```json
{
  "chunk_id": "c-123",
  "document_id": "d-9",
  "document_name": "架构设计.pdf",
  "page_no": 7,
  "section": "3.2 数据层",
  "score": 0.78,
  "rerank_score": 8.4,
  "snippet": "…",
  "uri": "/api/v2/knowledge/documents/d-9?page=7"
}
```

proto 扩展：`SearchResult` 增加 `sources`（repeated `SourceRef`），保留旧字段（DFX-C-01/C-04）。

### 4.10.6 更新与删除一致性（REQ-093）

| 操作 | 事务/顺序 | 失败处理 |
|---|---|---|
| 删除文档 | 标记 `status=deleting` → 删 Qdrant points → 删 Mongo chunks/document → 删 BM25 文档 → 硬删 | 任一步失败重试；`deleting` 状态不参与检索 |
| 更新文档 | 新版本写入临时 `version` → 全量索引成功 → 切换 `active_version` → 删旧版本 | 切换失败保留旧版本（不出现半新半旧） |

现状缺陷：删除只删 Mongo + Qdrant（`document_repository.go:162-174`），BM25 内存索引不失效 → 会返回陈旧结果。修复：删除路径必须调 `bm25.RemoveDocument` 并 bump 版本。

### 4.10.7 私有知识库隔离（REQ-094）

- `Document`/`Chunk` 增加 `kb_id`（必填，默认 `"default"` 兼容存量）。
- 检索必须带 `kb_id`（未带则查该 tenant 下全部授权 kb）；未授权 → 返回空结果 + `E_KB_FORBIDDEN` 仅在显式指定越权 kb 时返回（避免泄露标题）。
- 授权模型：复用 tenant 上下文 + `kb_acl`（tenant 内 kb 列表）；第一期只做 tenant 级隔离 + kb 级过滤，不做细粒度 ACL（范围外）。

### 4.10.8 性能（DFX-P-04）

- 混合召回 P95 ≤ 800ms：向量（Qdrant）≤ 200ms + BM25（内存）≤ 50ms + RRF ≤ 10ms + 序列化 ≤ 50ms，留余量。
- rerank P95 ≤ 2.5s：LLM rerank 超时 3s，超时即降级（不拖垮整体）。
- BM25 启动构建：>10 万 chunk 时改为后台构建 + 就绪前降级为 Mongo 文本检索（`searchBM25Fallback` 已有）。

## 4.11 CLI 与 runtime

### 4.11.1 命令集（REQ-097）

| 命令 | 作用 | 关键参数 | 输出 |
|---|---|---|---|
| `harness-cli init` | 初始化 `.harness/`、生成 global/project md、写默认配置、可选加入 .gitignore | `--project`, `--force`, `--tenant` | 文本/JSON |
| `run` | 提交任务并进入 TUI | `--goal/--goal-file`, `--workspace`, `--criteria`, `--budget`, `--no-tui`, `--json` | TUI / JSON |
| `resume` | 从检查点恢复 | `--task`, `--checkpoint`, `--json` | TUI / JSON |
| `status` | 查询任务状态（不阻塞执行） | `--task`, `--watch`, `--json` | 表 / JSON |
| `logs` | 查看事件/日志 | `--task`, `--stage`, `--since`, `--follow`, `--json` | 流 / JSONL |
| `abort` | 中止任务（幂等） | `--task`, `--reason` | JSON |
| `memory` | 查看/编辑/删除/检索记忆 | `ls/show/set/rm/search/--decayed` | 表 / md 片段 |
| `index` | 代码索引 | `--full`, `--paths`, `--status`, `--rebuild`, `--json` | 表 / JSON |
| `doctor` | 环境自检（PG/Mongo/LLM/工作区/权限/cgo） | `--json` | 检查清单 |
| `version` | 版本 | — | 文本 |

每个命令必须实现 `--help`（cobra 自动）；`run/resume/abort/status` 构成闭环。

### 4.11.2 cobra 结构

```
internal/cli/cmd/root.go        # 全局 flags: --server, --tenant, --config, --json, --no-color
            /init.go /run.go /resume.go /status.go /logs.go /abort.go
            /memory.go /index.go /doctor.go
internal/cli/tui/app.go         # bubbletea Program
             /model.go          # 顶层 Model（状态 + 订阅）
             /views/stage.go    # 阶段/尝试/验收结论
             /views/stream.go   # Agent 输出流
             /views/events.go   # 最近事件
             /views/help.go
internal/cli/output/json.go     # 统一 JSON envelope
             /exitcode.go       # 0 成功 / 2 失败 / 3 中止 / 4 参数错误 / 5 连接失败
```

配置优先级（REQ-101）：`命令行 flag > 环境变量（HARNESS_*）> ~/.harness/config.yaml > 内置默认`。API Key 只从环境变量或系统 keyring 读取，**不写入任何仓库文件**；`--json` 与日志统一走脱敏器（正则替换 `sk-[A-Za-z0-9]{8,}`、`Bearer xxx`、`password=`）。

### 4.11.3 TUI 视图（REQ-098）

```
┌ Harness Loop ─ task 7f3a ─ running ─────────────────────────────┐
│ Stage  c 执行编码   attempt #2   elapsed 04:12   tokens 38.2k    │
│ Acceptance  b:passed (program)   c:pending                        │
│ Retry  network=1/3  no_finish=0/2   Budget  tokens 38.2k/120k    │
├ Agent Output ────────────────────────────────────────────────────┤
│ [c] editing services/foo/handler.go …                            │
│ [tool] go test ./… → FAIL (2 tests)                              │
├ Events ──────────────────────────────────────────────────────────┤
│ 10:02:11 stage.entered c#2                                        │
│ 10:02:40 error(network) retry 1/3  backoff 1s                     │
│ 10:03:02 acceptance.failed → rollback c                           │
└ q quit  a abort  l logs  m memory  ? help ───────────────────────┘
```

- 刷新：事件流驱动（gRPC server stream），UI 帧率限制 10fps，避免高频 token 刷屏（token 合并批处理，每 100ms 渲染一次）。
- 验收失败：展示 `evidence` 摘要（≤ 3 条）+ `failed_criteria` 列表。
- 断线：TUI 显示 `reconnecting…`，重连后按 `last_seq` 补拉（REQ-099）。

### 4.11.4 通信协议（REQ-099/102）

| 选项 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| gRPC server streaming | 强类型、复用 pb、支持 metadata（tenant/token）、易做双向控制 | 浏览器不友好 | **CLI 选用** |
| SSE | 浏览器原生、单向、断线重连简单 | 无双向、二进制/大 payload 弱 | **Web 选用**（Gateway 转发） |
| WebSocket | 双向、低延迟 | 需 Gateway upgrade、鉴权与代理复杂 | 备选（事件量再增大时） |

CLI 读写分离：`status/logs` 走独立 unary/stream 调用，不占任务执行 stream；runtime 侧对查询接口只读快照（不持任务锁），保证"查询不阻塞执行"（REQ-099 验收条件）。

### 4.11.5 非交互与退出码（REQ-100）

统一 JSON envelope：

```json
{ "ok": true, "task_id": "7f3a", "status": "completed", "terminal_reason": "completed",
  "stages": [{"stage":"a","attempts":1,"accepted":true}], "tokens": 120345, "cost": 0.42,
  "report_path": "/workspace/.harness/tasks/7f3a/report.md" }
```

退出码：`0` 成功；`2` failed；`3` aborted；`4` 参数/配置错误；`5` 连接/服务不可用。中止用独立码（REQ-100）。

### 4.11.6 离线能力

- 无 PG：记忆走文件降级（4.7.5），代码索引禁用（`index` 命令提示 `E_INDEX_UNAVAILABLE`），任务仍可执行（事件只落本地 JSONL + Mongo 检查点）。
- 无网络（LLM 不可达）：任务立即 `network` 异常重试 3 次后 failed，不无限挂起。
- 本地 runtime：`harnessloop-service --local --workspace <path>`，监听 `127.0.0.1:50008`，默认只允许本机连接。

## 4.12 监控大屏与事件协议

### 4.12.1 事件 schema（REQ-103）

```json
{
  "event_id": "01J9Z6K3N5Q8V2M4T7R1X0B6C8",
  "event_type": "acceptance.failed",
  "schema_version": "1.0.0",
  "task_id": "7f3a",
  "trace_id": "4b2c...",
  "span_id": "9d1e...",
  "stage": "c",
  "attempt": 2,
  "seq": 147,
  "timestamp": "2026-11-10T10:03:02.145Z",
  "severity": "warn",
  "tenant_id": "t-001",
  "project_id": "repo-a",
  "agent_id": "exec-agent-1",
  "payload": {
    "acceptance_id": "acc-88",
    "verdict": "fail",
    "failed_criteria": [{"criterion_id":"c-3","category":"impl_gap"}],
    "evidence_digest": "sha256:...",
    "rollback_target": "c"
  },
  "payload_ref": null,
  "duration_ms": 8420
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `event_id` | 是 | ULID，生成后不可变 |
| `event_type` | 是 | 必须来自字典（4.12.2） |
| `schema_version` | 是 | 语义化版本，当前 `1.0.0` |
| `task_id` | 是 | — |
| `trace_id` / `span_id` | 是/否 | 任务级事件 span 为空 |
| `stage` / `attempt` | 否/否 | 终态事件可为空 |
| `seq` | 是 | 任务内严格单调递增 |
| `timestamp` | 是 | 生产端时间，UTC |
| `severity` | 是 | debug/info/warn/error |
| `tenant_id` / `project_id` | 是 | 隔离与查询 |
| `agent_id` | 否 | 验收/执行 Agent 区分时必填 |
| `payload` | 是 | 不得含密钥、完整提示词、完整文件内容 |
| `payload_ref` | 否 | 大对象（如压缩 JSONL）用引用代替内嵌 |
| `parent_task_id` | 否 | 子任务/交接事件必填，等于父 `task_id`（2026-11-10 新增，REQ-139） |
| `subtask_id` | 否 | 子任务维度事件（含子任务内 `tool.*`）必填（2026-11-10 新增） |
| `handoff_id` | 否 | `handoff.*` 事件必填（2026-11-10 新增） |
| `dag_id` / `dag_node_id` | 否 | DAG 相关事件填写，便于还原拓扑（2026-11-10 新增） |

**生产端校验**：缺必填字段或 `payload` 超限（默认 8KB）→ 拒绝发送 + `event.invalid` 告警（REQ-103 验收条件）。

### 4.12.2 事件类型字典（REQ-104）

| 族 | 类型 | 关键 payload |
|---|---|---|
| task | `task.started` `task.completed` `task.aborted` `task.failed` `task.paused` `task.resumed` | goal_hash, terminal_reason, report_ref |
| stage | `stage.entered` `stage.exited` `stage.transition` `stage.illegal_transition` | from/to, entry_reason, attempt_no |
| acceptance | `acceptance.started` `acceptance.passed` `acceptance.failed` `acceptance.invalid` `acceptance.rollback_overridden` | verdict, verified_by, evidence_digest, duration_ms |
| retry | `retry.scheduled` `retry.exhausted` | category, retry_index, max_retries, backoff_ms |
| checkpoint | `checkpoint.saved` `checkpoint.restored` `checkpoint.drift` `checkpoint.corrupted` | checkpoint_id, kind, drift_fields |
| compaction | `compaction.triggered` `compaction.completed` `compaction.degraded` `compaction.rejected` | tokens_before/after, ratio, strategy, reason |
| memory | `memory.written` `memory.merged` `memory.promoted` `memory.evicted` `memory.read_promoted` `memory.capacity_warning` `memory.decayed` `memory.corrupted` `memory.resync` `memory.index_degraded` | file, scope, topic_key, action, content_hash, order_rank；**`evicted` / `read_promoted` 仅由 project 产生；`capacity_warning` 仅由 global 超软上限产生**（2026-11-11 修订） |
| index | `index.queued` `index.updated` `index.failed` `index.rebuilt` | file, hash_before/after, duration_ms, queue_depth |
| tool | `tool.started` `tool.result` `tool.progress` | tool_name, call_id, ok, duration_ms |
| error | `error` | category, retryable, retry_index, original_finish_reason |
| security | `security.violation` | action, target, policy, decision |
| state | `state.rebuild` `stage.illegal_transition` | reason, diff |
| subtask（2026-11-10 新增） | `subtask.plan_created` `subtask.queued` `subtask.started` `subtask.handoff_waiting` `subtask.accepted` `subtask.rejected` `subtask.failed` `subtask.cancelled` `subtask.circuit_open` `subtask.circuit_reset` | subtask_id, parent_task_id, dag_id, dag_node_id, role, status, attempt_no, category, queue_position |
| handoff（新增） | `handoff.created` `handoff.accepted` `handoff.rejected` `handoff.expired` | handoff_id, from_agent_id, to_agent_id, a2a_task_id, reject_reason |
| aggregation（新增） | `aggregation.completed` `aggregation.conflict` `aggregation.missing` | task_id, dag_id, items, conflicts, accepted/failed/cancelled 计数 |
| model（新增） | `model.selected` `model.switched` `model.fallback` `model.capability_rejected` `model.unavailable` `model.recovered` `model.registry_changed` | stage, model_id, from, to, strategy, reason, candidates, violations |
| tool 扩展（新增） | `tool.allowed` `tool.denied` | tool_name, call_id, decision, rule_id, layer, stage |
| policy（新增） | `policy.changed` `policy.denied_summary` | rule_id, layer, changed_keys, operator |

未登记类型禁止发送（编译期常量 + 运行时白名单双重校验）。

### 4.12.3 顺序与幂等（REQ-105）

- `seq`：由 runtime 的 `SeqAllocator` 按任务分配，内存自增 + 持久化水位（每 100 条或每次转移持久化一次）。恢复时从 `max(seq)+1` 继续；若发现水位落后（事件已写入但水位未更新），扫描该任务最大 seq 后校准。
- `event_id`：ULID（时间有序），保证跨进程唯一；消费端按 `event_id` 去重（PG 主键 + `ON CONFLICT DO NOTHING`）。
- 消费幂等：大屏状态由"按 seq 归约"得到（`reduce(state, event)`），重复事件不改变归约结果；聚合计数用 `DISTINCT event_id`。

### 4.12.4 持久化与查询（REQ-106）

```sql
CREATE SCHEMA IF NOT EXISTS hlp_event;

CREATE TABLE hlp_event.events (
  event_id       text PRIMARY KEY,
  task_id        text NOT NULL,
  tenant_id      text NOT NULL,
  project_id     text NOT NULL,
  seq            bigint NOT NULL,
  event_type     text NOT NULL,
  schema_version text NOT NULL,
  stage          text,
  attempt        integer,
  severity       text NOT NULL,
  trace_id       text,
  span_id        text,
  ts             timestamptz NOT NULL,
  payload        jsonb NOT NULL,
  payload_ref    text,
  UNIQUE (task_id, seq)
);

CREATE INDEX idx_events_task_seq  ON hlp_event.events (task_id, seq);
CREATE INDEX idx_events_tenant_ts ON hlp_event.events (tenant_id, ts DESC);
CREATE INDEX idx_events_type_ts   ON hlp_event.events (tenant_id, event_type, ts DESC);
CREATE INDEX idx_events_trace     ON hlp_event.events (trace_id);
CREATE INDEX idx_events_payload   ON hlp_event.events USING GIN (payload jsonb_path_ops);
```

- 保留期：默认 30 天（可配），按 `ts` 分区（按月）或定时删除（单表 + 索引足够支撑第一阶段量级）。
- 查询 API：`GET /harness-loop/tasks/:id/events?stage=&type=&severity=&since_seq=&limit=`，按 `seq` 排序分页。

### 4.12.5 大屏视图（REQ-107/108/110/111）

| 视图 | 数据源 | 交互 |
|---|---|---|
| 生命周期泳道 | `stage.*` + `acceptance.*`（按任务聚合） | 横向 = a→b→c→d→e，色块 = attempt，红点 = 回退 |
| Agent 卡片网格 | 任务快照（当前阶段/attempt/最近 verdict/健康度/耗时） | 点击 → 右侧抽屉 |
| 健康度仪表 | 健康度算法（4.12.7） | 排序、过滤 |
| 终端输出流 | `tool.*` + token 流（仅展示摘要，完整流走 `logs`） | 虚拟滚动，最多 500 行 |
| 任务产物 | `artifact` 快照（路径/哈希/大小） | 点击复制路径、下载报告 |
| 异常定位抽屉 | 按 `trace_id` 聚合 error/日志/验收记录/最近检查点 | 一跳跳转，无需手工拼 ID（REQ-111） |
| 聚合统计 | 按状态计数（运行中/等待验收/重试中/失败/完成） | 顶部数字 + 趋势 |

**视觉约束（用户偏好）**：克制科技风，深灰底 + 单一强调色（青绿），禁用彩虹渐变与镭射；ECharts 统一主题从 `frontend/src/styles/` 的 token 派生；卡片只保留必要信息，文案避免"AI 味"（如"生命体征"改为"健康度"、"智能调度"改为"阶段推进"）。

### 4.12.6 实时推送与补拉（REQ-109）

```
GET /api/v2/harness-loop/monitor/stream?tenant=…&last_seq=…
Accept: text/event-stream

event: harness
id: 01J9Z6...
data: {"task_id":"7f3a","seq":148,...}

event: heartbeat
data: {"ts":"..."}
```

- Gateway 从 harnessloop-service 的 gRPC `WatchEvents(tenant, since_seq)` 读取，转 SSE。
- 断线重连：客户端带 `Last-Event-ID` 或 `last_seq`，服务端从 `since_seq+1` 补发；补发完成后发 `event: synced`。
- 缺口检测：客户端比对本地最大 seq，若服务端返回的 seq 跳号 → 主动 `GET /events?since_seq=` 补拉（5s 内完成，REQ-109/DFX-P-07）。
- 心跳每 15s，防代理超时。

### 4.12.7 健康度算法（REQ-110）

```
health = 100 * (0.35 * acceptance_pass_rate
              + 0.25 * (1 - retry_rate)
              + 0.20 * stage_freshness
              + 0.20 * (1 - error_density))

acceptance_pass_rate = passed / max(1, total_acceptance)
retry_rate           = retries / max(1, attempts)
stage_freshness      = clamp(1 - current_stage_dwell / stage_budget, 0, 1)
error_density        = errors_in_window / max(1, events_in_window)
```

- 权重可配（`config/harness_loop/health.yaml`），变更审计并只影响新任务（REQ-110 验收条件）。
- 冷启动（无验收）时用 `0.5` 中性值填充 `acceptance_pass_rate`，并在 UI 标注"样本不足"。
- 阈值：`< 40` 红色、`40-70` 黄色、`> 70` 绿色。

### 4.12.8 启停控制与审计（REQ-112/114）

| 操作 | API | 鉴权 | 二次确认 | 回执事件 |
|---|---|---|---|---|
| 启动 | `POST /tasks` | tenant + token | 否 | `task.started` |
| 暂停 | `POST /tasks/:id/pause` | tenant + token | 是 | `task.paused` |
| 恢复 | `POST /tasks/:id/resume` | 同上 | 否 | `task.resumed` |
| 中止 | `POST /tasks/:id/abort` | 同上 | 是 | `task.aborted` |
| 重建索引 | `POST /index/rebuild` | 同上 + 管理员角色 | 是 | `index.rebuilt` |
| 删除记忆 | `DELETE /memory/:topic` | 同上 | 是 | `memory.evicted`（reason=user） |

审计写入 `hlp_event.events`（`event_type=audit.*` 或单独 `hlp_event.audit` 表）+ 复用 `harness-service/audit` 的 hook 导出；字段：操作人、租户、对象、时间、结果、来源 IP、user_agent。普通用户不可删除审计记录（DB 权限约束）。

### 4.12.9 大屏性能（REQ-113，DFX-P-08）

- 50 卡片：虚拟滚动（只渲染视口内 ≤ 12 张完整卡片），其余为占位骨架。
- 事件批处理：SSE 消息按 200ms 合并，一次 `setState`；ECharts 用 `notMerge=false` 增量 `setOption`，避免重建实例。
- 单帧预算 16ms：卡片用 `React.memo` + 不可变数据；健康度计算放 Worker（`health.worker.ts`），主线程只接收结果。
- 1s 刷新下前端不重渲染整页：只更新变化的卡片（按 `task_id` 分发）。

---

## 4.13 多 Agent 协作层（REQ-123~141，2026-11-10 新增）

> **定位**：本层解决"父任务在 b 阶段把工作拆成可并行的子任务、调度执行、交接、汇聚"的问题。它**不推翻 FSM，也不另立编排入口**——父任务仍是唯一的 a~e 状态机持有者（4.1 不变）；子任务在 **b 阶段被规划**、在 **c 阶段被并行执行**、在 **d 阶段被汇聚**，本身不持有 a~e。
>
> **四条不变式（设计与评审的硬约束）**：
> 1. **层级明确**：父任务走 a~e；子任务**不进入 a~e**，只在自己的 7 态状态机内流转（REQ-124）。
> 2. **预算单账户**：子任务共享父任务 `goal` 与预算池，**不建独立预算账户**（REQ-141）。
> 3. **复用不重建**：Agent 注册、技能、工具集、handoff 工具、审批、A2A 传输全部复用存量（REQ-140）。
> 4. **无验收不汇聚**：未 `accepted` 的子任务产物不得进入父任务产物集合（REQ-133/141）。

### 4.13.1 父子任务模型与子任务状态机

**父 / 子对照**：

| 维度 | 父任务 TaskRun | 子任务 Subtask |
|---|---|---|
| 状态机 | a→b→c→d→e（4.1.2 转移表，8 条合法边） | `pending / running / waiting_handoff / accepted / rejected / failed / cancelled`（7 态，**不含 a~e**） |
| 执行实例 | 1 个 FSM 实例，跨阶段唯一 | 每个子任务 1 个独立 FSM 实例 |
| 尝试序号 | `attempt_no_by_stage{stage: n}` | 子任务内 `attempt_no` 从 1 递增（独立计数，不与父共享） |
| 检查点 | `harness_loop_checkpoints`（parent 视图） | 同 collection，文档带 `subtask_id`，可独立 restore |
| goal | 持有不可变副本 `goal_hash` | **只读引用父 goal**，不复制副本（避免漂移） |
| 预算 | 持有 `budget_tokens` / `budget_cost` | 只持有 `alloc_tokens`（父分配的额度视图，可回收）；消耗回写父 `consumed_*` |
| 验收 | 阶段验收 Agent（4.2） | 子任务级验收（复用同一验收器，输入收窄到子任务契约），通过才可 `accepted` |
| 终态 | completed / aborted / failed | accepted / rejected / failed / cancelled（均回写父汇聚视图） |

**子任务状态机**（`pkg/harnessloop/subtask/state.go`）：

```
pending ──(调度器选中·配额可用)──> running
running ──(产出需移交)──────────> waiting_handoff ──(接收方接手)──> running
running ──(自验收 pass)─────────> accepted
running ──(自验收 fail·可重试)──> running           (attempt_no+1)
running ──(自验收 fail·耗尽)────> rejected
running ──(异常超时/依赖失败)───> failed
pending/running/waiting_handoff ─(父回退或 fail-fast)─> cancelled
accepted / rejected / failed / cancelled = 终态（无出边）
```

**"running 内部两步"不新增状态**：子任务的"执行 → 自验收"是 `running` 内部的两个 step（`step=execute` / `step=self_verify`），写入检查点但不扩展状态枚举——这是为满足 REQ-124"仅 7 值"的刻意设计，避免出现"子任务也有 a~e"的误读。

**数据模型（逻辑层；DDL 见 5.1.2）**：

```
Subtask
  subtask_id            # 全局唯一，ULID
  parent_task_id        # 必填，指向 task_runs.task_id
  dag_id                # 本次拆分的 DAG 标识（同一父任务可因回退重新规划产生新 dag_id）
  dag_node_id           # DAG 内节点键（计划声明的 key，父任务内唯一）
  depth                 # 拆分深度，父的直接子任务=1；上限 2
  index_in_parent       # 在本次 DAG 中的序号（0-based，用于稳定排序与展示）
  parallel_set_id       # 所属并行集标识
  role                  # planner/coder/verifier/...（必须取自已注册角色）
  status                # 7 态之一
  depends_on[]          # 同父任务内 dag_node_id 列表（不得成环、不得跨父）
  input_contract        # 输入契约（结构化：类型 + 必填字段 + 产物引用）
  output_contract       # 输出契约（产物路径 glob + 校验方式）
  acceptance_criteria   # 子任务级验收标准（收窄自父阶段标准）
  failure_strategy      # 继承并行集：fail-fast|tolerate|retry
  attempt_no            # 从 1
  artifact_refs[]       # {path, hash, size}
  handoff_id            # waiting_handoff 时必填
  alloc_tokens          # 父分配额度视图
  consumed_tokens/cost  # 计入父预算池
  tenant_id/project_id  # 必须与父任务一致（REQ-137）
```

**为什么 `depth` 与 `dag_id` 都要有**：`dag_id` 支持"父任务回退到 b 后重新规划"时不覆盖历史 DAG（旧 DAG 的子任务保留审计）；`depth` 用于执行 REQ-127 的 2 层上限校验，且可在查询时直接过滤"第 3 层"。

**检查点文档的字段扩展（2026-11-10 新增，兼容 4.4.2）**：子任务复用同一 MongoDB collection `harness_loop_checkpoints`，`CheckpointDoc` 追加三个**可选**字段 `subtask_id` / `dag_node_id` / `depth`（缺省视为父任务检查点）。理由：① 子任务的阶段语义与父不同，但快照结构（messages/plan/artifacts/consumed/watermark）完全同构，另建 collection 会造成"两套恢复逻辑"；② 可选字段满足 DFX-C-04 向前兼容（旧文档可被新代码解析，`subtask_id` 为空即父任务）；③ 恢复时按 `task_id + subtask_id` 组合定位，父任务恢复不会误加载子任务快照。

### 4.13.2 b 阶段产出子任务 DAG（REQ-125~127）

b 阶段 finish 后，验收 Agent 除了校验"分步计划"，还**必须**校验子任务 DAG 的完整性；DAG 缺失或不合规即 b 阶段验收 fail（`failed_criteria` 指出缺失项）。

**计划产物 schema（b 阶段必要输出，`dag.go` 解析）**：

```json
{
  "$id": "SubtaskPlan/v1",
  "required": ["dag_id", "nodes", "edges", "parallel_sets"],
  "properties": {
    "dag_id": {"type": "string"},
    "nodes": [{
      "node_id": "n1",
      "role": "coder",
      "input_contract": {"type": "file_set", "paths": ["src/a/**"], "requires": ["n0"]},
      "output_contract": {"type": "artifacts", "paths": ["src/a/**"], "verify": ["go build ./src/a/..."]},
      "acceptance": [{"type": "build_pass", "expr": "go build ./src/a/..."}],
      "splittable": true
    }],
    "edges": [{"from": "n0", "to": "n1"}],
    "parallel_sets": [{"set_id": "p1", "nodes": ["n1", "n2"], "failure_strategy": "tolerate", "tolerate_ratio": 0.2}]
  }
}
```

**校验规则表（任一不通过 → b 验收 fail）**：

| # | 规则 | 不通过错误码 | 依据 |
|---|---|---|---|
| 1 | 节点数 ≥ 1；节点数 ≤ 20（`subtask.max_direct_children`） | `E_SUBTASK_LIMIT_EXCEEDED` | REQ-127 |
| 2 | 节点 > 1 时 `edges` 非空（必须有依赖或显式并行集声明） | `E_DAG_INCOMPLETE` | REQ-125 |
| 3 | 依赖边无环（拓扑排序可完成）；`from/to` 必须引用存在的 `node_id` | `E_DAG_CYCLE` | REQ-125 |
| 4 | 每个 `parallel_set` 内节点两两之间**不存在路径依赖**（有依赖不得同集） | `E_DAG_PARALLEL_CONFLICT` | REQ-125/128 |
| 5 | 每个节点 `input_contract` / `output_contract` 非空，且 output 的 paths 在 workspace 沙箱内（4.15.3 预校验） | `E_CONTRACT_INCOMPLETE` | REQ-125 |
| 6 | 不可拆单元不得出现在节点内（见下表） | `E_SUBTASK_NOT_SPLITTABLE` | REQ-126 |
| 7 | 所有节点 depth=1；若节点声明 `children`，其 depth=2 且不得再有 `children` | `E_SUBTASK_DEPTH_EXCEEDED` | REQ-127 |
| 8 | 每个 `parallel_set` 必须声明 `failure_strategy` | `E_PROPAGATION_UNDECLARED` | REQ-134 |

**不可拆条件（REQ-126，四类，计划期即拒绝）**：

| 类型 | 判定 | 理由 |
|---|---|---|
| 单文件原子修改 | 一个节点内 output paths 只指向 1 个文件且改动语义不可分割 | 拆分会产生同一文件的并发写冲突 |
| 单一确定性验证项 | 节点 acceptance 只有 1 条 `tests_pass`/`build_pass` | 验证项本身不可并行（依赖同一构建产物） |
| 含跨文件一致性约束 | input/output 契约声明 `consistency_group`（如同一接口的声明与实现） | 拆分后两半可能各自"验收通过"但整体不一致 |
| 含外部审批点 | 节点工具集含 `requires_approval=true` 的工具 | 审批是串行人工环节，拆分只会放大等待 |

**可并行集的来源**：以计划声明为准，但 runtime 用 DAG 复核（规则 4）。同时按拓扑层给出 `suggested_levels` 供前端展示（`level = 1 + max(level(dep))`），不改变调度依据。

**"数量上限 / 深度上限"是双闸门**：计划期校验（上面 7/8 条）+ 调度期兜底（`scheduler` 入队前再查一次，防止绕过计划直接调 `CreateSubtask` 的内部接口）。

### 4.13.3 调度器（REQ-128~130）

**结构**（`orchestrator/scheduler.go`）：每个父任务一个 `ParentScheduler`（内存态，可从 PG 重建），全局一个 `TenantGate`（跨父任务的租户级并发闸门）。

**并发与配额（DFX-P-11，REQ-128）**：

| 维度 | 默认 | 配置项 | 超限行为 |
|---|---|---|---|
| 单父任务并行子任务 | 8 | `subtask.max_parallel_per_parent` | 其余 `pending`，发 `subtask.queued(position=N)` |
| 单租户并发 Agent | 20 | `subtask.max_agents_per_tenant` | 排队（**不失败**，REQ-128 明确） |
| 单父任务直接子任务总数 | 20 | `subtask.max_direct_children` | 计划期拒绝（4.13.2 规则 1） |
| 拆分深度 | 2 | `subtask.max_depth` | 计划期拒绝 |
| 单子任务超时 | `min(父阶段剩余时间预算, 15min)` | `subtask.timeout_minutes` | 终止 → `failed(category=timeout)` + 按传播策略处理 |

**双配额**：每个父任务持有 `{agent_slots, token_budget}` 两个令牌桶；子任务启动需同时获取 1 个 agent slot + 预估 token（`min(输出契约预估, 剩余额度/2)`）。任一不足 → 排队，**不允许只拿一个**（否则会出现"占着 slot 等 token"的饥饿）。预测超估导致中途额度不足 → 子任务 `failed(category=quota)`，并按传播策略处理。

**同类熔断（REQ-130）**：

```
class_key = sha1(role + "|" + sorted(output_contract.paths) + "|" + primary_acceptance_type)
样本窗口 = 该 class_key 已进入终态的子任务
当 samples >= 4 且 failed/samples >= 0.5（subtask.circuit.failure_ratio）
  → circuit_open(class_key)，后续同 class 不再调度
  → 已 running 的同类子任务**不打断**（避免二次浪费）
  → 发 subtask.circuit_open，父任务可选择回退 b 或终止
恢复：仅在父任务重新规划（新 dag_id）或人工 `resume --reset-circuit` 时复位
```

**调度延迟**：从"依赖满足且配额可用"到"子任务进入 running" P95 ≤ 300ms。实现约束：调度循环用 50ms tick + 事件唤醒（`subtask.*` 状态变更即唤醒），不做全量轮询扫描。

### 4.13.4 handoff 契约（REQ-131/132）

**什么时候发生 handoff**：子任务 `running` 中发现"本角色能力/工具集不足以完成剩余部分"，或计划显式声明"由 verifier 接手 coder 的产物"。此时不得"自己越权做完"，而是生成 handoff。

**契约 schema（`handoff/contract.go`，落 PG 见 5.1.3）**：

```json
{
  "$id": "HandoffContract/v1",
  "required": ["handoff_id","from","to","artifacts","context_summary","unfinished","constraints"],
  "properties": {
    "handoff_id": {"type": "string"},
    "parent_task_id": {"type": "string"},
    "subtask_id": {"type": "string"},
    "from": {"type": "object", "required": ["agent_id","role","subtask_id"]},
    "to":   {"type": "object", "required": ["agent_id","role"]},
    "artifacts": [{"path": "src/a/x.go", "hash": "sha256:...", "size": 1234}],
    "context_summary": {
      "goal_ref": {"type": "string"},
      "done": {"type": "string"},
      "todo": {"type": "string"},
      "constraints": {"type": "string"},
      "background": {"type": "string"},
      "source_refs": {"type": "array"}
    },
    "unfinished": [{"item": "补 x_test.go 的边界用例", "reason": "无 write 权限", "evidence": ["file:src/a/x.go"]}],
    "constraints": {
      "role": "verifier", "tools": ["read_file","grep"],
      "workspace_subpaths": ["src/a/"], "token_quota": 12000,
      "acceptance": [{"type": "test_pass","expr":"go test ./src/a/..."}]
    },
    "created_at": "2026-11-10T10:00:00Z"
  }
}
```

- **`context_summary` 必须是五要素齐备**（复用 4.5.3 的字段定义与校验函数），缺项 → 拒绝交接并返回 `E_HANDOFF_INCOMPLETE`（REQ-131 验收条件）。
- **`unfinished` 不得为空**：没有未完成项就不应 handoff，应直接 `accepted`。空数组 → `E_HANDOFF_INCOMPLETE`。

**交接时序**：

```mermaid
sequenceDiagram
  participant A as 子任务A（coder）
  participant RT as runtime(orchestrator)
  participant HV as handoff 校验器
  participant B as 子任务B（verifier）
  participant A2A as a2a-service

  A->>RT: 请求 handoff（候选 to.agent_id）
  RT->>HV: 校验契约（五要素/未完成项/路径沙箱/同租户）
  HV-->>RT: 通过 or E_HANDOFF_*
  RT->>RT: 复用 agent.Registry.ValidateHandoff(from,to) + GetAgentTools(to)
  RT->>RT: 冻结 B 的上下文快照（只含契约 + runtime 注入项）
  RT->>A2A: SendTask(A2AMessage.metadata={handoff_id,parent_task_id})
  A2A-->>B: 任务投递（传输层复用，不自建通道）
  RT->>B: 注入执行上下文（不注入 A 的原始消息序列）
  A->>RT: status=waiting_handoff
  B->>RT: 接手 → 新 subtask（或原 subtask 续 running）
  B->>RT: 完成 → 子任务级验收 → accepted
```

**禁止隐式共享（REQ-132）**：

| 禁止项 | 拦截实现 |
|---|---|
| 共享完整对话历史 | `B` 的 `ExecutionContext.messages` 由 `handoff/transfer.go` 从零构造，**只允许**写入契约字段与 runtime 显式注入项；构造后记录 `input_digest` 与"消息来源白名单" |
| 读取 A 的执行侧 memory 命名空间 | B 使用独立命名空间 `handoff/<handoff_id>/`，读 A 的命名空间 → `security.violation` |
| 全量工作区快照 | 不打包工作区；B 只通过沙箱按 `constraints.workspace_subpaths` 访问 |
| 未裁剪检查点 | 不得把 A 的 checkpoint 文档直接交给 B；B 若有自己的恢复点，是 B 自己的 `subtask_id` 检查点 |

**复用边界**：传输走 `a2a-service` 的 `SendTask/GetTask`（`proto/a2a/a2a.proto:116-119`，`A2AMessage.metadata` 承载 `handoff_id`/`parent_task_id`）；Agent 合法性走 `pkg/agent/registry.go:301` 的 `ValidateHandoff`；handoff 工具形态复用 `pkg/agent/handoff.go` 的 `HandoffTool`/`HandoffResult`。**本组件不新增 Agent 注册表、不新增消息传输实现。**

### 4.13.5 d 阶段汇聚（REQ-133）

父任务进入 d 阶段，**第一件事是汇聚，而不是直接验收**：缺汇聚报告时 d 阶段验收不得启动（`E_AGGREGATION_MISSING`）。

**汇聚报告（`aggregate.go` 产出，作为 d 阶段验收的必填注入项）**：

```json
{
  "$id": "AggregationReport/v1",
  "required": ["task_id","dag_id","items","conflicts","summary"],
  "properties": {
    "items": [{
      "subtask_id": "st-1", "dag_node_id": "n1", "status": "accepted",
      "artifacts": [{"path":"src/a/x.go","hash":"sha256:...","exists":true,"hash_match":true}],
      "verdict": "pass", "consumed_tokens": 8123
    }],
    "conflicts": [{
      "kind": "same_path_multi_writer", "path": "src/b/y.go",
      "subtasks": ["st-2","st-3"], "resolution": "manual|last_writer|reject"
    }],
    "summary": {"total": 8, "accepted": 6, "rejected": 1, "failed": 1, "cancelled": 0}
  }
}
```

**逐项校验**（缺一即 `E_AGGREGATION_INCOMPLETE`）：
1. **存在性**：每个 `accepted` 子任务的 `artifact_refs` 文件必须存在。
2. **哈希一致**：磁盘内容哈希 = 子任务记录的 `hash`；不一致记 `artifact_drift`（不静默）。
3. **结论汇总**：`rejected/failed/cancelled` 必须出现在报告并标注原因，**不得被过滤成"只报成功项"**。
4. **未验收产物拦截**：只汇聚 `status=accepted` 的产物；尝试把 `running/rejected/failed` 的产物计入 → `security.violation`（REQ-141）。

**冲突检测（三类）**：

| 冲突 | 判定 | 处理 |
|---|---|---|
| `same_path_multi_writer` | 两个 accepted 子任务的 output paths 相交且都实际修改了同一文件 | 默认 `reject`：d 阶段验收 fail，`failed_criteria` 标 `impl_gap`，回退 `d→c`（或按规则表 `plan_gap` 回 b） |
| `symbol_multi_writer` | 不同文件但改动了同一导出符号（由代码索引 `code_edges` 反查） | 同上，但 evidence 附符号引用 |
| `contract_mismatch` | 上游 output 契约与下游 input 契约类型不匹配 | 视为计划缺陷，回退 b（`plan_gap`） |

冲突**不自动裁决**（不选"最后写入者赢"），因为自动裁决会让"验收通过但语义错误"流到 d 阶段之后。`config conflicts.auto_resolve=false` 为默认。

**父级验收如何消费子任务结论**：d 阶段验收输入的第 3 项注入从"实际产物清单"扩展为"产物清单 + 汇聚报告"（4.2.2 注入表第 3 项的增量），使验收 Agent 能看到"哪些子任务被拒、为什么"，而不是只看最终文件。汇聚报告不得替代父级验收结论——父仍然是唯一给出 `AcceptanceVerdict` 的主体。

### 4.13.6 失败传播（REQ-134）

| 策略 | 适用条件 | 行为 | 父任务终态判定 |
|---|---|---|---|
| `fail-fast` | 并行集内子任务**互为前置**（任一缺失则整体不可用），如"生成接口 + 生成客户端" | 任一子任务 `failed/rejected` → 立即 `cancelled` 该并行集其余子任务 → 父任务按回退映射流转（默认 `d→c`） | 父不因单点失败直接 failed；走回退，回退次数受 `acceptance.max_fails=5` 约束 |
| `tolerate` | 并行集内子任务**相互独立且可降级**，如"批量给 10 个模块补注释" | 允许失败比例 ≤ `tolerate_ratio`（默认 0.2，样本 ≥5 才生效）；失败项写入汇聚报告 `items[].status` | 容忍范围内 → 父继续 d；超比例 → 升级为 fail-fast 行为 |
| `retry` | 失败可能由瞬时因素导致（网络、限流） | 按子任务自身 `attempt_no` 上限（默认 2）重试；耗尽 → 降级为 `fail-fast` | 同 fail-fast |

**配置项（`policy/subtask.yaml`）**：
```
propagation:
  default: fail-fast
  tolerate_ratio_max: 0.5      # tolerate 不得比 50% 更宽松（超过即视为无意义）
  min_samples_for_ratio: 5
retry:
  max_attempts: 2
  backoff: [1s, 3s]
timeout:
  default_minutes: 15
```

**部分失败时的父终态**：严格遵守 4.1.4 的三类终态——子任务失败**不直接**把父置为 `failed`，而是"回退 → 重试 → 耗尽"后才 `failed`。父任务 `failed` 的记录必须包含 `failed_subtasks[]`（subtask_id + category），供终止报告与大屏追溯。

### 4.13.7 Agent 角色与能力（REQ-135/136）

**角色定义**（`subtask/registry.go` 内的角色契约，Agent 本体仍在 `agent-service`）：

| 角色 | 职责 | 可挂载工具集（默认白名单） | 可访问目录（相对 workspace_root） | 典型阶段 |
|---|---|---|---|---|
| `planner` | 需求理解、拆 DAG | `read_file, list_dir, grep` | 全仓库只读（排除 `.harness/policy`、密钥目录） | a / b |
| `coder` | 编码与重构 | `read_file, write_file, edit_file, list_dir, grep, run_command(白名单)` | `src/`, `docs/`, `test/`, `.harness/memory/` | c |
| `verifier` | 验收与测试 | `read_file, list_dir, grep, run_command(验收白名单)` | 全仓库只读 + 产物目录只读 | d |
| `handoff_receiver` | 承接交接的通用执行者 | 由 handoff `constraints.tools` 显式声明（**不得超出 to 角色白名单**） | 由 `constraints.workspace_subpaths` 收窄 | c / d |

- **角色是"上限"不是"授权"**：角色白名单是取值范围，实际生效 = 角色白名单 ∩ 任务级名单 ∩ 阶段级名单 − 黑名单（4.15.2）。REQ-136 的"角色未声明的工具即使存在于全局注册表也不得使用"即由"角色作为上限"实现。
- **未声明角色或工具集的 Agent 不得被计划引用**：`registry.go` 在计划解析阶段校验 `role` 存在且 `GetAgentTools(agent_id)` 非空，否则 `E_AGENT_ROLE_UNDECLARED`。
- **存量复用**：`pkg/agent/agent.go:28-38` 已有 `Tools []string`、`Handoffs []string`、`Skills []string`、`Model string`；`pkg/agent/registry.go:322` 已有 `GetAgentTools(agentID)`。本设计**不新增 Agent 结构**，只新增"角色 → 白名单模板"的映射与校验（`subtask/registry.go`），实际工具集仍从 agent-service 查询。
- **与 4.15 的联动**：角色白名单是权限解析链中的 `role` 层（system > tenant > **role** > task），见 4.15.7。

### 4.13.8 隔离与公平（REQ-137/138）

**隔离**：
1. **租户/项目绑定不可变**：`subtask.tenant_id/project_id` 必须与父任务完全一致，创建时校验，运行期不可改；跨租户创建/分配/handoff → `E_CROSS_TENANT_DENIED` + `security.violation`（REQ-137）。
2. **查询自动加隔离条件**：`ListSubtasks`、`GetDAGStatus`、`ListHandoffs` 强制 `tenant_id + project_id` 过滤；越权查询返回空而非 403（与 4.9.3 一致，避免泄露存在性）。
3. **handoff 不跨租户**：`ValidateHandoff` 之后追加租户一致性校验（存量 `ValidateHandoff` 只校验 Agent 可达性，不含租户维度——这是本设计必须补的一处）。

**公平（防单任务独占预算）**：采用 **WFQ（加权公平队列）** 而非 FIFO，理由：同租户多父任务并发时，FIFO 会让"先提交的大任务"长期吃到全部 20 个 Agent slot（REQ-138 明确禁止抢占）。

```
每个父任务 p 维护：
  weight[p]      = subtask.fairness.weight（默认 1.0，可由租户策略下调最低 0.5）
  virtual_time[p]= 已消耗的加权服务量（每次获得 1 个 slot 执行 1 个调度周期即 += 1/weight）

选择下一个可运行子任务：
  candidates = { 依赖满足 ∧ 配额可用 ∧ 未熔断 的子任务 }
  p* = argmin(virtual_time[p] for p in candidates 的父任务)
  在 p* 内按 (parallel_set_id, index_in_parent) 稳定排序取第一个
  virtual_time[p*] += 1 / weight[p*]

硬上限兜底（优先于 WFQ）：
  parent_used_slots[p] <= ceil(tenant_slots * parent_share[p])   # parent_share 默认 1/N，可配
  parent_used_tokens[p] <= parent_token_share[p]
```

**与双配额的关系**：WFQ 决定"下一个给谁"，双配额决定"能不能给"；两者串联，配额不足时该父任务跳过但不阻塞其他父任务（避免队头阻塞）。

**预算公平的可观测**：每个调度周期记录 `scheduler.alloc{parent_task_id, slots_used, tokens_alloc}`，大屏展示"租户内各父任务的 slot/预算占用条"，让"谁在吃配额"可见（REQ-138 验收条件）。

### 4.13.9 与 `a2a-service` / `agent-service` 的复用边界（REQ-140）

| 能力 | 现状（文件位置） | 本组件做什么 | 明确不做 |
|---|---|---|---|
| Agent 注册与查询 | `pkg/agent/registry.go`（`Register/Get/List/Exists`）、`services/agent-service`、`proto/agent/agent.proto:252-291` | 只读查询：按 `agent_id` 取 `Tools/Handoffs/Skills/Model` | **不自建 Agent 表、不做注册写入** |
| 角色工具集 | `pkg/agent/registry.go:322` `GetAgentTools` | 读取并与角色白名单求交 | 不复制工具清单到本地持久化 |
| 技能挂载 | `pkg/agent/skill.go`（`Skill`/`SkillStore`） | 计划引用技能时校验 `skill_id` 存在 | 不重建技能库 |
| handoff 运行时 | `pkg/agent/handoff.go`（`HandoffTool`/`ValidateHandoff`） | 在工具层 handoff 之上加**契约层**（五要素 + 未完成项 + 落库） | 不改 `HandoffTool` 语义 |
| Agent 可达性校验 | `pkg/agent/registry.go:301` `ValidateHandoff` | 直接调用 + 追加租户一致性 | 不重写可达性图 |
| A2A 传输 | `services/a2a-service`、`proto/a2a/a2a.proto`（`SendTask/GetTask/CancelTask`） | 用 `A2AMessage.metadata` 携带 `handoff_id`/`parent_task_id`/`subtask_id` | **不新建消息通道、不改 A2A 协议** |
| 工具审批 | `pkg/agent/approval/flow.go`（`ApprovalFlowManager`/`RuleEngine`） | 权限引擎命中 `requires_approval` 时复用其 `CreateRequest` + `WaitForApproval` | 不新建审批队列（见 4.15.6 的跨进程说明） |
| 工具注册与执行 | `services/mcp-service`（`ListTools`）、`pkg/agent/tools` | 扩展 `permission_tags`（4.15.1） | 不重写工具执行器 |
| 通用 DAG 编排 | `services/harness-service/internal/workflow` | 不使用；子任务 DAG 是"数据"不是"工作流引擎" | 不与 FSM 双编排（D12 结论延续） |

**一句话边界**：本组件是**编排者（orchestrator）**，不是**注册中心**也不是**传输层**。新增 Agent 或新通信通道时，本组件不需要改代码即可引用（REQ-140 验收条件）。

### 4.13.10 可观测：事件关联与大屏聚合（REQ-139）

**事件关联字段（4.12.1 schema 的增量，追加式）**：

| 字段 | 出现在 | 说明 |
|---|---|---|
| `parent_task_id` | 全部 `subtask.*`、`handoff.*` 事件 | 等于父 `task_id`（冗余但便于按父过滤，避免 join） |
| `subtask_id` | 全部 `subtask.*`、`handoff.*`、子任务内的 `tool.*` | 子任务维度 |
| `handoff_id` | 全部 `handoff.*` | 交接维度 |
| `dag_id` / `dag_node_id` | `subtask.plan_created`、`subtask.*` | 便于还原 DAG 拓扑 |
| `parallel_set_id` | `subtask.*` | 还原并行区间 |

**新增事件类型**（4.12.2 字典追加）：`subtask.plan_created`、`subtask.queued`、`subtask.started`、`subtask.handoff_waiting`、`subtask.accepted`、`subtask.rejected`、`subtask.failed`、`subtask.cancelled`、`subtask.circuit_open`、`handoff.created`、`handoff.accepted`、`handoff.rejected`、`aggregation.completed`、`aggregation.conflict`。

**大屏聚合视图（父子树 + DAG 进度）**：

```
父任务卡片（可展开）
├─ DAG 进度条：accepted/total（分段着色：accepted 青绿、running 蓝、failed 红、cancelled 灰）
├─ 并行区间条（甘特）：横轴时间，每个 parallel_set 一段，段内按 index_in_parent 分行
├─ 子任务树（缩进 2 层）：subtask_id · role · status · attempt · 耗时 · 最近 verdict
└─ handoff 链路：A →(handoff_id)→ B 的有向小图，点击可看契约摘要（不展示原始对话）
```

- 交互：点任一子任务 → 跳 `taskId` 详情页的"子任务"页签（同一 `parent_task_id`，路径 `/harness-loop/tasks/:id?subtask=`）。
- 性能：50 父任务 × 平均 8 子任务 = 400 节点，只渲染展开的父任务（默认折叠），折叠态只用汇总条（不建 DOM）。

### 4.13.11 禁止项拦截点（REQ-141）

| 禁止项 | 拦截点（代码位置） | 错误码/事件 |
|---|---|---|
| 未通过子任务级验收的产物汇入父产物集合 | `orchestrator/aggregate.go` 汇聚入口（只接受 `status=accepted`）+ `hlp_task.subtasks` 状态条件写（`WHERE status='accepted'`） | `E_SUBTASK_ACCEPTANCE_REQUIRED` + `security.violation` |
| 跨租户 / 跨项目 handoff | `handoff/validate.go`（租户/项目一致性）+ `a2a` 调用前 | `E_CROSS_TENANT_DENIED` + 审计 |
| 子任务数量 / 递归深度 / 并行集无限扩张 | 计划期 `orchestrator/dag.go` 校验（4.13.2 规则 1/7）+ 调度期 `scheduler.Enqueue` 二次兜底 | `E_SUBTASK_LIMIT_EXCEEDED` / `E_SUBTASK_DEPTH_EXCEEDED` |
| 子任务绕过父预算池独立计费 | 计量唯一写入点：`task_repo.AddConsumed(parent_task_id, tokens, cost)`；`Subtask` 表**不设** `budget_*` 列，只有 `alloc_tokens` 视图 | 写 `subtask_budget_account` 类记录 → 编译期不存在该 API；审计检出即为实现缺陷 |
| 子任务绕过验收直接置 `accepted` | `subtask/state.go` 转移表：`running→accepted` 必须附带 `acceptance_id` 且 verdict=pass | `E_SUBTASK_ACCEPTANCE_REQUIRED` |

---

## 4.14 模型选择（REQ-142~155，2026-11-10 新增）

> **定位**：把"用哪个模型"从散落在 `Agent.Model` / `pkg/config.LLM.Model` / 压缩模块硬编码窗口表的三处，收敛为**一个注册表 + 一条解析链**。模型选择结果**同时驱动**：预算估算、压缩触发阈值（4.5.1）、能力校验（是否需要 tool_call）、回退链（4.14.6）。
>
> **不变式**：注册表是窗口/单价/能力的**唯一事实源**；运行期默认**不允许**换模型；任何切换必须留下前后 model_id 与原因。

### 4.14.1 注册表结构与配置文件（REQ-142/143/153）

**存量锚点（不重建）**：`pkg/config/config.go:48-51` 已有 `LLM.Model`、`EmbeddingModel`、`Models map[string]string`；`pkg/agent/agent.go:38` 已有 `Agent.Model`。注册表是这三者的**上位结构**，不是替代品：`pkg/config` 的 `Model` 仍作为"全局默认"，注册表为其补充元数据。

**配置文件**：`config/harness_loop/models.yaml`（随二进制内嵌一份 seed，可被外部文件覆盖）。

```yaml
# 注册表 seed（示例，仅列关键字段）
version: 3
models:
  - id: qwen-max
    provider: dashscope
    context_window: 32768
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.02
    price_out_per_1k: 0.06
    status: available
    tags: [balanced, strong]
  - id: qwen-plus
    provider: dashscope
    context_window: 131072
    max_output_tokens: 8192
    tool_call: true
    structured_output: true
    price_in_per_1k: 0.0008
    price_out_per_1k: 0.002
    status: available
    tags: [long_context]
  - id: qwen-turbo
    provider: dashscope
    context_window: 8192
    max_output_tokens: 2048
    tool_call: true
    structured_output: false
    price_in_per_1k: 0.0003
    price_out_per_1k: 0.0006
    status: available
    tags: [cheap]
  - id: text-embedding-v3
    provider: dashscope
    kind: embedding
    embedding_dim: 1024           # 必须与 hlp_mem/hlp_code 的 vector(1024) 一致
    price_in_per_1k: 0.0007
    status: available

embedding:
  memory_model: text-embedding-v3
  code_model: text-embedding-v3
  enforce_dim: 1024

defaults:
  task_model: qwen-plus
  strategy: balanced
  fallback: [qwen-max, qwen-turbo]

stage_overrides:                   # 阶段级覆盖（4.14.3）
  d: qwen-max
```

**字段清单（与 spec 6.11 一一对应）**：`id / provider / kind(chat|embedding) / context_window / max_output_tokens / tool_call / structured_output / embedding_dim / price_in_per_1k / price_out_per_1k / status(available|unavailable|deprecated) / tags`。

**加载与热更新边界（REQ-153）**：

| 项 | 规则 |
|---|---|
| 加载时机 | 进程启动加载 seed → 若存在外部文件则覆盖；解析失败 → **保留旧快照并告警**（不因配置错误导致服务起不来） |
| 热更新 | 文件 mtime + `SIGHUP`/管理接口触发 reload；新快照原子替换（`atomic.Pointer`），**不阻塞进行中的调用** |
| 生效范围 | 只对 reload 之后**新发起**的模型调用生效；已发出的请求不中断、不改参数（REQ-153 验收条件） |
| 校验 | 加载时校验：`max_output_tokens ≤ context_window`、`kind=embedding` 时 `embedding_dim` 必填且 `= enforce_dim`、单价非负；任一不合规 → 整份快照拒绝（不部分生效） |
| 审计 | 每次 reload 写 `model.registry_changed`（操作人、变更项、结果；单价与密钥脱敏） |

**与 `pkg/config` 的收敛规则（避免两处定义）**：`pkg/config.LLM.Model` 视为"未配置注册表时的兜底默认"；一旦注册表加载成功，`ModelRegistry.Resolve()` 的优先级为：`阶段覆盖 > 任务显式指定 > 任务策略 > defaults.task_model > pkg/config.LLM.Model`。`pkg/config.LLM.Models`（旧 string map）不再用于窗口推断，仅保留给旧调用方读取，标记 deprecated（DFX-C-01 不破坏）。

### 4.14.2 任务级选择与打分（REQ-144）

**入口**：创建任务时 `CreateTaskRequest` 可带 `model_id`（显式）与 `model_strategy`（`cost_first|quality_first|balanced`，默认 `balanced`）。两者都写入 `task_runs`（`model_id`、`model_strategy`、`model_resolved_reason`）。

**候选集构建**：`status != unavailable` ∧ `kind=chat` ∧ 能力满足该任务"最高阶段需求"（若任务包含 c 阶段则必须 `tool_call=true`）。

**打分公式（可解释、可回归）**：三个子分数都归一到 `[0,1]`，公式固定写入 `selector.go` 并单测锁定：

```
cost_eff   = 1 - (price_in*0.7 + price_out*0.3) / max_blended_price_in_candidates   # 越便宜越高
             # 分母取候选集 blended 单价（price_in*0.7 + price_out*0.3）的最大值。
             # 早期写法误用 max_price_in_candidates：分子是 in/out 加权后的 blended 单价，
             # 输出单价通常远高于输入单价，按字面会得到 cost_eff < 0（如 qwen-max
             # in=0.02/out=0.06 → 0.032/0.02 = 1.6 → cost_eff = -0.6），无法归一到 [0,1]。
             # 实现以 `pkg/harnessloop/modelregistry/selector.go` 的 blendedPrice 为准。
quality    = 0.5*capability_match + 0.3*window_headroom + 0.2*provider_reliability
reliability= 1 - recent_error_rate(provider, 1h)                              # 来自 model.fallback/error 计数

cost_first   : score = 0.85*cost_eff + 0.10*quality + 0.05*reliability
quality_first: score = 0.80*quality + 0.10*cost_eff + 0.10*reliability
balanced     : score = 0.45*quality + 0.40*cost_eff + 0.15*reliability
```

- `capability_match`：`tool_call=true` 且 `structured_output=true` → 1.0；缺一 → 0.5（仅当阶段不需要该能力时才可入选）。
- `window_headroom`：`min(1, (context_window - stage_budget_tokens)/context_window)`，避免选"勉强够用"的模型导致频繁压缩。
- 平局（差值 < 0.02）→ 按 `reliability desc → context_window desc → id asc` 稳定排序，保证同一输入可复现同一结果。
- **记录可解释性**：`model.selected` 事件必须带 `candidates[].score` 与 `chosen` 三元组，满足 REQ-144"可解释"验收条件。

### 4.14.3 阶段级覆盖（REQ-145）

| 层级 | 配置位置 | 优先级 | 说明 |
|---|---|---|---|
| 阶段级 | `stage_overrides.{a..e}`（任务创建时可覆盖） | 最高 | 未配置 → 继承下一层 |
| 任务显式 | `CreateTaskRequest.model_id` | 次高 | 只作用于未单独覆盖的阶段 |
| 任务策略 | `model_strategy` | 次低 | 全任务统一策略，逐阶段独立求值（因各阶段 `stage_budget_tokens` 不同） |
| 全局默认 | `defaults.task_model` | 最低 | — |

**示例配置（Q17 / U18）**：

```yaml
stage_overrides:
  a: qwen-plus          # 理解需求，长上下文够用
  b: qwen-plus          # 计划
  c: qwen-plus          # 编码（需工具调用）
  d: qwen-max           # 验收用更强模型，降低假阳性/假阴性
  e: qwen-turbo         # 仅产出终态报告，最便宜足够
```

- **默认继承**：spec Q17 的默认结论是"不强制 d 用强模型，默认继承任务级"；上表是 `quality_first` 策略下的**推荐模板**，不是默认行为。是否将其设为默认，见 U18。
- **每次调用的选择事件**必须记录 `stage` 与 `effective_model`（REQ-145 验收条件）。
- **`e` 阶段的特殊性**：`e` 阶段不产生工具调用，且上下文需求小；若 `e` 被覆盖为不支持 `structured_output` 的模型仍可用（只产出报告文本），但**若报告要求结构化 JSON 则必须校验 `structured_output=true`**。

### 4.14.4 能力匹配校验（REQ-146）

**校验时机**：① 任务创建（对任务级模型做"最高阶段需求"校验）；② 每阶段进入前（对该阶段做精确校验）；③ 回退候选入选前（REQ-148 要求每个候选都过校验）。

**校验项**：

| # | 校验 | 不满足时 |
|---|---|---|
| 1 | 阶段需要工具调用（c、d 的 exec 类）→ 模型 `tool_call=true` | 拒绝 |
| 2 | `context_window ≥ system_prompt_tokens + goal_tokens + stage_budget_tokens` | 拒绝 |
| 3 | 阶段需要结构化输出（验收 verdict、finish 工具）→ `structured_output=true` | 拒绝 |
| 4 | embedding 场景：`embedding_dim == 目标索引维度` | 拒绝 |
| 5 | 模型 `status=available`（或处于允许的回退位） | 拒绝（作为主模型时）；回退时跳过该候选 |

**结构化错误（不得静默降级）**：

```json
{
  "code": "E_MODEL_CAPABILITY_MISMATCH",
  "message": "所选模型不满足 c 阶段的能力要求",
  "details": {
    "stage": "c",
    "required": {"tool_call": true, "min_context_window": 48200, "structured_output": true},
    "selected": {"model_id": "qwen-turbo", "tool_call": true, "structured_output": false, "context_window": 8192},
    "violations": ["context_window_too_small", "structured_output_missing"],
    "suggested_models": ["qwen-plus", "qwen-max"]
  },
  "trace_id": "..."
}
```

- `suggested_models` 必须**全部满足**上述校验项（REQ-146 验收条件明确要求）。
- 拒绝后**该阶段不得启动**（不是"用次优模型硬跑"），并写 `model.capability_rejected` 审计事件。

### 4.14.5 运行时切换（REQ-147）

**默认禁止**：任务运行期请求换模型 → `E_MODEL_SWITCH_FORBIDDEN`。

**允许的例外（穷举，不含其他）**：

| 例外 | 触发方 | 需审批 | 记录要求 |
|---|---|---|---|
| 主模型不可用回退（限流/超时/连续错误达阈值） | runtime 自动（4.14.6） | 否 | `model.switched{reason=unavailable, from, to, attempt_no}` |
| 注册表把该模型置 `unavailable`/`deprecated` | runtime 自动（下次调用前） | 否 | 同上 + `cause=registry_change` |
| 人工审批的模型升级（如 d 阶段临时换强模型） | 用户经大屏/CLI 请求 | **是**（复用审批，4.15.6） | `model.switched{reason=manual_upgrade, operator, approval_id}` |

**切换后的必做动作（REQ-150）**：重算 `threshold_tokens`（4.5.1）、重算预算估算、重算记忆注入 15% 预算上限、清理"基于旧模型窗口"的缓存（`EstimateTokens` 的窗口相关派生值）。未完成重算 → 拒绝切换（避免"用新模型跑旧阈值"）。

**与阶段边界的关系**：切换只在**同阶段的下一次调用**生效，不改变阶段划分；跨阶段切换（回退到 b 后换模型）走阶段级覆盖解析，不算"运行时切换"。

### 4.14.6 降级回退链（REQ-148/149/155）

**触发条件（满足任一）**：HTTP 429（非 quota 类）、连续 3 次超时、连续错误达阈值（`model.fallback.consecutive_errors=3`）。

```
for candidate in fallback_list(stage):            # 有序：阶段覆盖 > 全局 defaults.fallback
    if not capability_ok(candidate, stage): continue
    if status(candidate) == unavailable:     continue
    if stage_fallback_attempts >= 3: break        # REQ-149 单阶段回退上限 3 次
    try call(candidate)
        on success: emit model.switched(primary, candidate, reason, attempt_no)
                    emit model.fallback(primary, candidate, latency, tokens)
                    break
    on failure: stage_fallback_attempts += 1; continue
if all failed: 按异常分类处理（timeout / quota），父任务进入 4.3 的重试/终态流程
```

- **回退计入预算**：回退调用的 token 与费用计入父任务/阶段预算池（同一计量写入点），不新建账户（REQ-149）。
- **单阶段上限 3 次**：超限不再尝试，避免"回退风暴"把成本放大。上限可配（`model.fallback.max_per_stage=3`），但默认不放宽。
- **可用性探测与恢复（REQ-155）**：连续 5 次失败（`model.probe.fail_threshold=5`）→ `status=unavailable`，从自动选择候选中移除；探测成功 → `model.recovered` 并重新纳入。标记/恢复都写事件，大屏显示"不可用模型"清单。
- **回退列表为空**：不"死等"，直接按 timeout/quota 异常处理（REQ-148 验收条件）。

### 4.14.7 成本联动与压缩阈值（REQ-150，与 4.5.1 的关系）

> **两处定义的关系（明确回答"避免重复"）**：
> - **4.14 注册表 = 唯一事实源**（`context_window`、`price_*`、能力位）。
> - **4.5.1 = 消费方**：只读 `ModelRegistry.Get(effective_model).ContextWindow` 计算 `threshold_tokens = window × ratio`，**不再维护自己的窗口表**（已按 2026-11-10 修订收敛，4.5.1 的表格改为"注册表 seed 示例"）。
> - 任何"窗口"数字只允许出现在 `models.yaml`；代码中如出现硬编码窗口常量，视为缺陷（评审检查项 + 单测扫描）。

**联动清单（选择结果 → 下游）**：

| 下游 | 联动方式 |
|---|---|
| 预算估算 | `estimated_cost = Σ_stage (est_tokens_in*price_in + est_tokens_out*price_out)`，任务创建时按选定模型算并写 `budget_cost` |
| 压缩阈值 | `threshold_tokens` 按选定模型窗口重算（4.5.1） |
| 记忆注入预算 | `inject_budget = 0.15 × threshold_tokens`（4.7.4） |
| 验收阶段预算 | 验收模型与执行模型**可不同**：验收用 `stage_overrides.d`，其成本单列 `acceptance_budget`（4.3.4 的压缩/验收/执行三池） |
| 成本面板 | `model.cost_total{task,stage,model}` 按三元组聚合（4.14.9） |

### 4.14.8 密钥与配置安全（REQ-152）

| 项 | 规则 |
|---|---|
| 密钥来源 | 环境变量（`HARNESS_LLM_API_KEY` / provider 专属变量）或系统 keyring；**配置文件只允许写 `api_key_ref: env:VAR_NAME`**，不得写明文 |
| 不落日志 | 统一脱敏器（10.2）覆盖：`sk-[A-Za-z0-9]{8,}`、`Bearer ...`、`api_key=`；`models.yaml` 原样打印进日志的路径被禁止（配置加载日志只打 `version/模型数量/变更项键名`） |
| 不落事件 | `model.*` 事件 payload 只含 `model_id/provider/price`，不含任何凭据；`payload` 校验器增加"疑似密钥模式"扫描（命中即拒绝发送 + `event.invalid`） |
| 不落前端响应 | `/models` 接口返回注册表视图时**剔除** `api_key_ref` 与任何 `*_secret` 字段（白名单序列化，非黑名单过滤） |
| 配置变更审计 | `model.registry_changed`（操作人、时间、变更项、结果）；敏感值以 `***` 呈现；审计不可被普通用户删除（与 4.12.8 同规则） |
| 与 DFX-S-02/S-08 对齐 | 上述四条共同满足"凭据不得出现在日志/事件/记忆/前端"与"模型配置变更必须审计" |

### 4.14.9 可观测（REQ-154）

**事件（4.12.2 字典追加）**：

| 事件 | payload 关键字段 |
|---|---|
| `model.selected` | `stage`、`model_id`、`strategy`、`resolved_from(override\|explicit\|strategy\|default)`、`candidates[].score`、`window`、`threshold_tokens` |
| `model.switched` | `from`、`to`、`reason(unavailable\|registry_change\|manual_upgrade)`、`attempt_no`、`operator`、`approval_id` |
| `model.fallback` | `primary`、`fallback`、`trigger(rate_limit\|timeout\|error_threshold)`、`latency_ms`、`tokens` |
| `model.capability_rejected` | `stage`、`required`、`selected`、`violations`、`suggested[]` |
| `model.recovered` / `model.unavailable` | `model_id`、`fail_count`、`probe_window` |
| `model.registry_changed` | `operator`、`changed_keys`、`result` |

**指标（10.1 追加）**：

| 指标 | 类型 | 标签 |
|---|---|---|
| `hlp_model_calls_total` | counter | `task_id`（聚合时降维）、`stage`、`model` |
| `hlp_model_tokens_total` | counter | `stage`, `model`, `direction(in/out)` |
| `hlp_model_cost_total` | counter | `tenant`, `project`, `stage`, `model` |
| `hlp_model_fallback_total` | counter | `primary`, `fallback`, `trigger` |
| `hlp_model_select_latency_ms` | histogram | — （DFX-P-12：P95 ≤ 50ms） |
| `hlp_model_unavailable` | gauge | `model`、`provider` |

**大屏展示位（7 章联动）**：任务详情页新增"模型"页签——每阶段生效模型、回退次数、该阶段 token/成本；大屏顶部新增"模型分布（按调用次数）"与"回退趋势（1h）"两个小图；异常抽屉内出现"模型不可用/能力不匹配"时给出一跳跳转到模型清单。

---

## 4.15 工具权限与工作目录沙箱（REQ-156~170，2026-11-10 新增）

> **定位**：把"Agent 能做什么、能碰哪些文件、能跑哪些命令"从提示词约束变为**执行前同步校验的硬门禁**。原则三条：**默认拒绝**、**前层不可被后层放宽**、**拒绝必须留痕且不可静默放行**。
>
> **存量锚点**：`services/harness-service/internal/rule/rule.go:209-240` 已有 `PermissionRule{ToolPattern, Effect, MaxCalls, RequiresApproval}` 与 `PermissionMatrix`（按 agentType 分组的 deny 规则）；`pkg/agent/engine_stream.go:265-296` 已有"工具调用前查 rule + 走 approval"的注入点；`pkg/agent/approval/flow.go` 已有完整审批状态机。本设计**扩展**这些结构，不另起一套。

### 4.15.1 工具注册与权限标签（REQ-156）

**三类工具统一登记**：

| 类别 | 来源 | 登记位置 | 现状 |
|---|---|---|---|
| 内置工具 | `pkg/agent` 内置（read_file/write_file/edit_file/run_command 等） | `pkg/harnessloop/policy/registry.go`（新增的**标签补充表**） | 工具本体已存在（`pkg/agent/tools`），但**无权限标签** |
| MCP 工具 | `services/mcp-service`（`ListTools`） | mcp-service 注册时声明；`proto/mcp/mcp.proto` 的 `Tool` 追加 `permission_tags` | `Tool{name, description, input_schema}` 无标签字段，需扩展（追加式） |
| 自定义工具 | 租户/项目级注册 | 同上 | 需新增登记入口（管理 API） |

**标签语义（四类，取子集）**：

| 标签 | 含义 | 典型工具 |
|---|---|---|
| `read` | 读取文件/数据 | `read_file`、`list_dir`、`grep`、`search_code` |
| `write` | 写入/删除/移动 | `write_file`、`edit_file`、`delete_file`、`move_file` |
| `exec` | 执行命令 | `run_command`、`go_test`、`build` |
| `network` | 网络访问 | `http_get`、`web_fetch`、包管理安装 |

- **未声明标签 → 拒绝登记**（`E_TOOL_TAG_MISSING`），且未登记工具**不可被调度**（不是"登记后默认允许"）。
- 多标签工具按最严处理：`write+network` 的工具在只读阶段两个维度都不满足即拒绝。
- **标签不可自证**：工具声明 `read` 但实际写文件 → 由沙箱层兜底（4.15.3 的写入路径白名单与 4.15.10 的检测点），不依赖声明可信。

### 4.15.2 默认拒绝与双层名单（REQ-157）

**判定式（与逻辑，一目了然）**：

```
allow(tool, stage) =
    NOT hit_any_blacklist(tool, stage)              # 任一黑名单命中 → 直接拒绝
  AND in_task_whitelist(tool)                       # 任务级白名单
  AND in_stage_whitelist(tool, stage)               # 阶段级白名单
  AND tag_satisfied_by_stage(tool.tags, stage)      # 阶段权限矩阵（4.15.4）
  AND role_allows(role, tool)                       # 角色上限（4.13.7）
```

| 层 | 载体 | 优先级内规则 |
|---|---|---|
| 阶段黑名单 | `config/harness_loop/stage_deny.yaml` | **最高**：命中即 deny，不看白名单 |
| 任务黑名单 | `TaskRun.policy_snapshot.task_deny[]` | 次高 |
| 任务白名单 | `policy_snapshot.task_allow[]` | 必须命中 |
| 阶段白名单 | `config/harness_loop/stage_allow.yaml` | 必须命中 |
| 角色白名单 | agent-service `GetAgentTools(role)` | 必须命中（角色是上限） |

- **deny-by-default**：不在任何白名单 → `E_TOOL_NOT_ALLOWED`（不是 `E_TOOL_DENIED`，便于区分"未授权"与"显式禁用"）。
- **黑名单优先**：同一工具同时在任务白名单与阶段黑名单 → 拒绝（REQ-157 验收条件）。
- 允许通过 → 记录 `tool.allowed` 审计（含 `rule_id`），**不是静默通过**（REQ-166 要求每次调用都有决策记录）。

### 4.15.3 工作目录沙箱（REQ-158/159/160）

**三层约束**：

```
L1 根约束：workspace_root 必须为绝对路径（filepath.IsAbs）
          且位于租户授权根 allowed_roots[] 之下（创建任务时校验，不合规 → E_INVALID_WORKSPACE）
L2 路径约束：realpath(请求路径) 必须落在 workspace_root 的允许子路径内
L3 写入约束：write 类工具的 realpath 必须落在写白名单子路径内（产物目录/记忆目录/临时隔离目录）
```

**规范化算法（`sandbox/realpath.go`，顺序不可颠倒）**：

```
1. reject_if_contains_nul(path)                        # 内嵌 \0
2. reject_if_scheme_like(path)                          # "file://", "\\\\?\\", "\\\\" UNC 前缀
3. p = filepath.FromSlash(path)
4. if !filepath.IsAbs(p): p = filepath.Join(workspace_root, p)     # 相对路径以根为基准，不以上一层 cwd
5. p = filepath.Clean(p)                                # 消解 "." 与 ".."
6. rp = filepath.EvalSymlinks(p)
     - 目标不存在时：对"最近的已存在祖先"做 EvalSymlinks，再拼回剩余段（防止 ToCToU 绕过）
     - 任何一段解析到根外 → 立即拒绝（不"先执行后校验"）
7. 平台归一化：Windows 转小写比较（保留原大小写用于展示）、统一分隔符
8. containment check: strings.HasPrefix(rp_normalized, root_normalized + sep)
     - 特例：rp == root 本身允许（读根目录列表），但写操作需在子路径白名单内
9. 命中 allow 子路径列表（前缀匹配，且必须在路径段边界，禁止 /src-evil 匹配 /src）
```

**为什么必须在执行前做**：REQ-159 明确"不得先执行后校验"。实现上校验是工具 `Execute` 之前的**同步中间件**（`policy.Middleware`），拒绝即返回，工具函数根本不被调用（无副作用）。

**写入白名单（默认值）**：

| 白名单 | 路径 | 用途 |
|---|---|---|
| 产物目录 | `artifacts/`、`dist/`、`out/` | 构建/验收产物（可在任务配置中改名） |
| 记忆目录 | `.harness/memory/` | md 事实源（4.6） |
| 临时隔离目录 | `.harness/tmp/<task_id>/` | 压缩 JSONL、验收临时目录 |
| 已声明源码路径 | 任务 `editable_paths[]`（如 `src/`） | c 阶段编码 |

**禁止写入（系统硬限制，任何层级不可放宽）**：`.harness/policy/`、`config*.yaml`、`.git/`、`proto/`、`services/*/internal/`（策略与实现自保护）、`~/.harness/memory/.lock`、任何位于 `allowed_roots` 之外的路径。

### 4.15.4 阶段权限矩阵（REQ-161）

| 工具标签 \ 阶段 | a 需求理解 | b 计划 | c 执行编码 | d 验收测试 | e 结束 |
|---|---|---|---|---|---|
| `read` | ✅ 允许 | ✅ 允许 | ✅ 允许 | ✅ 允许 | ❌ **禁止任何工具调用** |
| `write` | ❌ 拒绝 | ❌ 拒绝 | ✅ 允许（受写白名单） | ❌ 拒绝 | ❌ 拒绝 |
| `exec` | ❌ 拒绝 | ❌ 拒绝 | ✅ 允许（exec 白名单） | ✅ 允许（**仅验收命令白名单**） | ❌ 拒绝 |
| `network` | ⚠️ 白名单 | ⚠️ 白名单 | ⚠️ 白名单 | ⚠️ 白名单 | ❌ 拒绝 |

- `⚠️ 白名单` 表示默认拒绝，仅当任务级 `network_allow[]` 显式列出域名/工具名时允许（REQ-161 的"network 按白名单受限"）。
- **a/b 只读**、**c 读写 + exec**、**d 只读 + exec 白名单**、**e 无工具**——这四句是矩阵的结论，代码中以 `stage_matrix.yaml` 表达，不在业务代码里 if-else 硬编码（DFX-M-05 可配置化）。
- **验收 Agent 复用 d 列**：4.2.6 的"只读约束"由本矩阵的 d 列实现（工具清单移除写类工具 + 沙箱拒绝），不是两套机制。
- **子任务继承父阶段**：子任务处在父的 c 阶段 → 使用 c 列 + 角色白名单求交（4.13.7），不单独定义子任务矩阵。

### 4.15.5 exec 命令白名单（REQ-162）

**允许清单（默认，按阶段）**：

| 阶段 | 允许命令（单条，不含参数拼接） |
|---|---|
| c | `go build`、`go vet`、`go test`、`gofmt -l`、`golangci-lint run`、`eslint`、`prettier --check`、`tsc --noEmit`、`pytest`、`python -m compileall` |
| d | `go test`、`go build`、`go vet`、`golangci-lint run`、`eslint`、`tsc --noEmit`、`pytest`（验收命令子集，**不含 `gofmt`/`prettier` 等格式化会写文件的命令**） |
| a/b/e | 空（不可执行任何命令） |

**拒绝规则（每条独立校验，任一命中即拒绝）**：

```
1. shell 元字符：; & | ` $ ( ) < > \n " && " " || " " $( " " ` " 
   → E_EXEC_NOT_ALLOWED（REQ-162 "复合命令必须拒绝"）
2. 可执行文件名不在白名单 → E_EXEC_NOT_ALLOWED
3. 参数出现以下模式 → 拒绝：
   - 绝对路径或含 ".."（除 arguments.allowed_paths 显式列出的）
   - 重定向目标（> / >> / tee）
   - 包管理器安装动作（install/add/get/update）
   - 网络动作（curl/wget/git clone/remote）
   - 删除动作（rm/del/rmdir/Remove-Item）
4. 执行方式：exec.CommandContext(name, args...) 直接起进程，**不经 shell**（无 sh -c / cmd /c）
5. 环境变量最小化：只传 PATH、HOME(TMPDIR)，剥离 API Key 类变量（DFX-S-02）
6. 超时（默认按命令类型：test 300s / build 120s / lint 120s）+ 工作目录固定在 workspace_root
```

- **为什么"拒绝复合命令"而不是"解析后逐段校验"**：解析 shell 语法本身就是一个高风险的子问题（引号、转义、别名、`$IFS`、Unicode 全角字符），逐段解析难以穷尽；直接拒绝元字符是"可证明安全"的做法，代价是计划需要拆成多次独立调用（可接受）。
- 白名单可扩展但**必须走配置 + 审计**：`config/harness_loop/exec_allow.yaml`，变更写 `policy.changed`，不允许 Agent 写入（4.15.10）。

### 4.15.6 审批联动（REQ-163）

**复用而非重建（明确说明现状与缺口）**：

| 能力 | 现状 | 本设计 |
|---|---|---|
| 审批状态机 | `pkg/agent/approval/flow.go`：`ApprovalFlowManager`（`CreateRequest`/`SubmitDecision`/`WaitForApproval`/`EscalateRequest`/超时/自动批准），`ApprovalStatus` 完整 | **直接复用**，不新增状态机 |
| 审批规则 | 同文件 `RuleEngine` + `ApprovalRule`；`harness-service/internal/rule` 的 `RequiresApproval` 标记 | 权限引擎命中 `requires_approval` 时生成请求，规则来源统一到 `config/harness_loop/approval.yaml` |
| 工具调用前拦截点 | `pkg/agent/engine_stream.go:265-296`（已有 rule + approval 分支，发 `approval` 流式事件） | 升级为调用 `policy.Decide()` 后分派 allow / deny / require_approval |
| 审批承载进程 | **在 agent-service 进程内**（工具调用实际发生处） | harnessloop-service **不持有审批队列**，只消费审批结果事件 + 新增薄代理 `GetApprovalStatus(approval_id)`（见 6.2） |

**高危规则（默认）**：

| 类别 | 判定 | 审批粒度 |
|---|---|---|
| 删除 | `write` 标签且工具名/参数语义为 delete/remove | 每次调用 |
| 网络写 | `network` 标签且方法 ∈ {POST, PUT, PATCH, DELETE} | 每次调用 |
| 部署 | 工具名匹配 `deploy*`/`release*`/`kubectl apply` | 每次调用 + 二次确认 |
| 大额成本 | 单次调用预估成本 > `policy.approval.cost_threshold`（默认任务预算 10%） | 每次触发 |

> "大额成本"不是工具属性而是**预算守卫**：由 `budget_guard` 在调用前估算成本并走同一审批通道，避免为成本单独造一套确认机制。

**超时与拒绝的终态处理**：

| 结果 | 处理 | 事件 |
|---|---|---|
| 审批通过 | 继续执行；结果与普通调用一致 | `tool.result` + `approval.approved` |
| 审批拒绝 | 调用失败，返回 `E_APPROVAL_REJECTED`（**不重试**——审批拒绝是确定性决策） | `tool.denied` + `approval.rejected` |
| 审批超时（默认 300s） | 视为拒绝，返回 `E_APPROVAL_TIMEOUT`；**不静默放行**（REQ-164） | `tool.denied` + `approval.timeout` |
| 任务被 abort（审批挂起中） | 取消审批请求，调用终止 | `approval.cancelled` |

超时阈值可配（`policy.approval.timeout_seconds`），但**不允许配置为"超时自动通过"**（配置校验拒绝该取值）。

> **与存量 `SetAutoApprove` 的冲突处理（2026-11-10 新增）**：`pkg/agent/approval/flow.go` 提供 `SetAutoApprove(enabled)`，可为**全部**请求开启自动批准。本设计要求高危类别（删除/网络写/部署/大额成本）**不得**被自动批准，因此：
> 1. 复用该设施但**不使用全局 `SetAutoApprove`**；改为在 `ApprovalRule` 级别按类别决定（存量 `RuleEngine` 已支持按规则匹配）。
> 2. 若运维显式开启全局自动批准，权限引擎在 `Decide()` 阶段仍对高危类别返回 `require_approval`，并在 `ApprovalRequest.RiskReason` 中标注 `policy.high_risk`——即"自动批准"只对非高危生效。
> 3. 评审核查项：搜索代码中 `SetAutoApprove(` 的调用点，确认不在 harnessloop 高危路径上。

### 4.15.7 权限来源优先级与解析算法（REQ-165）

**优先级**：`system > tenant > role > task`，**前层不可被后层放宽**。

**解析算法（`policy/resolver.go`，确定性、可复现）**：

```
Decide(req{Tool, Tags, Stage, Role, TaskID, Path, Args}) -> Decision{Effect, RuleID, Layer}

1. layers = [system, tenant, role, task]（按此顺序）
2. decisions = []
3. for L in layers:
     r = match(L, req)                      # 该层是否有命中规则（tool_name 或 pattern）
     if r == nil: continue
     if r.effect == deny:
         return {deny, r.rule_id, L}        # deny 立即短路，最高优先级
     if r.requires_approval: mark_pending(L, r)   # 不短路，继续收集（后续层仍可 deny）
     decisions.append({allow, r.rule_id, L})
4. if any pending_approval: return {require_approval, 最早命中的 rule_id, L}
5. # 到这里说明没有 deny；必须"任务层与角色层都 allow"才算授权
   if !has_allow(role) : return {deny, "policy.role.no_match", role}
   if !has_allow(task) : return {deny, "policy.task.no_match", task}
   return {allow, 最早命中的 rule_id, task}
6. # 正交校验（与层无关，永远执行）
   sandbox_check(path) / exec_check(args) / tag_stage_check(tags, stage)
   → 任一失败即 deny（sandbox 的 rule_id 形如 sandbox.path_escape）
```

**为什么 deny 短路而不是"后层覆盖"**：若允许 task 层的 allow 覆盖 tenant 层的 deny，等于"任务创建者可以自行放宽租户策略"，这是提权路径（REQ-165 验收条件"任务声明允许写入某路径但租户策略禁止 → 实际拒绝"）。短路保证**只能收紧不能放宽**。

**系统硬限制（不可覆盖清单）**：`system` 层包含：不可访问 `.harness/policy/`、密钥目录、`.git/` 内部对象、`/etc`、`/proc`、宿主 `~/.harness/memory/.lock`；不可执行任何 `system` 层 deny 的命令（`rm -rf /`、`shutdown`、`dd`）。即使 tenant/role/task 全 allow，仍拒绝。

**缓存与失效（REQ-170）**：

```
key = sha256(tenant | project | task | stage | role | tool | path_realpath | args_digest
             | policy_version | approval_version)
policy_version   : 任一规则文件 reload 时 +1（进程内单调）
approval_version : 每次审批状态变更 +1
cache: LRU 10k，TTL 60s（双保险：版本变了 key 自然不同）
```

- 权限变更、审批状态变化、配置热更新 → 下一次调用必然按新规则（REQ-170 验收条件"不出现旧缓存放行"）。
- **不缓存 deny 的"路径解析结果"以外的中间态**：`realpath` 结果可缓存，但 `EvalSymlinks` 在写操作前必须重新执行（防 ToCToU：校验时是文件，执行前被替换成符号链接）。写类工具**不做 realpath 缓存**。

### 4.15.8 越界拦截与结构化错误（REQ-164）

**统一错误结构**（Gateway REST 与 gRPC 共用，见 6.3）：

```json
{
  "code": "E_PATH_OUT_OF_SANDBOX",
  "message": "路径解析后不在工作区允许范围内",
  "details": {
    "rule_id": "sandbox.path_escape",
    "layer": "system",
    "tool": "write_file",
    "stage": "c",
    "subtask_id": "st-3",
    "requested_path": "src/../../etc/passwd",
    "normalized_path": "/etc/passwd",
    "workspace_root": "/workspace/repo-a",
    "decision": "deny"
  },
  "trace_id": "..."
}
```

**错误码表（新增，与 6.3 合并）**：

| code | HTTP/gRPC | 场景 | rule_id 示例 |
|---|---|---|---|
| `E_TOOL_TAG_MISSING` | 422 / InvalidArgument | 工具未声明权限标签 | `policy.registry.tag_missing` |
| `E_TOOL_NOT_ALLOWED` | 403 / PermissionDenied | 不在任何白名单（默认拒绝） | `policy.task.no_match` |
| `E_TOOL_DENIED` | 403 / PermissionDenied | 命中黑名单或系统硬限制 | `policy.stage_deny.delete`、`system.protected_path` |
| `E_TOOL_NOT_PERMITTED` | 403 / PermissionDenied | 角色未声明该工具（REQ-136） | `policy.role.no_match` |
| `E_PATH_OUT_OF_SANDBOX` / `E_PATH_ESCAPE` | 403 / PermissionDenied | 路径规范化后越界（`..`、绝对路径、符号链接） | `sandbox.path_escape` |
| `E_WRITE_PATH_DENIED` | 403 / PermissionDenied | 写路径不在写白名单 | `sandbox.write_allowlist` |
| `E_EXEC_NOT_ALLOWED` / `E_EXEC_DENIED` | 403 / PermissionDenied | 命令不在白名单或含复合结构 | `sandbox.exec_allowlist`、`sandbox.shell_metachar` |
| `E_APPROVAL_REJECTED` / `E_APPROVAL_TIMEOUT` | 403 / PermissionDenied | 审批拒绝/超时 | `policy.approval.*` |
| `E_STAGE_TOOL_FORBIDDEN` | 403 / PermissionDenied | 阶段矩阵禁止（如 a 阶段 write、e 阶段任意） | `policy.stage_matrix` |
| `E_POLICY_SELF_MODIFY` | 403 / PermissionDenied | 尝试修改策略/白名单（REQ-168） | `system.policy_self_protect` |

**三条硬约束**：
1. **不得静默降级为允许**：拒绝路径只有"返回错误"一种出口，代码中不允许 `if err != nil { continue }` 式放行（评审检查项 + 单测断言）。
2. **不得改写为"成功但无效果"**：拒绝不返回空结果冒充成功（否则 Agent 会基于错误假设继续）。
3. **重试不改变决策**：同一调用参数 + 未变更配置 → 同一 `rule_id` 的 deny（REQ-164 验收条件），由决策缓存与确定性算法保证。

### 4.15.9 审计与可追溯（REQ-166/167）

**每次工具调用必写一条审计（allow 与 deny 都写）**：

```json
{
  "audit_id": "01J9...",
  "ts": "2026-11-10T10:00:00Z",
  "tenant_id": "t-001", "project_id": "repo-a",
  "task_id": "7f3a", "subtask_id": "st-3", "handoff_id": null,
  "stage": "c", "attempt": 2, "role": "coder", "agent_id": "coder-1",
  "tool": "write_file",
  "tags": ["write"],
  "decision": "deny",
  "rule_id": "sandbox.write_allowlist",
  "layer": "system",
  "args_digest": "sha256:...",          // 参数整体哈希
  "args_summary": {"path": "src/../../etc/passwd", "bytes": 128},   // 脱敏摘要，不含文件内容
  "path_normalized": "/etc/passwd",
  "duration_us": 1200
}
```

- **参数摘要脱敏**：文件内容、密钥、长文本一律截断为 `{len, sha256_prefix}`；路径保留（路径本身是审计的关键证据）。
- **事件与审计一致（REQ-167）**：deny 只产生 `tool.denied`（**不产生** `tool.result`）；allow 产生 `tool.started` + `tool.result`。同一 `call_id` 在事件与审计中的 `decision`/`rule_id` 必须完全相同（由同一决策对象双写保证，不做二次判断）。
- **可检索**：支持按 `task_id` / `rule_id` / 时间范围 / `decision` 查询；按 `rule_id` 聚合可得到"各规则命中次数与拒绝比例"（REQ-166 验收条件）。
- **存储**：复用 `hlp_event.audit`（5.1 已有），不新建审计库；审计不可被普通用户删除（DB 权限）。

### 4.15.10 禁止项检测点（REQ-168/169）

| 禁止项 | 检测实现点 | 拦截方式 |
|---|---|---|
| Agent 通过工具参数改白名单 | `policy.Decide` 的 system 层：任何 `write` 到 `config/harness_loop/*`、`config*.yaml`、`.harness/policy/` → deny（`E_POLICY_SELF_MODIFY`） | 路径白名单 + 系统硬限制 |
| 通过环境变量提权 | 工具子进程环境变量白名单（只传 PATH/TMPDIR/HOME），剥离 `HARNESS_*_POLICY`；Agent 无法设置父进程环境 | `sandbox/execguard.go` |
| 通过配置写入工具提权 | 配置类工具（若存在）登记为 `system.deny`，且 `Tasks` 不可调用（未登记 → 不可调度） | 工具注册表 |
| 提示注入改变权限 | 权限来自 `policy_snapshot`（任务创建时冻结），执行期 Agent 上下文不含策略原文；注入文本无法影响 `Decide()` 输入 | 上下文构造（4.2.1 同理） |
| 符号链接逃逸 | `EvalSymlinks` + 逐段校验；写操作前重算（不缓存） | `sandbox/realpath.go` |
| `../` / 绝对路径逃逸 | `Clean` + `IsAbs` + containment check | 同上 |
| 路径编码变体（URL/Unicode/大小写） | 入口先做 `url.PathUnescape`（仅一次）+ `norm.NFC` 归一化 + Windows 小写比较；归一化后再进算法 1~9 步 | 同上 |
| 子进程绕过 | 禁止 `sh -c`；exec 白名单 + 元字符拒绝 | `sandbox/execguard.go` |
| 工具串联绕过（A 写脚本，B 执行） | 写白名单不含可执行目录（`bin/`、`scripts/` 默认不在白名单）；exec 只允许白名单命令名（不是"允许执行任意文件"） | 组合约束 |
| 任务结束时权限配置被改 | 任务创建时记录 `policy_hash`；结束时比对，不一致 → `security.violation` + 审计（REQ-168 验收条件） | `TaskRun.policy_snapshot_hash` |

### 4.15.11 性能（REQ-170，DFX-P-10）

| 路径 | 目标 | 实现手段 |
|---|---|---|
| 缓存命中（同 tool/stage/role/path/args） | P95 ≤ 5ms | LRU 10k + 决策对象直接返回（不做字符串拼接、不重读配置） |
| 缓存未命中 | P95 ≤ 20ms | 规则预编译为前缀树/正则集合；`realpath` 只对涉及的路径段做 syscall；规则文件常驻内存快照 |
| 对工具调用整体时延的影响 | ≤ 10% | 校验在工具执行前串行完成，不引入额外 RPC（审批仅在高危时触发，属异步等待，不计入校验时延） |

**压测口径**：1000 次工具调用（含 70% 缓存命中）→ 统计 `hlp_policy_decision_latency_ms` P50/P95/P99；同时断言"校验总耗时 / 工具总耗时 ≤ 10%"。

**指标（10.1 追加）**：`hlp_policy_decisions_total{decision,rule_id,layer}`、`hlp_policy_decision_latency_ms{hit}`、`hlp_policy_cache_hit_ratio`、`hlp_sandbox_escape_attempts_total{kind}`、`hlp_exec_denied_total{reason}`、`hlp_approval_pending`（gauge）。

---

# 五、数据设计

## 5.1 新增 PostgreSQL schema 汇总

| schema | 表 | 归属 | 写入方 | 读取方 |
|---|---|---|---|---|
| `hlp_mem` | `memory_topics`、`memory_chunks` | 记忆索引 | memory-service（同步器） | memory-service、harnessloop-service（注入） |
| `hlp_code` | `indexed_files`、`code_chunks`、`code_edges`、`embedding_cache` | 代码索引 | codeindex | harnessloop-service（检索 API） |
| `hlp_event` | `events`、`tool_idempotency`、`audit`、`schema_migrations` | 事件/幂等/审计 | harnessloop-service | gateway（SSE/查询）、harness-service（trace 桥接）、大屏 |

> **新增表与"复用事件表"的边界（2026-11-10 新增）**：
> - **子任务 / DAG / handoff / 模型选择** → 新建关系表（`hlp_task.*`）。理由：需要按状态过滤、行锁、唯一约束、与外键关联，放进 JSONB 事件流难以保证一致性与查询效率。
> - **工具权限决策审计** → **复用 `hlp_event.audit`**，不新建表。理由：① 频次高（每次工具调用一条，50 任务 × 数百调用/天），独立表会成倍放大写入；② 复用"审计不可删 + 已有导出 hook"能力（DFX-S-05）；③ 查询按 `task_id`/`rule_id`/时间已可由 `audit` 的列 + payload GIN 索引覆盖（见 5.1.5 的索引补充）。
> - **模型选择/切换** → 新建 `hlp_task.model_selections`（低频、需结构化对账：阶段 × 模型 × token × 成本），同时仍发 `model.*` 事件供大屏消费（事件是"流"，表是"账"，两者不互替）。
| `hlp_task` | `task_runs`、`stage_attempts`、`acceptance_records`、**`task_dags`、`subtasks`、`subtask_attempts`、`subtask_acceptance_records`、`handoff_records`、`model_selections`**（2026-11-10 新增） | FSM 状态（关系型）+ 多 Agent 协作 + 模型选择记录 | harnessloop-service | 同上 |

`hlp_task` 说明：TaskRun/StageAttempt/AcceptanceVerdict 需要"按 tenant/状态/时间"查询与行锁，放关系表比放 MongoDB 更合适；检查点的大 blob 仍在 MongoDB（D7），两者通过 `checkpoint_id` 关联。

```sql
CREATE SCHEMA IF NOT EXISTS hlp_task;

CREATE TABLE hlp_task.task_runs (
  task_id            text PRIMARY KEY,
  tenant_id          text NOT NULL,
  project_id         text NOT NULL,
  workspace_root     text NOT NULL,
  goal               text NOT NULL,
  goal_hash          text NOT NULL,
  current_stage      char(1) NOT NULL CHECK (current_stage IN ('a','b','c','d','e')),
  status             text NOT NULL CHECK (status IN ('running','paused','completed','aborted','failed')),
  terminal_reason    text CHECK (terminal_reason IN ('completed','aborted','failed')),
  attempt_no         jsonb NOT NULL DEFAULT '{}',
  retry_count        jsonb NOT NULL DEFAULT '{}',
  acceptance_fail    jsonb NOT NULL DEFAULT '{}',
  budget_tokens      bigint NOT NULL DEFAULT 0,
  budget_cost        numeric(12,4) NOT NULL DEFAULT 0,
  consumed_tokens    bigint NOT NULL DEFAULT 0,
  consumed_cost      numeric(12,4) NOT NULL DEFAULT 0,
  fsm_enabled        boolean NOT NULL DEFAULT false,
  policy_snapshot    jsonb NOT NULL,
  version            bigint NOT NULL DEFAULT 0,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_task_tenant_status ON hlp_task.task_runs (tenant_id, status, updated_at DESC);

CREATE TABLE hlp_task.stage_attempts (
  attempt_id        text PRIMARY KEY,
  task_id           text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  stage             char(1) NOT NULL CHECK (stage IN ('a','b','c','d')),
  attempt_no        integer NOT NULL,
  entry_reason      text NOT NULL CHECK (entry_reason IN ('initial','acceptance_failed','retry','resume')),
  started_at        timestamptz NOT NULL,
  ended_at          timestamptz,
  finish_received   boolean NOT NULL DEFAULT false,
  acceptance_id     text,
  retry_count       integer NOT NULL DEFAULT 0,
  acceptance_fail_count integer NOT NULL DEFAULT 0,
  UNIQUE (task_id, stage, attempt_no)
);

CREATE TABLE hlp_task.acceptance_records (
  acceptance_id     text PRIMARY KEY,
  task_id           text NOT NULL,
  stage             char(1) NOT NULL,
  attempt_id        text NOT NULL,
  verdict           text NOT NULL CHECK (verdict IN ('pass','fail','blocked')),
  verified_by       text NOT NULL CHECK (verified_by IN ('program','model','mixed')),
  reason            text NOT NULL,
  evidence          jsonb NOT NULL,
  failed_criteria   jsonb,
  unverified        jsonb,
  rollback_target   char(1),
  prompt_version    text NOT NULL,
  model             text,
  input_digest      text NOT NULL,
  tokens_in         integer,
  tokens_out        integer,
  duration_ms       bigint,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_accept_task_stage ON hlp_task.acceptance_records (task_id, stage, created_at DESC);
```

### 5.1.2 多 Agent 协作表（2026-11-10 新增，REQ-123~141）

```sql
-- DAG 快照：一次规划的产物，回退重规划产生新 dag_id，旧快照保留（审计）
CREATE TABLE hlp_task.task_dags (
  dag_id          text PRIMARY KEY,
  task_id         text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  tenant_id       text NOT NULL,
  project_id      text NOT NULL,
  attempt_no      integer NOT NULL,            -- 产生该 DAG 的 b 阶段 attempt
  plan_hash       text NOT NULL,               -- 计划产物 canonical JSON 哈希
  nodes           jsonb NOT NULL,              -- [{node_id, role, contracts, splittable}]
  edges           jsonb NOT NULL,              -- [{from,to}]
  parallel_sets   jsonb NOT NULL,              -- [{set_id, nodes[], failure_strategy, tolerate_ratio}]
  node_count      integer NOT NULL,
  max_depth       integer NOT NULL,
  status          text NOT NULL CHECK (status IN ('active','superseded','rejected')),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_dags_task ON hlp_task.task_dags (task_id, created_at DESC);

-- 子任务：父子关系 + 独立状态机 + 独立 attempt/检查点
CREATE TABLE hlp_task.subtasks (
  subtask_id        text PRIMARY KEY,
  parent_task_id    text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  dag_id            text NOT NULL REFERENCES hlp_task.task_dags(dag_id) ON DELETE CASCADE,
  dag_node_id       text NOT NULL,
  depth             integer NOT NULL DEFAULT 1 CHECK (depth BETWEEN 1 AND 2),
  index_in_parent   integer NOT NULL,          -- DAG 内 0-based 序号，稳定排序用
  parallel_set_id   text,
  tenant_id         text NOT NULL,
  project_id        text NOT NULL,
  role              text NOT NULL,
  status            text NOT NULL CHECK (status IN
                    ('pending','running','waiting_handoff','accepted','rejected','failed','cancelled')),
  depends_on        jsonb NOT NULL DEFAULT '[]',   -- dag_node_id 列表（同 dag_id 内）
  input_contract    jsonb NOT NULL,
  output_contract   jsonb NOT NULL,
  acceptance_criteria jsonb NOT NULL,
  failure_strategy  text NOT NULL CHECK (failure_strategy IN ('fail-fast','tolerate','retry')),
  attempt_no        integer NOT NULL DEFAULT 0,
  artifact_refs     jsonb NOT NULL DEFAULT '[]',   -- accepted 时必须非空
  handoff_id        text,                          -- waiting_handoff 时必填
  acceptance_id     text,                          -- accepted 时必须非空（4.13.11 拦截点）
  class_key         text NOT NULL,                 -- 熔断同类键（4.13.3）
  alloc_tokens      bigint NOT NULL DEFAULT 0,
  consumed_tokens   bigint NOT NULL DEFAULT 0,     -- 只读视图，真实计量在 task_runs
  consumed_cost     numeric(12,4) NOT NULL DEFAULT 0,
  failure_category  text,
  timeout_at        timestamptz,
  started_at        timestamptz,
  ended_at          timestamptz,
  UNIQUE (dag_id, dag_node_id),
  CONSTRAINT ck_subtask_accepted CHECK (
    status <> 'accepted' OR (acceptance_id IS NOT NULL AND jsonb_array_length(artifact_refs) > 0)
  ),
  CONSTRAINT ck_subtask_handoff CHECK (
    status <> 'waiting_handoff' OR handoff_id IS NOT NULL
  )
);
CREATE INDEX idx_subtasks_parent ON hlp_task.subtasks (parent_task_id, status, index_in_parent);
CREATE INDEX idx_subtasks_class  ON hlp_task.subtasks (parent_task_id, class_key, status);
CREATE INDEX idx_subtasks_tenant ON hlp_task.subtasks (tenant_id, project_id, status);

-- 子任务级 attempt（独立于父的 stage_attempts）
CREATE TABLE hlp_task.subtask_attempts (
  subtask_attempt_id text PRIMARY KEY,
  subtask_id         text NOT NULL REFERENCES hlp_task.subtasks(subtask_id) ON DELETE CASCADE,
  attempt_no         integer NOT NULL,
  entry_reason       text NOT NULL CHECK (entry_reason IN ('initial','retry','handoff_return','resume')),
  checkpoint_id      text,                     -- MongoDB harness_loop_checkpoints._id
  started_at         timestamptz NOT NULL,
  ended_at           timestamptz,
  UNIQUE (subtask_id, attempt_no)
);
```

-- 子任务级验收记录：**不复用** acceptance_records
-- 原因：acceptance_records.stage 是 char(1) NOT NULL 且取值 a~d；REQ-124 明确禁止子任务使用 a~e 阶段标识，
--       复用会强迫给子任务编造一个阶段值，语义错误。故独立建表，以 subtask_id + attempt_no 定位。
CREATE TABLE hlp_task.subtask_acceptance_records (
  subtask_acceptance_id text PRIMARY KEY,
  subtask_id            text NOT NULL REFERENCES hlp_task.subtasks(subtask_id) ON DELETE CASCADE,
  parent_task_id        text NOT NULL,
  attempt_no            integer NOT NULL,
  verdict               text NOT NULL CHECK (verdict IN ('pass','fail','blocked')),
  verified_by           text NOT NULL CHECK (verified_by IN ('program','model','mixed')),
  reason                text NOT NULL,
  evidence              jsonb NOT NULL,
  failed_criteria       jsonb,
  unverified            jsonb,
  prompt_version        text NOT NULL,
  model_id              text,                       -- 子任务验收所用模型（4.14.5）
  input_digest          text NOT NULL,
  duration_ms           bigint,
  created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_st_acc_subtask ON hlp_task.subtask_acceptance_records (subtask_id, created_at DESC);

**约束落地说明**：
- `ck_subtask_accepted` 是 REQ-133/141"未验收不得汇聚"的**数据库级兜底**（应用层校验之外的第二道闸门）。
- **子任务验收与父阶段验收是两套记录、同一判定器**：判定逻辑复用 `pkg/harnessloop/acceptance`（4.2），但输入收窄到子任务 `input_contract/output_contract/acceptance_criteria`，产出写 `subtask_acceptance_records`（U19 待确认是否改用轻量验收器）。
- `consumed_*` 只作"该子任务消耗了多少"的视图；真实预算池在 `task_runs.consumed_*`，**不存在子任务独立预算账户**（REQ-141）。子任务计量写入通过 `task_repo.AddConsumed(parent_task_id, ...)` 单点完成。
- `depends_on` 用 JSONB 而非独立边表：DAG 是"一次规划的不可变快照"，节点数 ≤ 20，不需要图查询；需要图遍历时读 `task_dags.edges`（同样 JSONB）。

### 5.1.3 handoff 记录（2026-11-10 新增，REQ-131/132）

```sql
CREATE TABLE hlp_task.handoff_records (
  handoff_id        text PRIMARY KEY,
  parent_task_id    text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  subtask_id        text NOT NULL REFERENCES hlp_task.subtasks(subtask_id) ON DELETE CASCADE,
  tenant_id         text NOT NULL,
  project_id        text NOT NULL,
  from_agent_id     text NOT NULL,
  from_role         text NOT NULL,
  to_agent_id       text NOT NULL,
  to_role           text NOT NULL,
  dag_id            text NOT NULL,
  contract          jsonb NOT NULL,            -- HandoffContract/v1 原文（五要素 + 未完成项 + 约束）
  artifacts         jsonb NOT NULL,            -- [{path,hash,size}]
  context_digest    text NOT NULL,             -- 接收方上下文的 canonical 哈希（禁共享证明）
  injected_from     jsonb NOT NULL,            -- 接收方消息来源白名单枚举（不含"executor_history"）
  a2a_task_id       text,                      -- a2a-service 返回的任务 id（传输复用证据）
  status            text NOT NULL CHECK (status IN ('created','accepted','rejected','expired')),
  reject_reason     text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  accepted_at       timestamptz,
  CONSTRAINT ck_handoff_tenant_same CHECK (tenant_id IS NOT NULL AND project_id IS NOT NULL)
);
CREATE INDEX idx_handoff_task    ON hlp_task.handoff_records (parent_task_id, created_at DESC);
CREATE INDEX idx_handoff_subtask ON hlp_task.handoff_records (subtask_id);
CREATE INDEX idx_handoff_agent   ON hlp_task.handoff_records (to_agent_id, status);
```

- `context_digest` + `injected_from` 是 REQ-132"禁止隐式共享全量对话"的**可核查证据**：接收方上下文构造完成后计算哈希并落库，`injected_from` 中若出现 `executor_history` 说明实现违约（单测 + 审计双检）。
- `a2a_task_id` 为"复用 a2a 传输"的可核查证据（REQ-140 评审核查项）。
- 跨租户/跨项目 handoff 在应用层校验（4.13.8），DB 层通过 `parent_task_id` 级联 + 应用写入前比对父任务租户来兜底。

### 5.1.4 模型选择记录（2026-11-10 新增，REQ-143/144/145/154）

```sql
CREATE TABLE hlp_task.model_selections (
  selection_id     text PRIMARY KEY,
  task_id          text NOT NULL REFERENCES hlp_task.task_runs(task_id) ON DELETE CASCADE,
  stage            char(1) NOT NULL CHECK (stage IN ('a','b','c','d','e')),
  attempt_no       integer NOT NULL,
  model_id         text NOT NULL,
  strategy         text NOT NULL CHECK (strategy IN ('cost_first','quality_first','balanced','explicit')),
  resolved_from    text NOT NULL CHECK (resolved_from IN ('override','explicit','strategy','default','fallback')),
  reason           text,                          -- 回退/切换原因：unavailable|rate_limit|manual_upgrade
  candidates       jsonb,                         -- [{model_id, score, rejected_reason}]，可解释证据
  context_window   integer NOT NULL,              -- 选择时的窗口快照（用于阈值复算对账）
  threshold_tokens bigint NOT NULL,               -- window × ratio，写入即冻结
  input_price      numeric(12,6) NOT NULL,
  output_price     numeric(12,6) NOT NULL,
  tokens_in        bigint NOT NULL DEFAULT 0,
  tokens_out       bigint NOT NULL DEFAULT 0,
  cost             numeric(12,4) NOT NULL DEFAULT 0,
  is_fallback      boolean NOT NULL DEFAULT false,
  approval_id      text,                          -- manual_upgrade 时必填
  operator         text,
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_model_sel_task  ON hlp_task.model_selections (task_id, stage, created_at);
CREATE INDEX idx_model_sel_combo ON hlp_task.model_selections (stage, model_id);
CREATE INDEX idx_model_sel_tenant ON hlp_task.model_selections (created_at DESC);
```

- **`threshold_tokens` 与 `context_window` 落库**：满足 REQ-143"变更不追溯修改既有选择记录"——单价/窗口是**选择时的快照**，注册表后续改动不影响历史行。
- 按 `(task, stage, model)` 聚合 `tokens_in/out/cost` 即得 REQ-154 的三项统计（配合 `hlp_model_*` 指标）。
- `is_fallback` + `reason` 支持"回退趋势"查询；`approval_id` 满足 REQ-147"未经审批的升级拒绝并可审计"。

### 5.1.5 权限审计：复用 `hlp_event.audit`（2026-11-10 新增，REQ-166/167）

不新建表（理由见 5.1 表下的边界说明）。`hlp_event.audit` 的 payload 规范为固定键集，便于按规则聚合：

```sql
-- 约束 payload 结构（应用层写前校验，DB 用 CHECK 兜底）
ALTER TABLE hlp_event.audit
  ADD CONSTRAINT ck_audit_tool_decision CHECK (
    (payload->>'kind') <> 'tool_decision' OR (
      payload ? 'decision' AND payload ? 'rule_id' AND payload ? 'layer'
      AND payload->>'decision' IN ('allow','deny','require_approval')
    )
  );

-- 支撑"按 rule_id 统计命中次数与拒绝比例"（REQ-166 验收条件）
CREATE INDEX idx_audit_rule   ON hlp_event.audit ((payload->>'rule_id'), ts DESC);
CREATE INDEX idx_audit_task   ON hlp_event.audit ((payload->>'task_id'), ts DESC);
CREATE INDEX idx_audit_payload ON hlp_event.audit USING GIN (payload jsonb_path_ops);
```

**决策主体是"同一对象双写"**：`tool.denied` 事件与 `audit(kind=tool_decision)` 由同一个 `Decision` 结构体生成，保证 REQ-167"事件与审计结论、rule_id 完全一致"。

## 5.2 数据归属与一致性

| 实体 | 事实源 | 索引/副本 | 一致性策略 | 冲突裁决 |
|---|---|---|---|---|
| 记忆主题 | markdown 文件 | `hlp_mem.*` | 单向同步（≤10s） | 以 md 为准，索引重建 |
| 记忆向量 | 由 md 派生 | `hlp_mem.memory_chunks` | 同步时重算；失败保留旧向量 | 以 md 为准 |
| legacy 记忆 | SQLite+Qdrant | — | `source=legacy` 时与现状一致 | 不参与 |
| 代码分块 | 源码文件 | `hlp_code.*` | 单事务增量；hash 判定 | 以磁盘为准 |
| 任务状态 | `hlp_task.task_runs` | 事件流（可重建） | 快照 + 事件水位 | 事件重建优先（4.4.4） |
| 检查点 | MongoDB `harness_loop_checkpoints` | — | 与任务状态交叉校验 | 校验失败即 drift |
| 事件 | 生产端内存 + PG | Redis 扇出 | 写 PG 成功后推送 | `event_id` 去重 |
| 知识文档 | 上传文件（Mongo 原文） | Mongo chunk + Qdrant | 异步状态机 | 以文件内容 hash 为准 |
| 审计 | PG `hlp_event.audit` | 导出到 harness-service | 同步写 | 不可修改 |
| 子任务 / 父子关系（2026-11-10 新增） | `hlp_task.subtasks` | 事件流（可重建汇聚视图） | 状态迁移先写事件再更新行；父子关系不可变 | 以行状态为准，事件用于归约展示 |
| 子任务 DAG（2026-11-10 新增） | `hlp_task.task_dags` | — | 不可变快照；重规划新建 dag_id | 以 `status=active` 的 DAG 为准，旧 DAG 只读 |
| handoff 记录（2026-11-10 新增） | `hlp_task.handoff_records` | `a2a-service` 任务（传输副本） | 创建即写；接受/拒绝回填 | 以 PG 记录为准，a2a 任务状态仅作传输证据 |
| 模型选择（2026-11-10 新增） | `hlp_task.model_selections` | 注册表（配置事实源，不存历史） | 选择时冻结窗口/单价快照 | 历史行不可被注册表变更改写（REQ-143） |
| 工具权限决策（2026-11-10 新增） | `hlp_event.audit`（复用） | 无 | 每次调用同步写（allow/deny 均写） | 审计不可修改；拒绝决策不因重试改变 |

## 5.3 迁移与版本管理

- 工具：`golang-migrate/migrate`（新增依赖）或自研 `pkg/pgindex/migrate.go`（内嵌 SQL + `schema_migrations` 表 + advisory lock 防并发迁移）。**选自研**：避免额外 CLI 依赖，迁移文件随二进制内嵌（`go:embed`）。
- 版本表：

```sql
CREATE TABLE hlp_event.schema_migrations (
  version    bigint PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now(),
  checksum   text NOT NULL
);
```

- 规则：只允许追加式迁移（`ALTER TABLE ... ADD COLUMN` 带默认值）；删列/改类型必须"新列 + 双写 + 回填 + 切换 + 删旧列"四步，跨两个版本完成（DFX-C-04）。
- 启动顺序：PG 连接 → 获取 `pg_advisory_lock(migrate)` → 应用未执行迁移 → 释放 → 服务启动。
- 回滚：每个迁移提供 `down.sql`；生产回滚只允许"前滚修复"（避免数据丢失），down 仅用于开发环境。

## 5.4 双写/单向同步策略矩阵

| 场景 | 开关 | 写入目标 | 读取路径 | 回滚 |
|---|---|---|---|---|
| 记忆（默认） | `memory.source=markdown` | md → PG 单向 | PG（失败降级 md） | 置 `legacy` |
| 记忆（灰度对比） | `memory.dual_write=true` | md + SQLite/Qdrant | 以 md 为准；Qdrant 仅对照 | 关开关 |
| 记忆（回滚） | `memory.source=legacy` | SQLite + Qdrant | 现状 | — |
| 代码索引 | `codeindex.enabled=true` | PG 单向 | PG | 关开关 → 检索 API 返回 `E_INDEX_DISABLED` |
| 知识文档 | 不变 | Mongo + Qdrant | 现状 + rerank | `knowledge.rerank.enabled=false` |
| FSM | `harness.loop.fsm.enabled` | 新链路 | 新链路 | 关开关（新任务走现状） |
| 事件 | `harness.loop.events.persist=true` | PG + Redis | PG/SSE | 关持久化 → 只走 trace 内存 |
| 多 Agent 协作（2026-11-10 新增） | `harness.loop.subtask.enabled`（默认 false，灰度开启） | 新链路（DAG + 调度） | 新链路 | 关开关 → 父任务按"单执行者"模式跑完 c 阶段（不拆分），行为等同 v1.0 |
| 模型选择（新增） | `harness.loop.model.strategy`（`balanced` 默认）与 `model.strategy.frozen=true`（任务创建时冻结） | 注册表 + 选择记录 | 注册表快照 | 置 `pkg/config.LLM.Model` 兜底模式（等价现状单模型） |
| 权限沙箱（新增） | `harness.loop.policy.enforce`（默认 true）；`policy.mode=shadow` 仅记录不拒绝（**仅限灰度观测，不得长期开启**） | policy_snapshot | 决策缓存 | 置 `shadow` 用于灰度对比；**生产必须在 24h 内切回 `enforce`** |

**禁止"双写两份事实源"**：`dual_write` 仅用于灰度期对照（Qdrant 为只读副本、不作为读取来源），避免双向冲突。

## 5.5 存量数据处理

| 存量数据 | 处理 | 说明 |
|---|---|---|
| Qdrant `memories` collection | 保留只读，不迁移 | `source=legacy` 时继续用；`markdown` 模式下不读 |
| SQLite memory 表 | 保留 | legacy 路径与审计需要 |
| Mongo 文档/Agent/检查点 | 保留 | 新增 `harness_loop_*` collection，不修改旧 collection |
| Qdrant `knowledge` collection | 保留并继续写入 | 不迁 PG（D2） |
| 代码库 | 首次 `index --full` 全量构建 | 不涉及存量（此前无索引） |
| harness-service SQLite | 保留 | 仅新增 trace 桥接字段，不改表结构 |

**首次上线顺序**：建 PG → 迁移 → 开启 codeindex 全量 → 开启 memory 索引同步（全量扫描 md，首次为空则建空索引）→ 灰度开启 FSM（按 tenant 白名单）→ 全量。

## 5.6 容量估算

| 数据 | 单量 | 规模假设 | 存储估算 |
|---|---|---|---|
| `memory_topics` | ~2KB/行 | 1000 项目 × 30 主题 = 3 万行 + 长期 1 文件 × 50 主题（超出仅告警不淘汰，按实际增长估算） | ~60MB + 索引 20MB（长期部分随告警增长，见 R25）（2026-11-11 修订） |
| `memory_chunks` | 1KB 文本 + 4KB 向量 | 每主题 2 chunk = 6 万行 | ~300MB |
| `code_chunks` | 2KB 文本 + 4KB 向量 | 500 文件 × 20 chunk = 1 万/项目；100 项目 | ~6GB |
| `code_edges` | ~200B/行 | 每 chunk 3 边 | ~2GB（含索引） |
| `events` | ~1KB/条 | 50 任务 × 2000 条/天 = 10 万/天；保留 30 天 | ~3GB + 索引 1GB |
| `task_runs`/`attempts`/`acceptance` | ~2KB | 100 任务/天 × 30 天 | ~100MB |
| `subtasks` / `subtask_attempts`（2026-11-10 新增） | ~3KB/行（含两份契约 JSON） | 100 任务/天 × 平均 6 子任务 × 30 天 = 1.8 万行 | ~54MB + 索引 12MB |
| `task_dags`（2026-11-10 新增） | ~8KB（20 节点 + 边 + 并行集） | 同任务量 × 1.2（含重规划） | ~29MB |
| `handoff_records`（2026-11-10 新增） | ~6KB（契约含五要素摘要） | 约为子任务数的 15% = 2700 行/30 天 | ~16MB |
| `model_selections`（2026-11-10 新增） | ~1KB | 每任务每阶段 1 行 + 回退行 ≈ 6.5 行/任务 → 2 万行/30 天 | ~20MB + 索引 5MB |
| 工具权限审计（复用 `audit`） | ~1KB | 每任务约 300 次工具调用 → 3 万行/天 | 计入 `events` 容量（约 +30%） |

单实例 8C/32G + 200GB SSD 可满足第一阶段；向量索引 `maintenance_work_mem` 建议 1-2GB（全量构建时）。

---

# 六、接口设计

## 6.1 新增/变更 proto 汇总

| 文件 | 类型 | 内容 | 兼容性 |
|---|---|---|---|
| `proto/harnessloop/harnessloop.proto` | 新增 | Task/Stage/Acceptance/Checkpoint/Event/Memory/Index 服务；**追加 Subtask/DAG/Handoff、Model、Policy 三组 RPC（2026-11-10 新增）** | 新命名空间，不影响旧契约 |
| `proto/mcp/mcp.proto` | 变更（2026-11-10 新增） | `Tool` 追加 `repeated string permission_tags = 4` | 追加式，旧客户端忽略新字段 |
| `proto/codeindex/codeindex.proto` | 新增 | 代码检索与索引状态查询 | 同上 |
| `proto/memory/memory.proto` | 变更 | 追加 `RecallForStage`、`SyncFromMarkdown`、`MemoryTopicRef`；**不删不改**旧字段 | 追加式 |
| `proto/knowledge/knowledge.proto` | 变更 | `Document` 追加 `kb_id`、`state`、`failure_reason`；`SearchResult` 追加 `sources`；`SearchRequest` 追加 `kb_id`、`rerank` | 追加式，旧客户端可忽略新字段 |
| `proto/agent/agent.proto` | 变更 | `ExecuteStreamRequest` 复用 `context_vars` 传 `__stage_mode__`，**不改 proto** | 零变更（推荐做法） |
| `proto/harness/harness.proto` | 不变 | trace/session/abtest/flag 复用 | — |

## 6.2 gRPC 方法签名草案

```protobuf
syntax = "proto3";
package harnessloop;
option go_package = "agent-platform/pkg/pb/harnessloop";

service HarnessLoopService {
  // 任务生命周期
  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse);
  rpc GetTask(GetTaskRequest) returns (TaskView);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);
  rpc PauseTask(TaskCommand) returns (TaskView);
  rpc ResumeTask(ResumeTaskRequest) returns (TaskView);
  rpc AbortTask(TaskCommand) returns (TaskView);

  // 事件
  rpc WatchEvents(WatchEventsRequest) returns (stream HarnessEvent);
  rpc ListEvents(ListEventsRequest) returns (ListEventsResponse);

  // 检查点
  rpc ListCheckpoints(ListCheckpointsRequest) returns (ListCheckpointsResponse);
  rpc RestoreFromCheckpoint(RestoreRequest) returns (TaskView);

  // 记忆（md 事实源）
  rpc UpsertMemoryTopic(UpsertMemoryTopicRequest) returns (MemoryTopicView);
  rpc ReadMemoryTopic(ReadMemoryTopicRequest) returns (MemoryTopicView);
  rpc DeleteMemoryTopic(DeleteMemoryTopicRequest) returns (Empty);
  rpc ListMemoryTopics(ListMemoryTopicsRequest) returns (ListMemoryTopicsResponse);

  // 代码索引
  rpc SearchCode(SearchCodeRequest) returns (SearchCodeResponse);
  rpc IndexStatus(IndexStatusRequest) returns (IndexStatusResponse);
  rpc TriggerIndex(TriggerIndexRequest) returns (TriggerIndexResponse);

  // ===== 多 Agent 协作（2026-11-10 新增，REQ-123~141）=====
  rpc GetSubtask(GetSubtaskRequest) returns (SubtaskView);
  rpc ListSubtasks(ListSubtasksRequest) returns (ListSubtasksResponse);        // 强制 tenant+project 过滤
  rpc GetDAGStatus(GetDAGStatusRequest) returns (DAGStatusView);              // 节点状态 + 并行集进度
  rpc CancelSubtask(CancelSubtaskRequest) returns (SubtaskView);              // 需 tenant+token
  rpc ResetCircuit(ResetCircuitRequest) returns (CircuitStateView);           // 人工复位熔断（审计）
  rpc ListHandoffs(ListHandoffsRequest) returns (ListHandoffsResponse);
  rpc GetHandoff(GetHandoffRequest) returns (HandoffView);
  rpc GetAggregationReport(GetAggregationReportRequest) returns (AggregationReport);

  // ===== 模型选择（2026-11-10 新增，REQ-142~155）=====
  rpc ListModels(ListModelsRequest) returns (ListModelsResponse);             // 已剔除密钥字段
  rpc ResolveModel(ResolveModelRequest) returns (ResolveModelResponse);       // 能力匹配预检 + 候选列表
  rpc RequestModelSwitch(RequestModelSwitchRequest) returns (ModelSwitchView);// 仅允许审批升级入口

  // ===== 工具权限（2026-11-10 新增，REQ-156~170）=====
  rpc PreflightTool(PreflightToolRequest) returns (ToolDecision);             // 权限预检（不执行）
  rpc GetPolicySnapshot(GetPolicySnapshotRequest) returns (PolicySnapshotView);
  rpc GetApprovalStatus(GetApprovalStatusRequest) returns (ApprovalStatusView);// 薄代理，转发 agent-service
}

service CodeIndexService {
  rpc Search(SearchCodeRequest) returns (SearchCodeResponse);
  rpc GetFileStatus(GetFileStatusRequest) returns (FileIndexStatus);
  rpc Rebuild(RebuildRequest) returns (RebuildResponse);
}

message CreateTaskRequest {
  string tenant_id = 1;
  string project_id = 2;
  string workspace_root = 3;
  string goal = 4;
  repeated StageCriteria stage_criteria = 5;
  Budget budget = 6;
  string user_id = 7;
  string idempotency_key = 8;
}

message StageCriteria {
  string stage = 1;                 // a|b|c|d
  repeated Criterion criteria = 2;
}
message Criterion {
  string criterion_id = 1;
  string type = 2;                  // tests_pass|build_pass|lint_pass|artifact_exists|custom|...
  string expr = 3;                  // 命令或路径
  bool required = 4;
}
message Budget { int64 tokens = 1; double cost = 2; int64 wall_clock_seconds = 3; }

message HarnessEvent {
  string event_id = 1;
  string event_type = 2;
  string schema_version = 3;
  string task_id = 4;
  string trace_id = 5;
  string span_id = 6;
  string stage = 7;
  int32 attempt = 8;
  int64 seq = 9;
  int64 timestamp_ms = 10;
  string severity = 11;
  string tenant_id = 12;
  string project_id = 13;
  string agent_id = 14;
  bytes payload = 15;               // JSON
  string payload_ref = 16;
}

message WatchEventsRequest {
  string tenant_id = 1;
  int64 since_seq = 2;              // 0 = 只推新事件
  repeated string task_ids = 3;     // 空 = 全部
}

// ===== 多 Agent 协作（2026-11-10 新增）=====
message SubtaskView {
  string subtask_id = 1;
  string parent_task_id = 2;
  string dag_id = 3;
  string dag_node_id = 4;
  int32 depth = 5;
  int32 index_in_parent = 6;
  string role = 7;
  string status = 8;                // pending|running|waiting_handoff|accepted|rejected|failed|cancelled
  repeated string depends_on = 9;
  int32 attempt_no = 10;
  repeated ArtifactRef artifact_refs = 11;
  string acceptance_id = 12;
  string handoff_id = 13;
  int64 consumed_tokens = 14;
  double consumed_cost = 15;
  int64 started_at_ms = 16;
  int64 ended_at_ms = 17;
}
message ArtifactRef { string path = 1; string hash = 2; int64 size = 3; }

message ListSubtasksRequest {
  string tenant_id = 1;
  string project_id = 2;
  string parent_task_id = 3;
  repeated string status = 4;
  int32 limit = 5;
  string page_token = 6;
}
message ListSubtasksResponse { repeated SubtaskView subtasks = 1; string next_page_token = 2; }

message GetDAGStatusRequest { string tenant_id = 1; string project_id = 2; string parent_task_id = 3; string dag_id = 4; }
message DAGStatusView {
  string dag_id = 1;
  int32 node_count = 2;
  int32 accepted = 3;
  int32 running = 4;
  int32 failed = 5;
  int32 pending = 6;
  repeated ParallelSetProgress parallel_sets = 7;
  repeated CircuitState circuits = 8;
}
message ParallelSetProgress { string set_id = 1; int32 total = 2; int32 done = 3; string failure_strategy = 4; }

message HandoffView {
  string handoff_id = 1;
  string parent_task_id = 2;
  string subtask_id = 3;
  string from_agent_id = 4;
  string to_agent_id = 5;
  string status = 6;                // created|accepted|rejected|expired
  bytes contract_json = 7;          // HandoffContract/v1
  string a2a_task_id = 8;
  int64 created_at_ms = 9;
}
message ListHandoffsRequest { string tenant_id = 1; string project_id = 2; string parent_task_id = 3; string status = 4; int32 limit = 5; }

message AggregationReport {
  string task_id = 1;
  string dag_id = 2;
  repeated AggregationItem items = 3;
  repeated Conflict conflicts = 4;
  int32 total = 5; int32 accepted = 6; int32 rejected = 7; int32 failed = 8; int32 cancelled = 9;
}

// ===== 模型选择（2026-11-10 新增）=====
message ModelEntry {
  string model_id = 1;
  string provider = 2;
  string kind = 3;                  // chat|embedding
  int32 context_window = 4;
  int32 max_output_tokens = 5;
  bool tool_call = 6;
  bool structured_output = 7;
  int32 embedding_dim = 8;          // kind=embedding 时 >0
  double price_in_per_1k = 9;
  double price_out_per_1k = 10;
  string status = 11;               // available|unavailable|deprecated
  repeated string tags = 12;
  // 注意：不含任何凭据字段（REQ-152）
}
message ListModelsRequest { string tenant_id = 1; bool include_unavailable = 2; }

message ResolveModelRequest {
  string tenant_id = 1;
  string project_id = 2;
  string stage = 3;
  bool requires_tool_call = 4;
  bool requires_structured_output = 5;
  int64 stage_budget_tokens = 6;
  string strategy = 7;              // cost_first|quality_first|balanced
  string explicit_model_id = 8;
}
message ResolveModelResponse {
  bool ok = 1;
  string effective_model_id = 2;
  string resolved_from = 3;
  string error_code = 4;            // E_MODEL_CAPABILITY_MISMATCH 等
  repeated ModelScore candidates = 5;
  repeated string suggested_models = 6;
  int32 context_window = 7;
  int64 threshold_tokens = 8;
}
message ModelScore { string model_id = 1; double score = 2; string rejected_reason = 3; }

// ===== 工具权限（2026-11-10 新增）=====
message PreflightToolRequest {
  string tenant_id = 1;
  string project_id = 2;
  string task_id = 3;
  string subtask_id = 4;
  string stage = 5;
  string role = 6;
  string tool_name = 7;
  repeated string permission_tags = 8;
  string requested_path = 9;
  repeated string argv = 10;        // exec 类工具的参数（用于白名单校验，不执行）
}
message ToolDecision {
  string effect = 1;                // allow|deny|require_approval
  string rule_id = 2;
  string layer = 3;                 // system|tenant|role|task
  string error_code = 4;            // E_TOOL_DENIED / E_PATH_ESCAPE / E_EXEC_DENIED ...
  string normalized_path = 5;
  string approval_id = 6;           // require_approval 时给出
  int64 duration_us = 7;
}
message ApprovalStatusView { string approval_id = 1; string status = 2; string decided_by = 3; int64 decided_at_ms = 4; }
```

`memory.proto` 追加：

```protobuf
message RecallForStageRequest {
  string tenant_id = 1;
  string project_id = 2;
  string stage = 3;
  string query = 4;
  int32 top_k = 5;
  int32 token_budget = 6;
}
message RecalledTopic {
  string topic_key = 1;
  string category = 2;
  string title = 3;
  string body = 4;
  string source_file = 5;
  int64 updated_at = 6;
  double score = 7;
  repeated string hit_sources = 8;  // fulltext|vector
}
message RecallForStageResponse { repeated RecalledTopic topics = 1; int64 latency_ms = 2; bool degraded = 3; }

message SyncFromMarkdownRequest {
  string tenant_id = 1;
  string project_id = 2;
  string scope = 3;            // global|project：决定 LRU / 软上限治理分支（2026-11-11 修订）
  string file = 4;
  repeated string changed_topic_keys = 5;
  int64 order_version = 6;     // project：LRU 顺序版本；global：写入序号版本（读取不递增，2026-11-11 修订）
  bool full_reparse = 7;
}
message SyncFromMarkdownResponse { int32 indexed = 1; int32 deleted = 2; int64 indexed_at = 3; }
```

## 6.3 Gateway REST 路由表（REQ-119）

| 方法 | 路径 | 说明 | 鉴权 |
|---|---|---|---|
| POST | `/api/v2/harness-loop/tasks` | 创建任务（Idempotency-Key） | tenant + token |
| GET | `/api/v2/harness-loop/tasks` | 列表（按状态/项目过滤） | tenant |
| GET | `/api/v2/harness-loop/tasks/:id` | 详情（阶段/attempt/健康度） | tenant |
| POST | `/api/v2/harness-loop/tasks/:id/pause` | 暂停 | tenant + token + 二次确认 |
| POST | `/api/v2/harness-loop/tasks/:id/resume` | 恢复 | tenant + token |
| POST | `/api/v2/harness-loop/tasks/:id/abort` | 中止（幂等） | tenant + token + 二次确认 |
| GET | `/api/v2/harness-loop/tasks/:id/events` | 事件查询（stage/type/severity/since_seq） | tenant |
| GET | `/api/v2/harness-loop/tasks/:id/checkpoints` | 检查点列表 | tenant |
| POST | `/api/v2/harness-loop/tasks/:id/checkpoints/:cid/restore` | 从检查点恢复 | tenant + token |
| GET | `/api/v2/harness-loop/tasks/:id/report` | 终止报告（markdown/json） | tenant |
| GET | `/api/v2/harness-loop/monitor/stream` | SSE 全局事件流 | tenant |
| GET | `/api/v2/harness-loop/monitor/summary` | 聚合统计 | tenant |
| GET | `/api/v2/harness-loop/memory/topics` | 记忆主题列表 | tenant + project |
| PUT | `/api/v2/harness-loop/memory/topics/:key` | 写入/更新主题 | tenant + token |
| DELETE | `/api/v2/harness-loop/memory/topics/:key` | 删除主题（审计） | tenant + token + 二次确认 |
| GET | `/api/v2/harness-loop/memory/search` | 记忆检索（调试入口） | tenant |
| POST | `/api/v2/harness-loop/index/search` | 代码检索 | tenant + project |
| GET | `/api/v2/harness-loop/index/status` | 索引新鲜度/队列/失败列表 | tenant + project |
| POST | `/api/v2/harness-loop/index/rebuild` | 全量重建（管理员） | tenant + role |
| GET | `/api/v2/harness-loop/tasks/:id/subtasks` | 子任务列表（status 过滤、分页） | tenant + project |
| GET | `/api/v2/harness-loop/tasks/:id/subtasks/:sid` | 子任务详情（含 attempt、artifacts） | tenant + project |
| POST | `/api/v2/harness-loop/tasks/:id/subtasks/:sid/cancel` | 取消子任务（审计） | tenant + token |
| GET | `/api/v2/harness-loop/tasks/:id/dag` | 当前 active DAG 状态（节点 + 并行集进度） | tenant + project |
| GET | `/api/v2/harness-loop/tasks/:id/dag/:dagId` | 指定 DAG 快照（含 superseded） | tenant + project |
| POST | `/api/v2/harness-loop/tasks/:id/circuit/reset` | 复位熔断（审计，需二次确认） | tenant + token + admin |
| GET | `/api/v2/harness-loop/tasks/:id/handoffs` | handoff 列表 | tenant + project |
| GET | `/api/v2/harness-loop/handoffs/:hid` | handoff 详情（契约 + 证据字段） | tenant |
| GET | `/api/v2/harness-loop/tasks/:id/aggregation` | d 阶段汇聚报告 | tenant + project |
| GET | `/api/v2/harness-loop/models` | 模型清单（**已剔除凭据字段**） | tenant |
| POST | `/api/v2/harness-loop/models/resolve` | 能力匹配预检 + 候选打分（不创建任务） | tenant |
| POST | `/api/v2/harness-loop/tasks/:id/model-switch` | 人工审批的模型升级（REQ-147 例外之一） | tenant + token + 审批 |
| POST | `/api/v2/harness-loop/policy/preflight` | 工具权限预检（不执行工具） | tenant + project |
| GET | `/api/v2/harness-loop/policy/snapshot?task=:id` | 该任务冻结的策略快照（脱敏） | tenant |
| GET | `/api/v2/harness-loop/approvals/:aid` | 审批状态（薄代理 agent-service） | tenant + token |

**租户严格模式**：新增路由组挂 `middleware.StrictTenant()`——缺失 `X-Tenant-ID` 直接 401，而非默认 `"default"`。存量 `/api/v2/*` 保持现状（DFX-C-01），通过配置 `gateway.strict_tenant_paths=/harness-loop` 只对新增命名空间生效。

**错误码**（统一 envelope `{code,message,details,trace_id}`）：

| code | HTTP | 场景 |
|---|---|---|
| `E_INVALID_STAGE` | 400 | 非法阶段值 |
| `E_ILLEGAL_TRANSITION` | 409 | 转移不在白名单 |
| `E_ACCEPTANCE_CONTEXT_INCOMPLETE` | 422 | 注入缺项 |
| `E_CHECKPOINT_NOT_FOUND` | 404 | 检查点不存在/已清理 |
| `E_CHECKPOINT_CORRUPTED` | 409 | 校验失败 |
| `E_FSM_DISABLED` | 503 | 开关关闭 |
| `E_INDEX_DISABLED` | 503 | 索引关闭 |
| `E_INDEX_UNAVAILABLE` | 503 | PG 不可用 |
| `E_QUOTA_EXCEEDED` | 402 | 额度不足 |
| `E_MEMORY_WRITE_BUSY` | 409 | 文件被占用 |
| `E_KB_FORBIDDEN` | 403 | 越权知识库 |
| `E_SUBTASK_LIMIT_EXCEEDED` | 422 | 直接子任务数超过 20（REQ-127） |
| `E_SUBTASK_DEPTH_EXCEEDED` | 422 | 拆分深度超过 2 层（REQ-127） |
| `E_SUBTASK_NOT_SPLITTABLE` | 422 | 命中不可拆条件（REQ-126） |
| `E_SUBTASK_ACCEPTANCE_REQUIRED` | 409 | 未通过子任务级验收却置 accepted（REQ-124/141） |
| `E_DAG_INCOMPLETE` / `E_DAG_CYCLE` / `E_DAG_PARALLEL_CONFLICT` | 422 | DAG 缺依赖/成环/并行集冲突（REQ-125） |
| `E_CONTRACT_INCOMPLETE` | 422 | 输入/输出契约缺失（REQ-125） |
| `E_PROPAGATION_UNDECLARED` | 422 | 并行集未声明失败传播策略（REQ-134） |
| `E_HANDOFF_INCOMPLETE` | 422 | 契约缺五要素或未完成项（REQ-131） |
| `E_CROSS_TENANT_DENIED` | 403 | 跨租户/跨项目 handoff 或子任务（REQ-137） |
| `E_AGGREGATION_MISSING` / `E_AGGREGATION_INCOMPLETE` | 409 | 缺汇聚报告或校验不全，d 阶段不得启动（REQ-133） |
| `E_AGENT_ROLE_UNDECLARED` | 422 | 计划引用未声明角色/工具集的 Agent（REQ-135） |
| `E_MODEL_META_INCOMPLETE` | 422 | 注册项缺窗口/工具调用标识/单价（REQ-142） |
| `E_MODEL_CAPABILITY_MISMATCH` | 422 | 能力/窗口不满足，附 `suggested_models`（REQ-146） |
| `E_MODEL_SWITCH_FORBIDDEN` | 409 | 运行期非法换模型（REQ-147） |
| `E_MODEL_UNAVAILABLE` | 503 | 主模型与回退列表均不可用（REQ-148） |
| `E_EMBEDDING_DIM_MISMATCH` | 422 | 向量维度与索引库不一致（REQ-151） |
| `E_TOOL_TAG_MISSING` | 422 | 工具未声明权限标签（REQ-156） |
| `E_TOOL_NOT_ALLOWED` | 403 | 不在任何白名单（deny-by-default，REQ-157） |
| `E_TOOL_DENIED` | 403 | 命中黑名单/系统硬限制（REQ-157/165） |
| `E_TOOL_NOT_PERMITTED` | 403 | 角色未声明该工具（REQ-136） |
| `E_STAGE_TOOL_FORBIDDEN` | 403 | 阶段权限矩阵禁止（REQ-161） |
| `E_PATH_OUT_OF_SANDBOX` / `E_PATH_ESCAPE` | 403 | 路径规范化后越界（REQ-159） |
| `E_WRITE_PATH_DENIED` | 403 | 写路径不在白名单（REQ-160） |
| `E_EXEC_NOT_ALLOWED` / `E_EXEC_DENIED` | 403 | 命令不在白名单或含复合结构（REQ-162） |
| `E_APPROVAL_REJECTED` / `E_APPROVAL_TIMEOUT` | 403 | 审批拒绝/超时，不得放行（REQ-163） |
| `E_POLICY_SELF_MODIFY` | 403 | 尝试修改策略/白名单（REQ-168） |
| `E_INVALID_WORKSPACE` | 422 | workspace_root 非绝对路径或越出授权根（REQ-158） |

> **错误响应统一结构（2026-11-10 修订）**：所有权限类错误必须携带 `details.rule_id`、`details.layer`、`details.tool`、`details.stage`、`details.requested_path`（或 `args_summary`）——这是 REQ-164 的硬性要求，Gateway 层不得裁剪这些字段。

## 6.4 向后兼容策略

| 变更 | 策略 |
|---|---|
| proto 新增字段 | 只追加，字段号跳跃预留；`reserved` 标注废弃字段 |
| 枚举扩展 | 只追加新值，客户端必须容忍未知值（`*_UNSPECIFIED` 兜底） |
| 事件 schema | `schema_version` 主版本变更需双写过渡；消费端按版本解析（DFX-M-04） |
| 检查点结构 | 新 collection；旧 `agent_checkpoints` 由旧代码继续读 |
| REST | 新命名空间；旧路由零改动 |
| CLI | `--json` envelope 增加字段兼容；主版本升级时提示 |

---

# 七、前端设计

## 7.1 路由与页面

`frontend/src/App.tsx` 追加（不修改既有路由）：

```tsx
<Route path="harness-loop/monitor" element={<HarnessLoopMonitor />} />
<Route path="harness-loop/tasks/:taskId" element={<HarnessLoopTaskDetail />} />
```

侧边栏（`frontend/src/components/Layout`）新增"任务循环"分组：监控大屏、任务列表、记忆、索引。

## 7.2 组件树

```
pages/HarnessLoop/
  index.tsx                     # 路由出口
  Monitor.tsx                   # 大屏容器（布局 + SSE 订阅）
  TaskDetail.tsx                # 单任务详情（事件时间线 + 检查点）
  components/
    SummaryBar.tsx              # 运行中/等待验收/重试中/失败/完成
    AgentCardGrid.tsx           # 50 卡片虚拟滚动
    AgentCard.tsx               # 单卡片（阶段/attempt/健康度/最近 verdict）
    LifecycleSwimlane.tsx       # 泳道图（ECharts custom series）
    HealthGauge.tsx             # 健康度仪表（ECharts gauge）
    StageDurationChart.tsx      # 阶段时长分布（ECharts bar）
    TerminalStream.tsx          # 终端输出（虚拟列表，最多 500 行）
    ArtifactPanel.tsx           # 产物列表（路径/哈希/大小）
    AnomalyDrawer.tsx           # 异常定位抽屉（trace 聚合）
    ControlBar.tsx              # 暂停/恢复/中止（二次确认）
    SubagentTree.tsx            # 新增（2026-11-10）：父子任务树（缩进 2 层，可折叠，显示 role/status/attempt/耗时）
    DagProgressBar.tsx          # 新增：DAG 进度条（accepted/running/failed/cancelled 分段着色）
    DagTimeline.tsx             # 新增：并行集甘特图（ECharts custom series，横轴时间）
    HandoffGraph.tsx            # 新增：handoff 有向小图（A→B，点击看契约摘要，不展示原始对话）
    ModelStageTable.tsx         # 新增：每阶段生效模型 / 回退次数 / token / 成本
    ModelDistributionChart.tsx  # 新增：模型分布（按调用次数）+ 回退趋势（1h）
    PolicyDenyDrawer.tsx        # 新增：权限拒绝明细（rule_id / layer / 参数摘要），从 tool.denied 打开
    ApprovalInline.tsx          # 新增：审批内联提示（复用现有 approval 事件与 /approvals 接口）
  hooks/
    useEventStream.ts           # SSE + 重连 + 补拉
    useTaskEvents.ts            # 按任务拉取 + 归约
    useMonitorSummary.ts        # TanStack Query 轮询兜底（10s）
  api/harnessLoop.ts            # axios 封装（复用 api/client.ts）
  types.ts                      # 与 proto 对齐的 TS 类型
```

## 7.3 状态管理

| 状态 | 方案 | 说明 |
|---|---|---|
| 服务端数据 | TanStack Query | `queryKey: ['hlp','tasks',filters]`、`['hlp','events',taskId,seq]`、`['hlp','summary']`；`staleTime=5s` |
| 实时事件 | `useEventStream` + 本地 reducer | 用 `useSyncExternalStore` 维护"任务 → 最新状态"，避免全局 store 频繁写 |
| UI 状态 | Zustand | 抽屉开关、筛选、选中任务；与现有 `stores/` 风格一致 |
| 图表实例 | ref + `echarts.init` | 复用现有 `CostDashboard.tsx` 的封装方式；`resize` 监听 |

**归约函数**（与后端同一套语义）：`reduceTaskState(state, event)` 在 `types.ts` 中实现，单测覆盖全部事件类型；事件乱序按 `seq` 排序后归约，重复 `event_id` 跳过。

## 7.4 ECharts 图表与数据源

| 图表 | 类型 | 数据源 | 刷新 |
|---|---|---|---|
| 生命周期泳道 | `custom` series（a/b/c/d/e 五列，attempt 色块） | 事件归约 | 事件驱动 |
| 健康度仪表 | `gauge` | `health` 字段 | 事件驱动 / 10s |
| 阶段时长分布 | `bar`（横向） | `stage.exited` 的 duration | 10s |
| 验收通过率 | `line`（按小时） | `acceptance.*` 聚合 | 30s |
| 重试热力 | `heatmap`（阶段 × 异常分类） | `retry.*` / `error` | 30s |
| 异常趋势 | `line` + `markPoint` | `error` 按分类 | 10s |
| DAG 进度条（2026-11-10 新增） | 自定义 SVG/`bar` 堆叠 | `subtask.*` 归约（accepted/running/failed/cancelled 计数） | 事件驱动 |
| 并行集甘特（2026-11-10 新增） | `custom` series（时间 × 子任务行） | `subtask.started/ended` 的 `parallel_set_id` | 事件驱动 |
| 模型分布（2026-11-10 新增） | `bar`（按调用次数 Top5） | `model.*` 聚合 / `GET /models` | 30s |
| 回退趋势（2026-11-10 新增） | `line`（1h，按 `trigger` 分组） | `model.fallback` 计数 | 30s |
| 权限拒绝分布（2026-11-10 新增） | `bar`（rule_id × 次数，堆叠 allow/deny） | 审计 `tool_decision` 聚合 | 30s |

主题：从现有 `styles/globals.css` 的 CSS 变量派生（用户偏好 CSS 变量 token 体系），不新增自定义 CSS 文件；ECharts `color` 取 3 个主色 + 中性灰阶，禁止高饱和多色。

## 7.5 实时通道

```
useEventStream(tenant, lastSeq):
  - EventSource(`/api/v2/harness-loop/monitor/stream?last_seq=${lastSeq}`)
  - onmessage: 解析 → 按 seq 插入 buffer（200ms 合并 flush）
  - onerror: 指数退避重连（1s/2s/4s，上限 30s），带 last_seq
  - 重连后若发现 seq 跳号 → 调用 REST 补拉缺口
  - 页面隐藏时降频（visibilitychange），避免后台占资源
```

降级：EventSource 不可用（代理不支持）→ 退化为 5s 轮询 `summary` + `events?since_seq`，UI 标注"实时不可用"。

## 7.6 视觉与交互要点

1. **克制科技风**：深灰底（`#0F1115`）+ 面板 `#161A20` + 单一强调色（青绿 `#2BD4A8`），警示色仅用于 error/warn；禁用镭射渐变、霓虹描边。
2. **信息密度**：卡片只放 5 个指标（阶段、attempt、健康度、最近 verdict、耗时），其余进抽屉。
3. **文案去 AI 味**：使用工程语义（"阶段推进""验收未通过""重试 2/3"），避免"智能体协同""AI 赋能"。
4. **可访问性**：健康度不只靠颜色，附数字与文本标签；键盘可切换卡片（方向键 + Enter 打开抽屉）。
5. **操作安全**：暂停/中止/重建索引需二次确认（输入任务 ID 后 4 位或勾选），操作结果以事件回执展示。
6. **空态与错误态**：无任务时展示"如何创建任务"（CLI 命令 + API 示例）；SSE 断开展示明确原因与重连按钮。
7. **权限拒绝的呈现（2026-11-10 新增）**：`tool.denied` 不在终端流里堆红字淹没输出；折叠为一行"`write_file` 被拒（sandbox.write_allowlist）"，点击展开 rule_id / layer / 参数摘要。**不得**提供"忽略此次拒绝继续"的按钮——拒绝是硬约束。
8. **模型信息的克制展示（2026-11-10 新增）**：卡片上只显示生效模型短名（如 `qwen-plus`）+ 一个回退角标；完整候选打分放详情页。不使用"智能选择""AI 优选"等文案，用"生效模型 / 回退 1 次"。
9. **DAG 视图的可读性（2026-11-10 新增）**：默认折叠，展开只显示 2 层；并行集用同一色系的深浅区分，不使用彩虹色；失败节点用警示色 + 文本标签"失败（超时）"而非仅变红。

---
# 八、代码规划

## 8.1 目标目录结构

```
agent-platform/
├── proto/
│   ├── harnessloop/harnessloop.proto        [新增]
│   ├── codeindex/codeindex.proto            [新增]
│   ├── memory/memory.proto                  [改造：追加 2 RPC + 3 message]
│   └── knowledge/knowledge.proto            [改造：追加 kb_id/state/sources]
├── pkg/
│   ├── harnessloop/                         [新增]
│   │   ├── fsm/{machine.go,transitions.go,attempt.go,terminal.go,guard.go}
│   │   ├── acceptance/{verifier.go,context.go,schema.go,rollback.go,audit.go}
│   │   ├── acceptance/checks/{registry.go,go_test.go,go_build.go,go_vet.go,lint.go,file_exists.go,cmd_exit.go,symbol.go}
│   │   ├── retry/{classifier.go,policy.go,backoff.go,budget.go,finish_reason.go}
│   │   ├── checkpoint/{store.go,fsm_snapshot.go,restore.go,drift.go,retention.go}
│   │   ├── compaction/{trigger.go,template.go[唯一通道],engine.go[预留扩展点,无实现],jsonl.go,pairing.go,abtest.go}   [2026-11-10 修订：删 merge.go]
│   │   ├── orchestrator/{planner.go,dag.go,scheduler.go,quota.go,fairness.go,circuit.go,aggregate.go,propagation.go}  [新增 4.13]
│   │   ├── subtask/{model.go,state.go,registry.go,repo.go}                                                          [新增 4.13.1]
│   │   ├── handoff/{contract.go,validate.go,transfer.go,record.go}                                                   [新增 4.13.4]
│   │   ├── modelregistry/{registry.go,selector.go,capability.go,fallback.go,hotreload.go,secret.go}                  [新增 4.14]
│   │   ├── policy/{engine.go,resolver.go,matrix.go,registry.go,cache.go,audit.go}                                  [新增 4.15]
│   │   ├── policy/sandbox/{realpath.go,pathguard.go,execguard.go,escape.go}                                          [新增 4.15.3/4.15.5]
│   │   ├── event/{schema.go,types.go,seq.go,publisher.go,idempotency.go,health.go}
│   │   └── (Go 包内不含 yaml；配置文件统一放 config/harness_loop/，见下方 config/ 段) [2026-11-10 修订：原 policy/*.yaml 归类错误]
│   ├── config/harness_loop/                 [新增：配置目录，运行期只读快照]
│   │   ├── {retry.yaml,models.yaml,health.yaml,criteria_templates.yaml,compaction.yaml}
│   │   ├── {stage_matrix.yaml,stage_allow.yaml,stage_deny.yaml,exec_allow.yaml,approval.yaml,subtask.yaml}  [新增 4.13/4.15]
│   ├── memoryfile/                          [新增]
│   │   ├── {layout.go,format.go,parse.go,render.go,lru.go,capacity.go,writer.go,recover.go,events.go}   [capacity.go 2026-11-11 新增]
│   │   ├── lock_unix.go / lock_windows.go
│   │   └── {parse_test.go,render_test.go,lru_test.go,capacity_test.go,atomic_test.go,recover_test.go}
│   ├── pgindex/                             [新增]
│   │   ├── {conn.go,migrate.go,advisory.go,pool.go}
│   │   └── ddl/{001_mem.sql,002_code.sql,003_event.sql,004_task.sql}
│   ├── codeindex/                           [新增]
│   │   ├── {watcher.go,debounce.go,hash.go,chunk.go,writer.go,queue.go,metrics.go,ignore.go}
│   │   ├── parser/{parser.go,go_ast.go,treesitter_cgo.go,treesitter_stub.go,regex.go,lang.go}
│   │   └── {debounce_test.go,writer_test.go,parser_test.go,incremental_test.go}
│   ├── rrf/{fuse.go,dedup.go,stable_sort.go,fuse_test.go}     [新增]
│   └── harnessclient/{client.go,stream.go}                     [新增]
├── services/
│   ├── harnessloop-service/                 [新增]
│   │   ├── cmd/main.go
│   │   ├── internal/
│   │   │   ├── handler/{grpc_handler.go,event_stream.go}
│   │   │   ├── service/{runtime.go,stage_runner.go,memory_service.go,index_service.go}
│   │   │   ├── repository/{task_repo.go,attempt_repo.go,acceptance_repo.go,event_repo.go,checkpoint_repo.go}
│   │   │   ├── watchdog/watchdog.go
│   │   │   └── config/config.go
│   │   ├── Dockerfile
│   │   └── config.example.yaml
│   ├── memory-service/internal/
│   │   ├── service/{memory_service_with_forgetting.go [改],mdsync.go [新]}
│   │   ├── pgindex/{memory_writer.go,recall.go,tokenizer.go}  [新增]
│   │   └── handler/grpc_handler.go                             [改造]
│   ├── knowledge-service/internal/
│   │   ├── indexing/worker.go                                  [新增]
│   │   ├── rerank/{reranker.go,llm_reranker.go,http_reranker.go,noop.go} [新增]
│   │   ├── repository/document_repository.go                   [改造]
│   │   ├── search/bm25.go                                      [改造]
│   │   └── service/knowledge_service.go                        [改造]
│   ├── gateway/internal/
│   │   ├── router/router.go                                    [改造]
│   │   ├── handler/harness_loop_handler.go                     [新增]
│   │   └── middleware/middleware.go                            [改造：StrictTenant]
│   └── agent-service/internal/service/agent_service.go         [小改：stage_mode 透传]
├── cmd/harness-cli/main.go                                     [新增]
├── internal/cli/
│   ├── cmd/{root.go,init.go,run.go,resume.go,status.go,logs.go,abort.go,memory.go,index.go,doctor.go}
│   ├── tui/{app.go,model.go,theme.go,views/{stage.go,stream.go,events.go,help.go}}
│   └── output/{json.go,exitcode.go,redact.go}
├── frontend/src/pages/HarnessLoop/                              [新增]
├── docker/postgres/{init.sql,Dockerfile}                        [新增]
└── bench/repo-500/                                              [新增：基准仓库快照 + 脚本]
```

## 8.2 模块依赖规则

```mermaid
flowchart LR
  CLI["cmd/harness-cli"] --> HC["pkg/harnessclient"] --> PB["pkg/pb"]
  HLS["harnessloop-service"] --> HL["pkg/harnessloop"]
  HLS --> MF["pkg/memoryfile"]
  HLS --> CI["pkg/codeindex"]
  HLS --> PG["pkg/pgindex"]
  HLS --> HC
  HL --> LLM["pkg/llm"]
  HL --> RRF["pkg/rrf"]
  MS["memory-service"] --> PG
  MS --> RRF
  MS --> LLM
  KS["knowledge-service"] --> RRF
  KS --> LLM
```

强制规则（由 `depcheck` 单测校验）：
1. `pkg/harnessloop/*` 不得 import `services/*`（纯库）。
2. `pkg/memoryfile` 不得 import `pkg/llm`（文件层不调模型）。
3. `pkg/rrf` 不得 import 任何 DB 客户端。
4. `services/*` 之间只允许 gRPC，不允许直接 import 对方 internal。
5. 事件 schema 定义只在 `pkg/harnessloop/event`，前端 `types.ts` 由生成脚本同步（`scripts/gen-event-types.sh`）。
6. **（2026-11-10 新增）** `pkg/harnessloop/orchestrator` 不得 import `services/agent-service` 或 `services/a2a-service` 的 internal——Agent 注册/工具集/传输一律走 gRPC（`pkg/harnessclient`），这既是"复用而非重建"的可核查约束（REQ-140），也避免编排层与执行层编译期耦合。
7. **（新增）** `pkg/harnessloop/policy` 不得 import `pkg/llm`（权限判定不应触发模型调用，避免"用模型判权限"的循环依赖与不可判定性）；`policy/sandbox` 不得 import 任何 DB 客户端。
8. **（新增）** `pkg/harnessloop/modelregistry` 不得 import `services/*`；`pkg/llm` 不得反向 import `modelregistry`（方向是 runtime 读注册表后把 model 名传给 `pkg/llm`）。
9. **（新增）** `pkg/harnessloop/compaction` 不得 import 任何外部压缩引擎包（C 方案纪律，9.4 有扫描用例）。

## 8.3 实施顺序与最小可交付切片

### P0：FSM 闭环（可独立演示，不依赖记忆/索引）

| # | 切片 | 交付物 | 验收标准 |
|---|---|---|---|
| P0-1 | FSM 内核 | `pkg/harnessloop/fsm` + 穷举单测 | 8 条合法边全通过（a→b；b→a,c；c→b,d；d→c,b,e）；非法边拒绝且状态不变；`e` 无出边 |
| P0-2 | 任务状态与 attempt | `hlp_task` DDL + `task_repo`/`attempt_repo` + 乐观锁 | 并发转移仅一次成功；attempt_no 连续 |
| P0-3 | 阶段内执行适配 | `stage_runner` 调 agent-service `ExecuteStream`，注入 `__stage_mode__` | 无工具调用不视为完成；finish 工具被识别 |
| P0-4 | 验收 Agent（程序检查优先） | `acceptance` + `checks/registry` + `go test`/`go build`/`file exists` | 测试失败直接 fail 且不调模型；环境缺失记 unverified |
| P0-5 | 回退与三类终态 | `rollback.go` + `terminal.go` | c fail→b；d fail→c/b 符合规则表；abort 幂等；耗尽→failed |
| P0-6 | 检查点与恢复 | `checkpoint`（Mongo）+ 幂等表 + drift 校验 | 杀进程后 30s 内恢复；不重复已过阶段；drift 有事件 |
| P0-7 | 事件与查询 | `hlp_event` DDL + publisher + `ListEvents` | seq 单调；重复 event_id 不重复计数；按 stage/type 可查 |
| P0-8 | 网关与 CLI 最小闭环 | `/harness-loop/tasks*` 路由 + CLI `run/status/abort/resume` | CLI 与 Web 同任务流转一致；`--json` 退出码正确 |
| P0-9 | 开关与回滚 | feature flag 求值 + `E_FSM_DISABLED` | 关闭后走现状；已建任务仍可查 |
| P0-10（2026-11-10 新增） | **权限与沙箱最小闭环** | `policy/{engine,resolver,matrix,registry}` + `policy/sandbox/{realpath,pathguard,execguard}` + `stage_matrix.yaml`/`exec_allow.yaml` + 结构化错误码 | a 阶段 write 被拒；`../`、绝对路径、符号链接逃逸在**执行前**拒绝（无副作用）；`go test ./...` 允许、`go test && curl` 拒绝；每次调用有 allow/deny 审计 + `rule_id`；校验 P95 ≤ 5ms(命中)/20ms(未命中) |
| P0-11（2026-11-10 新增） | **模型注册表与能力校验** | `modelregistry/{registry,capability,hotreload}` + `models.yaml` + `ResolveModel` RPC/REST + `E_MODEL_CAPABILITY_MISMATCH` | 注册项缺窗口/工具标识/单价被拒；为 c 阶段选非工具模型时返回错误 + `suggested_models`（全部满足条件）；4.5.1 阈值改为读注册表且代码内无硬编码窗口常量 |

P0 完成后即可演示：`harness-cli run` → 五阶段推进 → 失败回退 → 中止幂等 → 事件可查 → 越权工具被拒并留痕。

> **为什么权限与模型放在 P0（2026-11-10 决策）**：c 阶段一旦开跑就要写文件、执行命令、调用模型。若 P0 不带这两块，等于"先开门再装锁"，P0 演示本身就会产生不可控的工作区改动与无法解释的模型选择。P0 只做**最小可判定集**（默认拒绝 + 路径/命令白名单 + 阶段矩阵 + 注册表 + 能力校验），策略打分、回退链、审批联动、公平调度放 P1/P2。

### P1：记忆、压缩、索引、TUI、大屏

| # | 切片 | 交付物 | 验收标准 |
|---|---|---|---|
| P1-1 | markdown 事实源 | `pkg/memoryfile` + `harness-cli init/memory` | **项目 md**：31 主题淘汰第 1 个；读取提升到队首；中断不留半写文件；重复写哈希不变。**长期 md（2026-11-11 修订）**：51 主题不删除仅 `capacity_warning`；读取 10 次顺序/内容/哈希不变 |
| P1-2 | 记忆索引同步 | `mdsync` + `hlp_mem` + `SyncFromMarkdown` | 手工改 md 后 10s 内可检索；删除主题不再返回 |
| P1-3 | 记忆混合召回与注入 | `RecallForStage` + `pkg/rrf` + 预算 15% | 全文满足 top_k 不调 embedding；注入带 4 项元数据；PG 停机仍注入 ≥1 条 |
| P1-4 | 上下文压缩（**C 方案**，2026-11-10 修订） | `compaction` 单通道模板摘要 + JSONL + 配对校验 + A/B（模板版本）+ 三级降级 | 五要素齐全；工具配对不合法时拒绝并回退截断；压缩比可对比；**引擎位为空时不产生 `compaction.degraded`**；`engine_fragment` 不出现 |
| P1-5 | 异常分类与重试 | `retry` 全量 + no_finish 检测 + finish_reason 映射 | 8 类异常可注入并归类；quota 30s 内 failed；两类预算互不消耗 |
| P1-6 | 代码索引（Go） | `pkg/codeindex` + `hlp_code` + Go 解析 + 增量事务 | 单文件 P95 ≤ 2s；touch 不写；删除清理边；事务失败旧索引可用 |
| P1-7 | CLI TUI | `internal/cli/tui` | 阶段变化 1s 内反映；断线重连补齐；验收失败显示 evidence |
| P1-8 | 大屏（基础视图） | `pages/HarnessLoop` + SSE + 泳道/卡片/健康度 | 50 卡片 1s 刷新可交互；断线 10s 重连补齐；异常抽屉一跳可达 |
| P1-9 | 知识广场增强 | 状态机 + 真实 BM25 分数 + RRF 稳定排序 + 引用 | 失败文档不可检索；引用可定位页码；混合不低于单路 |
| P1-10（2026-11-10 新增） | **多 Agent 协作（核心）** | `orchestrator/{planner,dag,scheduler,quota,circuit,aggregate,propagation}` + `subtask/*` + `handoff/*` + 5.1.2/5.1.3 表 + 相关 RPC/REST | 3 子任务并行且父 `current_stage` 仍为 b/c；21 个子任务被拒（`E_SUBTASK_LIMIT_EXCEEDED`）；第 3 层被拒；未 accepted 产物汇入被拒；4 同类 3 失败触发熔断；汇聚报告缺失时 d 不启动 |
| P1-11（2026-11-10 新增） | **模型策略与回退** | `modelregistry/{selector,fallback}` + 三策略打分 + 阶段覆盖 + `hlp_task.model_selections` + `model.*` 事件/指标 | 三种策略结果可复现且可解释；单阶段回退 ≤ 3 次；回退计入预算；切换后压缩阈值重算；模型不可用 5 次后移出候选并可恢复 |
| P1-12（2026-11-10 新增） | **审批联动与权限审计聚合** | `policy` 命中 `requires_approval` → 复用 `pkg/agent/approval` + `GetApprovalStatus` 代理 + `audit(kind=tool_decision)` + 按 `rule_id` 聚合查询 | 部署类工具挂起并等待；拒绝返回 `E_APPROVAL_REJECTED` 且不重试；超时不放行；`tool.denied` 与审计的 `rule_id` 完全一致；可按 `rule_id` 出拒绝比例 |

### P2：多语言索引、rerank 增强、运维完善

| # | 切片 | 交付物 | 验收标准 |
|---|---|---|---|
| P2-1 | TS/Python 解析 | tree-sitter（cgo）+ 正则降级 + 能力标注 | 三语言样例符号/边与人工标注一致（≥90%） |
| P2-2 | 图扩展召回 | `code_edges` 1 跳 + RRF 第三路 | 查函数名返回调用方；结果含命中来源列表 |
| P2-3 | 全量索引与性能 | `index --full` + `CONCURRENTLY` + 压测报告 | 1000 文件全量可查；100 文件批量 P95 ≤ 60s |
| P2-4 | Reranker 增强 | HTTP cross-encoder 适配 + 降级标记 | 停用服务返回融合顺序 + `rerank=degraded` |
| P2-5 | 压缩引擎适配（**二期可选**，2026-11-10 修订） | 注册 `ContextEngine` 适配器 + 打开 `compaction.engine.enabled`（需 U16 决策通过） | 一期不实施；若实施：`engine_fragment` 产物标识出现、单通道失败时按 4.5.7 合并 reason、调用方与事件 schema 不变 |
| P2-6 | 知识索引异步化 | worker + 四态 + 重试 | 上传 pending → ready；failed 可重试且不进检索 |
| P2-7 | 审计与严格租户 | `audit` 表 + `StrictTenant` + 二次确认 | 越权 401；操作可审计且普通用户不可删 |
| P2-8 | 运维 | 指标/告警/trace 桥接/容量报告 | DFX-M-01 全指标暴露；trace_id 可串全链路 |
| P2-9（2026-11-10 新增） | **公平调度与熔断调优** | WFQ 权重可配 + 单任务配额硬上限 + 熔断参数调优 | 同租户 3 父任务并发时各任务 slot/token 不超配额；单任务无法独占；熔断恢复路径可用 |
| P2-10（2026-11-10 新增） | **多 Agent 高级场景** | 跨阶段 handoff（c→d 交接）+ 嵌套 DAG（depth=2）+ 重规划（新 dag_id） | 二层子任务正确展开且超限被拒；重规划保留旧 DAG 可查；跨阶段 handoff 契约五要素齐备 |

## 8.4 每阶段的总体验收门槛

| 阶段 | 必须全绿的测试集 | 必须留档的产物 |
|---|---|---|
| P0 | FSM 穷举、终态、中止幂等、检查点恢复、事件幂等；**权限沙箱负向集（`../`、绝对路径、符号链接、复合命令、自我提权）；模型能力校验集（2026-11-10 新增）** | 状态机转移矩阵测试报告、**权限矩阵测试报告、模型能力匹配测试报告** |
| P1 | 记忆（**项目 md LRU / 长期 md 软上限告警**，2026-11-11 修订）/原子写、压缩保真、索引增量、检索指标、CLI E2E；**多 Agent 并发/失败传播/handoff/汇聚；模型三策略与回退；权限审计一致性（2026-11-10 新增）** | Recall@K/MRR 基线、索引性能报告、压缩 A/B 报告、**DAG 并发与熔断报告、模型回退与成本报告、权限决策聚合报告** |
| P2 | 三语言解析准确率、知识检索评测、50 卡片压测、故障注入；**公平调度压测、沙箱逃逸模糊测试、嵌套 DAG（2026-11-10 新增）** | 压测报告、混沌演练记录、容量评估、**公平性报告（配额占用曲线）、逃逸模糊测试报告** |

---

# 九、测试与验证

## 9.1 测试分层

| 层级 | 范围 | 工具 | 门槛 |
|---|---|---|---|
| 单元 | FSM、**memoryfile 双轨容量（project LRU / global 软上限，2026-11-11 修订）**、分词、RRF、JSONL、回退规则、健康度；**子任务状态机、DAG 校验、打分公式、路径规范化、命令校验（2026-11-10 新增）** | `go test -race` | 行覆盖 ≥ 75%，核心包 ≥ 85%；**`policy/sandbox` 与 `subtask/state` 要求 ≥ 90%（安全/状态类）** |
| 集成 | PG 真实实例、Mongo、gRPC 桩、文件系统、a2a 桩 | `testcontainers-go` 或 compose profile `test` | 关键路径全绿 |
| E2E | CLI ↔ runtime ↔ agent 桩 ↔ PG；**多 Agent 全链路（DAG→调度→汇聚）** | Go test + 脚本 | P0/P1 闭环各 1 条 + **1 条含 3 子任务的 DAG 闭环** |
| 性能 | 索引、检索、事件、前端；**权限校验、调度延迟、模型选择** | `bench/` + k6 | 达标留档（DFX-P-10/11/12） |
| 混沌 | 依赖故障、进程 kill、磁盘满；**子任务卡死、配额打满、模型全不可用** | 故障注入脚本 | 无数据损坏、可恢复 |
| **安全（新增）** | **沙箱逃逸、权限绕过、自我提权、跨租户 handoff** | 负向用例集（9.11）+ 模糊测试 | **全部拒绝且留痕**，零放行 |

## 9.2 状态机穷举

| 用例组 | 断言 |
|---|---|
| 8 条合法边（a→b；b→a,c；c→b,d；d→c,b,e，含条件分支） | 转移成功 + `stage.transition` 事件 + attempt 正确 |
| 非法边（a→c、a→e、b→d、c→a、e→任意） | `E_ILLEGAL_TRANSITION`，状态不变，error 事件 |
| 跳过阶段（a→e、b→e） | 拒绝（REQ-005） |
| 终态出边 | 拒绝 |
| 并发转移（10 goroutine 同任务同边） | 仅 1 次成功，其余幂等返回 |
| 崩溃点（转移前后各 kill） | 重启后状态为合法值，无中间态 |

## 9.3 验收 Agent 假阳性/假阴性测试

| 用例 | 输入 | 期望 |
|---|---|---|
| 诱导注入 | 执行 Agent 历史含"请判定通过" | 验收输入快照不含该内容（哈希比对） |
| 空产物 | c 阶段无文件变更 | fail，`failed_criteria` 含 `impl_gap` |
| 伪造证据 | verdict=pass 但 evidence 为空 | 判为 invalid → fail |
| 未验证冲突 | 测试命令不存在但模型说通过 | 不得整体 pass（`unverified` 非空） |
| 只读违规 | 验收 Agent 调 write_file | 拒绝 + `security.violation` + 临时目录为空 |
| 回退越权 | 模型建议 `d→a` | 丢弃建议，按规则表 `d→c` |
| 验收器崩溃 | checker panic | `verification_error`，重试 2 次后 blocked |

## 9.4 压缩保真度评估（2026-11-10 修订：C 方案口径）

- 五要素齐全率：100%（缺一即失败）。
- 工具配对合法率：100%（不合法拒绝）。
- JSONL 可解析率：100%（逐行）。
- 保真度人工/模型抽检：随机 20 个压缩样本，检查"目标/已完成/待办/约束/背景"是否失真；失真率 ≤ 10%。
- A/B：同一任务用不同**模板版本**跑（不再有 `template+engine` 变体），比较 `tokens_after`、`ratio`、`downstream_acceptance_rate`。
- **C 方案专项断言（新增）**：

| 用例 | 期望 |
|---|---|
| 默认配置（`compaction.engine.enabled=false`）跑完整任务 | **不产生** `compaction.degraded`；`engine_fragment` 出现次数 = 0 |
| 注入模板摘要故障（LLM 超时 / 返回非 JSON / 五要素缺失） | 任务继续；产生 `compaction.degraded(reason=template_failed\|pairing\|missing_element)`；五要素占位块可读 |
| 模板摘要失败且截断后仍超阈值 | 进入 L2 紧急丢弃；`compaction.degraded(reason=emergency)`；tool_call/tool_result 配对仍完整 |
| 人为装配 mock `ContextEngine` 并打开开关 | 允许调用；失败时 reason 并入 L1；关闭开关后回到单通道（证明确为"预留"而非"隐藏接入"） |
| 代码扫描 | `pkg/harnessloop/compaction` 内不存在对任何开源压缩引擎的 import 或进程拉起 |

## 9.5 索引增量正确性

| 用例 | 期望 |
|---|---|
| touch 不改内容 | 不产生写事务（只更新 mtime 可接受，status 不变） |
| 修改单函数 | 该文件 chunks 全量替换，其他文件不变；能被语义检索命中 |
| 删除文件 | chunks/edges 清理，`indexed_files` 删除 |
| 删除被调用函数 | 指向它的边消失 |
| 事务中途失败 | 回滚；旧 chunks 仍可检索；status=failed + failure_count+1 |
| 并发 10 次同文件更新 | 最终状态唯一，边无重复 |
| debounce | 1s 内 5 次保存 → 仅 1 次索引任务 |
| 全量后增量 | 仅变更文件参与事务 |

## 9.6 检索指标

| 指标 | 定义 | 目标 |
|---|---|---|
| Recall@K | 相关结果在前 K 的比例（人工标注 50 query） | 代码 K=20 ≥ 0.75；记忆 K=5 ≥ 0.8 |
| MRR | 首个相关结果排名倒数均值 | 代码 ≥ 0.55；记忆 ≥ 0.65 |
| RRF 增益 | 混合 vs 单路 Recall@K 提升 | ≥ 5%（相对） |
| 延迟 | P95 | 代码 ≤ 500ms；记忆 ≤ 300ms；知识 ≤ 800ms |

A/B 方法：同一 query 集分别跑 `fulltext only`、`vector only`、`rrf`、`rrf+graph`，输出对比表并留档。

**记忆容量治理双轨验证（2026-11-11 新增，REQ-050/051/057）**

| 用例 | 操作 | 期望 |
|---|---|---|
| 长期 md 读取不变 | 对 `global.md` 任一主题连续读取 10 次 | 主题顺序、文件内容、`order_version`、哈希均不变；事件流无 `memory.read_promoted` |
| 长期 md 超软上限 | 依次写入 51 个不同主题 | 主题数为 51，无删除；出现 `memory.capacity_warning`；第 51 个位于文件末尾，整体顺序 = 写入顺序 |
| 长期 md pinned | 写入 `pinned=true` 主题并读取 | 文件位置不变；仅在注入排序中优先 |
| 项目 md LRU 保持 | 依次写入 31 个主题；再读取队尾主题 | 主题数 30，第 1 个被淘汰、第 31 个在队首；读取后该主题到队首；发 `evicted` / `read_promoted` |
| 项目 md pinned | 30 主题中 29 个 pinned 后新增 1 个 | 仅非 pinned 主题被淘汰，pinned 全部保留 |
| 升级兼容 | 用既有 `global.md`（含历史 `order`）启动新版本 | 不重排、不重置顺序、不产生一次性写盘；仅解析并同步索引 |

## 9.7 性能压测

| 场景 | 方法 | 目标 |
|---|---|---|
| 单文件索引 | 基准仓库 100 次单文件改 | P95 ≤ 2s / P99 ≤ 5s |
| 批量索引 | 100 文件同时改 | P95 ≤ 60s，检索不阻塞 |
| 代码检索 | 50 query × 10 轮 | P95 ≤ 500ms |
| 记忆召回 | 50 query | P95 ≤ 300ms |
| 验收 | 程序检查路径 20 次 | P95 ≤ 10s；含模型 ≤ 60s |
| 事件端到端 | 产生 → 大屏 | P95 ≤ 1s；断线 5s 补齐 |
| 大屏 | 50 卡片 × 1s × 10min | 主线程单帧 ≤ 16ms，无内存增长 |
| 进程内存 | 20 并发任务 | RSS ≤ 4GB；单任务 ≤ 256MB |

## 9.8 混沌与故障注入

| 故障 | 期望 |
|---|---|
| PG 停机 | 记忆降级 md；索引 API 503；事件缓冲后重试；任务不中断 |
| Mongo 停机 | 检查点保存失败 → 任务继续但标记 `checkpoint_degraded` 告警 |
| LLM 429/quota | 分类 quota → 快速 failed；429 非 quota → network 重试 |
| LLM 超时 | no_finish/timeout 分类，重试；不误判完成 |
| 进程 kill -9 | 重启恢复；不重复计费；事件续号 |
| 磁盘满 | 写入失败明确报错，不产生半写 md |
| 工作区被外部改动 | drift 事件 + 按策略处理 |
| 子任务进程级卡死（2026-11-10 新增） | 超时检测触发 `subtask.failed(category=timeout)`；父按传播策略推进，不静默忽略 |
| 并发额度被打满（2026-11-10 新增） | 新子任务排队 + `subtask.queued`；不失败、不抢占其他父任务配额 |
| 模型提供方整体不可用（2026-11-10 新增） | 回退链逐级尝试；全不可用 → 按 timeout/quota 分类，父任务不静默卡住 |
| 审批服务不可达（2026-11-10 新增） | 高危调用保持挂起并告警（**不放行**）；非高危调用不受影响 |

---

## 9.9 多 Agent 协作测试（2026-11-10 新增，REQ-123~141）

| 用例组 | 用例 | 期望 |
|---|---|---|
| 父子层级 | 父 b 阶段产出 3 子任务 | 父 `current_stage` 仍为 b；3 子任务各有独立 `subtask_id`、`attempt_no` 从 1 起、各自可恢复检查点 |
| 层级 | 任一子任务失败 | 父 `current_stage` 不变，仅汇聚视图回写 |
| 子任务状态机 | 强制把未验收子任务置 `accepted` | 拒绝 + `E_SUBTASK_ACCEPTANCE_REQUIRED`；DB `ck_subtask_accepted` 亦拒绝 |
| DAG 完整性 | 计划缺 `edges`（>1 节点） | b 验收 fail，`failed_criteria` 指出缺失 |
| DAG | 构造环 a→b→a | `E_DAG_CYCLE`，b 验收 fail |
| DAG | 有依赖的两节点放同一并行集 | `E_DAG_PARALLEL_CONFLICT` |
| 上限 | 21 个直接子任务 | `E_SUBTASK_LIMIT_EXCEEDED` + b 验收 fail |
| 上限 | 第 3 层拆分 | `E_SUBTASK_DEPTH_EXCEEDED` |
| 不可拆 | 含外部审批点的单元发起拆分 | `E_SUBTASK_NOT_SPLITTABLE` |
| 并发 | 可并行集 12 个节点 | 同时 `running` ≤ 8，其余 `pending` 且发 `subtask.queued` |
| 并发 | 租户已有 18 并发 Agent | 新子任务排队，不失败 |
| 超时 | 子任务超过 `min(父阶段剩余, 15min)` | `subtask.failed(category=timeout)`；父按传播策略推进 |
| 熔断 | 4 个同类子任务 3 失败 | 剩余同类不调度 + `subtask.circuit_open`；父可回退/终止 |
| 传播 | `fail-fast` 且 1 个失败 | 并行集其余 `cancelled`；父按 `d→c` 回退 |
| 传播 | `tolerate(0.2)` 且 5 中 1 失败 | 父继续；汇聚报告标注失败项 |
| 传播 | `retry` 重试耗尽 | 降级为 fail-fast |
| handoff | 契约缺未完成项 | `E_HANDOFF_INCOMPLETE`，交接被拒 |
| handoff | A→B 后检查 B 的输入快照 | 不含 A 的原始消息序列；`injected_from` 不含 `executor_history` |
| handoff | 尝试以共享会话方式读取 A 历史 | 拒绝 + `security.violation` |
| handoff | 跨租户 A→B | `E_CROSS_TENANT_DENIED` + 审计 |
| 汇聚 | 10 个子任务完成 | 报告含 10 项产物引用 + 哈希 + 结论；d 验收可读取 |
| 汇聚 | 缺汇聚报告即启动 d 验收 | `E_AGGREGATION_MISSING`，不启动 |
| 汇聚 | 未 accepted 产物计入汇聚 | 拒绝 + `security.violation` |
| 隔离 | 查询他租户父任务的子任务 | 返回空（不报错泄露存在性） |
| 预算 | 子任务消耗 | 计入父预算池，不存在独立账户（表结构无 `budget_*` 列） |
| 事件 | 按 `parent_task_id` 查询事件 | 可还原 DAG 执行顺序与并行区间；`handoff.*` 带 `handoff_id` |

## 9.10 模型选择测试（2026-11-10 新增，REQ-142~155）

| 用例组 | 用例 | 期望 |
|---|---|---|
| 注册表 | 登记缺 `context_window` 的模型 | `E_MODEL_META_INCOMPLETE` |
| 注册表 | 修改某模型单价 | 新任务按新单价；存量任务 `model_selections` 历史行不变 |
| 注册表 | 停用某模型 | 引用它的任务下一次调用前回退 + `model.switched` |
| 注册表 | 加载非法配置（`max_output > window`） | 整份快照拒绝，服务仍以旧快照运行 + 告警 |
| 策略 | 同一需求分别 cost_first / quality_first / balanced | 记录 `model_id` + `strategy` + `candidates[].score`，结果可复现（同输入同输出） |
| 策略 | 平局（差值 < 0.02） | 按 `reliability → window → id` 稳定排序 |
| 阶段覆盖 | 仅 d 配置强模型 | a/b/c/e 用任务级模型；d 用覆盖模型；每次调用事件含 stage + model |
| 能力匹配 | c 阶段选非工具调用模型 | `E_MODEL_CAPABILITY_MISMATCH` + `suggested_models` 全部满足条件；阶段不启动 |
| 能力匹配 | 窗口小于 system+goal+stage_budget | 同上，`violations` 含 `context_window_too_small` |
| 切换 | c 阶段中途请求换模型 | `E_MODEL_SWITCH_FORBIDDEN` |
| 切换 | 主模型限流触发回退 | 允许 + `model.switched(primary, fallback, reason)` |
| 切换 | 未经审批的模型升级 | 拒绝 + 审计 |
| 回退 | 单阶段连续 4 次回退 | 第 4 次不再发起，按异常分类处理 |
| 回退 | 回退调用 token/费用 | 计入任务预算池，不超上限 |
| 回退 | 回退列表为空 | 直接按 timeout/quota 处理，不挂起 |
| 可用性 | 连续 5 次失败 | `status=unavailable`，三策略均不再选中；恢复后 `model.recovered` 且重新可选 |
| 成本联动 | 32k → 128k 窗口切换 | `threshold_tokens` 重算且后续触发点后移；预算估算按新单价更新 |
| embedding | 768 维模型写入 1536 维索引 | `E_EMBEDDING_DIM_MISMATCH`，无部分写入 |
| 密钥 | 检查事件/日志/任务详情/`GET /models` | 无密钥明文；`api_key_ref` 不出现在响应 |
| 热更新 | 运行中改 d 阶段模型 | 当前调用用原模型完成；下一次用新配置；无报错 |

## 9.11 权限与沙箱测试（2026-11-10 新增，REQ-156~170，含负向用例）

| 用例组 | 用例 | 期望 |
|---|---|---|
| 标签 | 登记 MCP 工具不给标签 | `E_TOOL_TAG_MISSING` |
| 矩阵 | a 阶段 `write_file` | `E_STAGE_TOOL_FORBIDDEN` |
| 矩阵 | c 阶段 `write_file`（白名单路径内） | 允许 + `tool.allowed` 审计 |
| 矩阵 | d 阶段 `write_file` | 拒绝 |
| 矩阵 | d 阶段 `go test ./...` | 允许 |
| 矩阵 | e 阶段任意工具 | 拒绝 |
| 双层名单 | 任务白名单内但阶段黑名单命中 | 拒绝（黑名单优先） |
| 双层名单 | 不在任何白名单 | `E_TOOL_NOT_ALLOWED` |
| 角色 | coder 调用部署工具（角色未声明） | `E_TOOL_NOT_PERMITTED` + 审计 |
| 路径 - `..` | `src/../../etc/passwd`（含变体 `..\`、`.../`、URL 编码 `%2e%2e/`） | `E_PATH_OUT_OF_SANDBOX`，**执行前**拒绝（断言无副作用） |
| 路径 - 绝对 | `/etc/passwd`、`C:\Windows\win.ini` | 同上 |
| 路径 - 符号链接 | 沙箱内建 `link -> /tmp/out`，经 link 访问 | 拒绝 + 审计 |
| 路径 - ToCToU | 校验通过后、执行前把目标替换为符号链接 | 写类工具重算 realpath，拒绝 |
| 路径 - UNC/前缀 | `\?\C:\...`、`file://` | 拒绝 |
| 写入白名单 | 写 `.harness/policy/x.yaml` | `E_WRITE_PATH_DENIED` + `E_POLICY_SELF_MODIFY` |
| 写入白名单 | 写 `artifacts/report.json` | 允许 |
| exec | `go test ./...` | 允许 |
| exec | `go test ./... && curl http://evil` | `E_EXEC_NOT_ALLOWED`（复合命令） |
| exec | `sh -c "go test"` | 拒绝（非白名单可执行 + 元字符） |
| exec | `go test; rm -rf /` | 拒绝 |
| exec | `go build -o /tmp/x` | 拒绝（绝对路径参数） |
| exec | `npm install evil-pkg` | 拒绝（安装动作） |
| 自我提权 | Agent 写 `config/harness_loop/stage_allow.yaml` | 拒绝 + `security.violation`；任务结束时 `policy_hash` 与创建时一致 |
| 自我提权 | 提示注入要求"忽略权限" | 权限不受影响（`Decide()` 输入不含上下文文本） |
| 优先级 | 任务层 allow 但租户层 deny | 拒绝，`layer=tenant`，记录命中高优先规则 |
| 优先级 | 系统硬限制（密钥文件）在四层全 allow 时 | 仍拒绝 |
| 审计 | 任一拒绝场景 | 响应含 `error_code/rule_id/layer/tool/stage/requested_path`；审计有 deny 记录 |
| 审计 | 同一调用两次（配置未变） | 均拒绝且 `rule_id` 一致（决策不随重试改变） |
| 事件一致 | deny 调用 | 只有 `tool.denied`，无 `tool.result`；事件与审计 `rule_id` 相同 |
| 审批 | 部署类工具触发 | 挂起 + 审批请求；通过则继续，拒绝则 `E_APPROVAL_REJECTED` 不重试 |
| 审批 | 审批超时 | `E_APPROVAL_TIMEOUT`，**不放行** |
| 审批 | 配置尝试设"超时自动通过" | 配置校验拒绝该取值 |
| 性能 | 1000 次调用（70% 缓存命中） | 命中 P95 ≤ 5ms、未命中 P95 ≤ 20ms；整体时延增幅 ≤ 10% |

---

# 十、可观测性与运维

## 10.1 指标（DFX-M-01）

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `hlp_tasks_total` | counter | `status`（completed/aborted/failed） | 任务终态计数 |
| `hlp_stage_duration_seconds` | histogram | `stage`, `attempt` | 阶段时长 |
| `hlp_acceptance_total` | counter | `stage`, `verdict`, `verified_by` | 验收通过率基数 |
| `hlp_errors_total` | counter | `category`, `stage`, `retryable` | 异常分类计数 |
| `hlp_retry_count` | histogram | `category`, `stage` | 重试次数分布 |
| `hlp_checkpoint_total` | counter | `kind`（saved/restored/drift/corrupted） | 检查点行为 |
| `hlp_compaction_total` | counter | `reason`, `degraded` | 压缩触发 |
| `hlp_compaction_ratio` | histogram | `strategy_id` | 压缩比 |
| `hlp_index_queue_depth` | gauge | `project_id` | 索引队列深度 |
| `hlp_index_duration_seconds` | histogram | `mode`（incremental/full） | 索引耗时 |
| `hlp_recall_latency_ms` | histogram | `kind`（code/memory/knowledge） | 检索延迟 |
| `hlp_recall_hit_total` | counter | `kind`, `route`（fulltext/vector/graph） | 命中率 |
| `hlp_event_lag_seconds` | histogram | — | 事件产生到可查 |
| `hlp_inject_tokens_ratio` | gauge | `stage` | 注入 token 占比 |
| `hlp_subtasks_total`（2026-11-10 新增） | counter | `status`、`role` | 子任务终态计数 |
| `hlp_subtask_duration_seconds`（新增） | histogram | `role`、`parallel_set_id` | 子任务耗时 |
| `hlp_subtask_queue_depth`（新增） | gauge | `tenant_id` | 排队子任务数 |
| `hlp_subtask_circuit_open_total`（新增） | counter | `class_key` | 熔断次数 |
| `hlp_scheduler_alloc`（新增） | gauge | `parent_task_id`、`kind(slots\|tokens)` | 公平调度占用视图 |
| `hlp_handoff_total`（新增） | counter | `status`、`from_role`→`to_role` | 交接次数与成败 |
| `hlp_aggregation_conflict_total`（新增） | counter | `kind` | 汇聚冲突计数 |
| `hlp_model_calls_total`（新增） | counter | `stage`、`model` | 模型调用（见 4.14.9） |
| `hlp_model_tokens_total`（新增） | counter | `stage`、`model`、`direction` | token 消耗 |
| `hlp_model_cost_total`（新增） | counter | `tenant`、`project`、`stage`、`model` | 成本 |
| `hlp_model_fallback_total`（新增） | counter | `primary`、`fallback`、`trigger` | 回退次数 |
| `hlp_model_select_latency_ms`（新增） | histogram | — | 选择耗时（DFX-P-12） |
| `hlp_model_unavailable`（新增） | gauge | `model`、`provider` | 不可用模型数 |
| `hlp_policy_decisions_total`（新增） | counter | `decision`、`rule_id`、`layer` | 权限决策（REQ-166 聚合源） |
| `hlp_policy_decision_latency_ms`（新增） | histogram | `hit` | 校验时延（DFX-P-10） |
| `hlp_policy_cache_hit_ratio`（新增） | gauge | — | 决策缓存命中率 |
| `hlp_sandbox_escape_attempts_total`（新增） | counter | `kind(..\|abs\|symlink\|unc\|encode)` | 逃逸尝试次数 |
| `hlp_exec_denied_total`（新增） | counter | `reason` | exec 拒绝原因分布 |
| `hlp_approval_pending`（新增） | gauge | `priority` | 挂起审批数 |

暴露方式：沿用 `pkg/observability`（OTel + Prometheus exporter），在 `harnessloop-service` 的 `/metrics` 暴露；Gateway 聚合展示。

## 10.2 日志

结构化 JSON，必含：`trace_id`、`task_id`、`stage`、`attempt`、`event_type`、`severity`、`component`、`duration_ms`。

脱敏（`internal/cli/output/redact.go` 与后端共用规则）：
- `sk-[A-Za-z0-9]{8,}` → `sk-***`
- `Bearer <token>` → `Bearer ***`
- `password|secret|api_key` 字段值 → `***`
- 文件内容只记录路径 + 哈希 + 行数，禁止整段打印
- 提示词只记录 `prompt_version` + `input_digest`（REQ-020/DFX-M-02）

采样：`error` 全量；`info` 按 10%；`debug` 默认关闭（`HARNESS_DEBUG=1` 开启，仅本地）。

## 10.3 Trace

- runtime 为每个任务创建根 span（`task`），每阶段子 span（`stage`），验收独立子 span（`acceptance`，parent=stage），工具调用子 span（`tool`）。
- 验收 span 的 `messages` 与执行 span 无交集（REQ-014 验收条件），trace 可见 parent 关系。
- 通过 `services/harness-service/internal/trace` 上报（复用现有 Span 结构，追加 `stage`/`attempt`/`verdict` 到 `Attributes`）；OTel 同时导出到 collector。
- `trace_id` 贯穿：事件、日志、检查点、验收记录、CLI 输出（`--json` 含 `trace_id`）。
- **多 Agent span 层级（2026-11-10 新增）**：`task`（父根 span）→ `subtask`（每个子任务一个，`Attributes` 含 `subtask_id/dag_id/depth/role`）→ `tool`；`handoff` 作为 `subtask` 的子 span（`Attributes` 含 `handoff_id/from/to`）。
- **关联不共享上下文**：子任务 span 的 parent 是父 `task` span，但 span 间**不携带消息体**（避免把执行上下文写进 trace）。`aggregation` span 挂在父 `d` 阶段 span 下，`Attributes` 含 `items/conflicts` 计数。
- **模型与权限的 span 属性**：每次 LLM 调用在 `tool`/`stage` span 上追加 `model_id`、`is_fallback`；每次工具调用追加 `policy_decision`、`rule_id`（便于按 trace 还原"为什么被拒"）。

## 10.4 告警

| 告警 | 条件 | 级别 | 处置 |
|---|---|---|---|
| 验收失败率异常 | 10min 内 `failed/total > 0.6` 且样本 ≥ 10 | warn | 检查验收标准/模型 |
| 重试耗尽激增 | `retry.exhausted` > 5/10min | error | 看分类分布，判断依赖故障 |
| 事件滞后 | `event_lag P95 > 5s` | warn | 检查 PG 写入/Redis |
| 索引积压 | `queue_depth > 500` 持续 5min | warn | 扩容 indexer/降并发 |
| 索引失败 | 单项目 `failed_files > 20` | error | 看 last_error，重建 |
| 检查点异常 | `drift/corrupted > 0` | error | 排查工作区外部改动 |
| PG 不可用 | 连接失败 > 3 次 | error | 降级 + 通知 |
| 压缩降级率高 | `degraded/total > 0.3` | warn | 检查摘要模型/超时 |
| 记忆损坏 | `memory.corrupted` 任意 | error | 立即备份排查 |
| 长期记忆超过软上限（2026-11-11 新增） | `memory.capacity_warning` 任意 | warn | 提示用户手动清理（不自动删除）；展示主题数与文件大小（见 R25） |
| 子任务熔断（2026-11-10 新增） | `subtask.circuit_open` > 2/10min | warn | 看该 `class_key` 的失败原因与角色工具集 |
| 子任务排队积压（新增） | `hlp_subtask_queue_depth > 40` 持续 5min | warn | 检查租户配额与父任务数，考虑放宽 slot 或拆分计划 |
| 汇聚冲突（新增） | `aggregation.conflict` 任意 | warn | 检查计划是否把同路径改动的单元拆开（REQ-126） |
| 子任务超时比例（新增） | 10min 内 `subtask.failed(category=timeout)` / total > 0.3 | warn | 检查超时配置与模型响应 |
| 模型回退激增（新增） | `hlp_model_fallback_total` > 10/5min | warn | 查提供方状态；确认回退列表是否含可用候选 |
| 模型整体不可用（新增） | `hlp_model_unavailable >= 2` 且为 c/d 阶段主模型 | error | 切主模型或放宽回退链 |
| 能力不匹配拒绝（新增） | `model.capability_rejected` 任意 | error | 配置错误（阶段覆盖选了不满足能力的模型） |
| 权限拒绝激增（新增） | `hlp_policy_decisions_total{decision="deny"}` 占比 > 0.2 持续 10min | warn | 检查阶段矩阵/白名单是否与计划冲突（可能是计划在越权尝试） |
| 沙箱逃逸尝试（新增） | `hlp_sandbox_escape_attempts_total > 0` | **error** | 安全事件：立即核查 Agent 与提示词，必要时中止任务 |
| 自我提权尝试（新增） | `E_POLICY_SELF_MODIFY` 或 `policy_hash` 不一致 | **error** | 立即中止任务 + 审计追溯（REQ-168） |
| 审批积压（新增） | `hlp_approval_pending > 5` 持续 10min | warn | 通知审批人；考虑调整高危规则阈值 |
| 权限校验时延退化（新增） | `hlp_policy_decision_latency_ms` P95 > 20ms | warn | 检查规则数量与缓存命中率 |

## 10.5 部署与升级

1. 新增 compose 服务：`postgres`、`harnessloop-service`、可选 `codeindex`。
2. 启动顺序：postgres → harnessloop-service（自动迁移）→ codeindex → gateway。
3. 升级流程：
   - 先迁移（向后兼容，旧代码可读）；
   - 部署新服务（开关默认关）；
   - 灰度 `harness.loop.fsm.enabled`（tenant 白名单 → 10% → 全量）；
   - 观察 24h 指标；
   - 清理旧数据（仅当新路径稳定）。
4. 回滚：关开关 → 回滚镜像 → 保留 PG 数据（旧代码不读，不影响）→ 必要时执行 `down.sql`（仅开发环境）。
5. **配置目录（2026-11-10 新增）**：`config/harness_loop/` 以 ConfigMap/只读卷挂载；变更走"提交 → 评审 → 发布 → reload"，**不允许运行时由服务自写**；`policy/*.yaml` 的变更是安全相关变更，必须双人评审 + 审计（DFX-S-07）。
6. **权限规则灰度（新增）**：新规则先以 `shadow` 模式跑 24h，比对"若 enforce 会拒绝多少调用"（`hlp_policy_decisions_total{decision="deny"}` 在 shadow 下仍计数但不阻断），确认误伤率可接受后切 `enforce`。

## 10.6 容量与资源

| 资源 | 单实例建议 | 说明 |
|---|---|---|
| harnessloop-service | 4C/8G | 20 并发任务（单任务 ≤ 256MB，20 × 256MB = 5GB，留余量） |
| codeindex | 2C/4G | 解析 + embedding 外呼，内存主要给 AST |
| postgres | 8C/32G/200GB | 向量 + 事件 + 任务；`shared_buffers=8GB`、`maintenance_work_mem=2GB`（全量索引时） |
| redis | 1C/2G | 事件扇出 + 缓存 |
| mongo | 4C/8G | 检查点与文档 |

并发 20 任务时进程 RSS ≤ 4GB（DFX-P-09）：单任务内存预算 = 消息历史（≤ 50MB）+ 压缩缓冲（≤ 30MB）+ 检查点序列化（≤ 20MB）+ 运行时（≤ 50MB），其余为共享开销。

---
# 十一、风险与未决问题

## 11.1 风险清单

| ID | 风险 | 影响 | 可能性 | 缓解措施 | 负责阶段 |
|---|---|---|---|---|---|
| R1 | 双库并存导致职责漂移（代码 chunk 误写 Qdrant、知识 chunk 误写 PG） | 检索结果混乱、成本翻倍 | 中 | `vector_owner` 常量 + 单测断言 + 代码评审检查项 | P1 |
| R2 | md → PG 最终一致窗口内检索漏新主题 | 新写入的约束本轮不被注入 | 高 | 写入后同步调用（P95 ≤1s）+ "最近写入主题直读"兜底 + 大屏可见 `index_lag` | P1 |
| R3 | 用户手工编辑 md 与 Agent 更新冲突 | 主题内容被覆盖 | 中 | 文件锁 + `content_hash` 基线校验 + 冲突时以磁盘为准重放 + `memory.resync` | P1 |
| R4 | Windows 上 fsnotify 漏事件（编辑器原子保存） | 索引不新鲜 | 高 | 30s mtime 扫描兜底 + `index --paths` 手动补偿 + 状态查询暴露陈旧文件 | P1 |
| R5 | tree-sitter cgo 导致构建复杂/CI 失败 | TS/Python 索引不可用 | 中 | 构建标签隔离 + 无 cgo 降级正则 + 能力标注 `degraded` + CI 双构建 | P2 |
| R6 | embedding 在事务内导致长事务/锁等待 | 索引 P95 超标、阻塞更新 | 中 | `embedding_cache` 命中优先 + 批量 ≤64 + 3s 超时回滚 + 全量模式事务外预算 | P1 |
| R7 | 验收 Agent 假阳性（模型宽松）导致缺陷流出 | 质量风险 | 中 | 程序检查优先 + `unverified` 不得 pass + 证据必填 + 抽检 + 回退规则表裁决 | P0 |
| R8 | 验收 Agent 假阴性（过严）导致无限回退 | 成本与时长失控 | 中 | `acceptance.max_fails=5` + blocked 终态 + evidence 注入 + 人工介入入口 | P0 |
| R9 | 上下文压缩失真（丢关键约束） | 后续阶段重复犯错 | 中 | 五要素强校验 + 配对校验 + A/B 指标 + 失败回退截断 | P1 |
| R10 | LLM 调用成本上升（验收 + 压缩 + rerank + embedding） | 费用超预算 | 高 | 分离预算（验收/压缩/执行）+ 程序检查优先 + 按需向量 + rerank 超时降级 + 成本面板 | P1 |
| R11 | PG 成为新单点 | 记忆/索引/事件同时不可用 | 中 | 连接池 + 超时 + 降级（md grep）+ 事件本地缓冲 + 主从（P2 可选） | P1 |
| R12 | 事件高频写入拖慢 PG | 任务延迟 | 低 | 批量写（100 条/100ms）+ 分区 + 只索引必要列 + 采样 debug | P1 |
| R13 | 平台模式下工作区未正确挂载 | 记忆/索引路径错乱 | 中 | 启动自检（`doctor`）+ workspace_root 白名单 + 容器内路径校验 | P0 |
| R14 | 现有租户中间件"软鉴权"（默认 default） | 越权风险 | 高 | 新增命名空间强制 `StrictTenant` + 审计越权尝试；存量路径风险单列整改项 | P2 |
| R15 | 现有 `DeleteBySession` 缺陷被新代码复用 | 误删租户记忆 | 中 | 新索引层不复用该实现；补单测覆盖"空 sessionID 删除租户"语义 | P1 |
| R16 | CLI 与 runtime 版本不匹配（proto 演进） | 字段缺失/解析失败 | 中 | 版本握手（`version` RPC）+ 最低版本校验 + 明确错误提示 | P1 |
| R17 | 长期记忆被遗忘算法或误操作删除 | 用户长期约束丢失 | 低（2026-11-11 下调：长期 md 已明确不参与 LRU、不淘汰） | 长期 md 不参与 LRU、不淘汰（仅软上限 50 告警）；遗忘只降权不删除；git 版本化 | P1 |
| R18 | 知识 BM25 内存索引多副本不一致 | 检索结果不稳定 | 中 | 每副本独立构建 + 版本号 + 启动异步构建 + 就绪前降级 | P2 |
| **R19**（2026-11-10 新增） | **多 Agent 并发爆炸**：计划把任务拆成大量子任务，触发 LLM 调用与工具执行的乘数级放大 | 成本失控、PG/模型限流、任务时长超预期 | 中 | 三重闸门：数量 20 / 深度 2 / 并发 8（计划期 + 调度期双校验）；双配额（token + Agent 数）；熔断 50%/样本 4；大屏展示 DAG 规模与实时占用 | P1 |
| **R20**（新增） | **预算公平被破坏**：先提交的大任务长期吃满租户 slot，其他任务饿死 | 多租户体验劣化，小任务无法推进 | 中 | WFQ 虚拟时间调度 + 单任务 slot/token 硬上限（`parent_share`）+ 配额耗尽即排队不抢占；`hlp_scheduler_alloc` 可视化 | P1 / P2 调优 |
| **R21**（新增） | **模型切换语义漂移**：回退到弱模型后仍用原验收标准，导致假阴性（弱模型做不出被反复打回）或假阳性（弱模型自评宽松） | 任务抖动、成本与时长不确定 | 中 | 回退时记录 `model.switched` 并在验收上下文中标注"本次由回退模型产出"；单阶段回退上限 3 次；d 阶段验收优先 program 检查（不受模型影响）；验收阈值可按模型分级（配置化） | P1 |
| **R22**（新增） | **权限配置漂移**：阶段矩阵、角色白名单、任务白名单三处独立维护，出现"计划认为可做、运行时被拒"或反之 | 计划与执行不一致，反复失败 | 高 | 单一来源：角色白名单由 agent-service 提供、阶段矩阵与名单合并为 `policy_snapshot` 冻结；`doctor` 提供一致性自检；计划期用 `PreflightTool` 预检（提前暴露冲突） | P0 / P1 |
| **R23**（新增） | **handoff 契约退化**：五要素摘要失真，接收方重复已完成工作或漏做约束 | 重复劳动、产物不一致 | 中 | 五要素字段强校验 + `unfinished` 必填；接收方首次调用前做"契约理解确认"（结构化回执）；`handoff_id` 贯穿事件便于追因 | P1 |
| **R24**（新增） | **平台路径语义差异**：Windows 大小写不敏感、8.3 短名、junction/reparse point 使沙箱判定与 Linux 不一致 | 沙箱在 Windows 上被绕过（本地 CLI 模式高频） | 中 | 平台归一化（小写比较 + 分隔符统一 + 拒绝 UNC/`\?\`）；写类工具不缓存 realpath；CI 双平台跑逃逸测试集；U20 待确认 junction 处理策略 | P0 |
| **R25**（2026-11-11 新增） | **长期记忆无限增长**：软上限 50 只告警不删除，主题数与文件体积可持续增长 | 解析/同步/注入延迟上升，检索噪声增加 | 中 | `memory.capacity_warning` 告警 + 大屏/CLI 展示主题数与文件大小 + `harness-cli memory --prune` 由用户手动清理（不自动删除）；单主题正文上限 4000 字符；索引按主题分页同步 | P1 |

## 11.2 未决问题（需用户确认）

| ID | 问题 | 选项 | 建议 | 影响 |
|---|---|---|---|---|
| U1 | D1-C 的冲突处理是否符合预期：**保留 memory-service 内部实现与遗忘算法，仅改长期记忆写入路径并让遗忘只降权不删除** | A 认可；B 要求长期记忆写入路径也保持原样（则方案 A 不成立）；C 其他折中 | **A（已确认，2026-11-10）** | 决定 memory-service 改造范围与回滚设计 |
| U2 | 任务总超时默认值 | 2h / 8h / 24h / 不限 | 8h（长时任务但可控） | REQ-007 异常类停止、预算校验 |
| U3 | 长期记忆 md 的主题上限策略 | 严格 30 / **软上限 50 仅告警（不 LRU、不淘汰）** / 不限 | **已确认（2026-11-11）**：软上限 50 仅告警、不 LRU、不淘汰 | REQ-050 与"长期记忆不丢"的平衡 |
| U4 | reranker 实现 | 仅 LLM rerank / 增加 HTTP cross-encoder / 暂不启用 | P1 用 LLM，P2 评估 HTTP | 检索质量、成本、部署复杂度 |
| U5 | TS/Python 是否允许 cgo 构建 | 允许 / 仅正则降级 / 用独立解析服务 | 允许（隔离构建标签） | 索引精度与 CI 复杂度 |
| U6 | 事件保留期 | 7 天 / 30 天 / 90 天 | 30 天 | PG 容量、审计要求 |
| U7 | embedding 模型与维度是否固定 1024 | 固定 / 可配多维度 | 固定（与现有 Qdrant 一致） | 表结构 `vector(1024)` |
| U8 | 平台模式是否需要 KB 细粒度 ACL（非仅 tenant+ kb） | 需要 / 不需要（第一期） | 不需要 | 知识广场隔离设计 |
| U9 | CLI 是否需要支持 Windows 本机 runtime（含 fsnotify/文件锁） | 需要 / 仅 Linux/macOS | 需要（当前开发环境为 Windows） | 文件锁与 watcher 实现、测试矩阵 |
| U10 | 是否接受"项目 md 放在项目目录 `.harness/`"（会出现在 `git status`） | 接受 / 外置到 `~/.harness/projects/<id>/` | 接受（便于 git 版本化） | 目录布局与 `.gitignore` 策略 |
| U11 | 现有 `/api/v2/*` 的"软租户"是否在本次一并收紧 | 一并收紧 / 仅新增命名空间 | 仅新增命名空间（避免破坏存量） | DFX-S-01 达标范围 |
| U12 | 大屏是否需要多租户切换与权限分级 | 需要 / 不需要（仅运维单租户视图） | 需要只读 + 运维角色 | 前端路由与 Gateway 鉴权 |
| **U13**（2026-11-10 新增，对应 spec **Q13**） | 子任务并发默认上限与父任务预算向子任务的拆分比例 | A 8/20/20/2 + 并行集均分（当前默认）；B 更保守（4 并行）；C 更激进（16 并行） | **A（已确认，2026-11-10）** | 决定调度默认值与压测基线 |
| **U14**（新增，对应 **Q14**） | 回退模型清单粒度（全局 / 按阶段）与回退触发阈值 | A 每阶段 2 候选 + 限流或连续 3 次超时触发（当前默认）；B 全局单一回退；C 每阶段 3 候选 | A | 可用性保障与成本波动 |
| **U15**（新增，对应 **Q15**） | 租户策略能否覆盖阶段权限矩阵 | A 只可收紧不可放宽，放宽需平台审批（当前默认）；B 租户可完全自定义 | A | 安全边界与多租户灵活性 |
| **U16**（新增，对应 **Q16**） | 压缩引擎适配接口的接入时机与选型（一期已定"不接入"，此处确认二期） | A 二期按压缩比/延迟评估后决定（当前默认）；B 明确二期也不接入；C 二期必接入指定引擎 | A | 二期成本与压缩效果 |
| **U17**（新增，**spec 无对应 Q**） | 审批队列的跨进程归属：工具调用发生在 agent-service（`pkg/agent/approval` 为进程内状态），而任务编排在 harnessloop-service | A 审批队列留在 agent-service，harnessloop 只读状态（当前设计）；B 审批状态迁至共享存储（Redis/PG），两服务共用；C 由 harnessloop 自建队列 | **A（已确认，一期；二期转 B）** | 决定 `GetApprovalStatus` 是薄代理还是共享读 |
| **U18**（新增，对应 **Q17**） | 阶段级模型配置的默认值：d 阶段是否默认用更强模型 | A 默认继承任务级（spec 默认，当前设计）；B quality_first 下 d 默认强模型（4.14.3 模板）；C 全部阶段都默认强模型 | **A（已确认，2026-11-10）**，B 保留为配置模板 | 成本与验收质量平衡 |
| **U19**（新增，**spec 无对应 Q**） | 子任务级验收由谁执行：复用父阶段验收器（同一套注入与判定）还是独立轻量验收器 | A 复用同一验收器、输入收窄到子任务契约（当前设计）；B 子任务只做程序化检查、不做模型判定；C 独立轻量验收器 | **A（已确认，2026-11-10）+ 确定性优先分流** | 验收成本与一致性 |
| **U20**（新增，**spec 无对应 Q**） | Windows junction / reparse point 是否需要与符号链接同等对待（本地 CLI 模式） | A 同等对待（`EvalSymlinks` 已覆盖大部分，junction 需额外校验）；B 仅在 Windows 拒绝 junction 路径 | A + B 兜底 | 沙箱在 Windows 的可靠性 |

> **2026-11-10 决议**：**U1-A、U13-A、U17-A（二期转 B）、U18-A、U19-A 已确认**，P1 范围锁定，4.13.8 调度默认值、4.14.3 阶段模型、4.15.6 审批代理、4.7 遗忘边界按此执行。
>
> **2026-11-11 决议（v1.3）**：**U3 已关闭**——长期记忆 md 的主题上限为**软上限 50，仅告警、不 LRU、不淘汰**；30 主题上限、LRU 淘汰与读取提升仅适用于项目级记忆 `project.md`（对应 spec v1.2 Q10 已关闭问题）。
>
> 仍待确认（均有正文默认值，**不阻塞 P0/P1 开工**）：U2、U5、U6、U7、U8、U9、U10、U11、U12、U14、U15、U16、U20。**阻塞项：无。**
>
> **与 spec v1.1 的对应关系（2026-11-10）**：U13↔Q13、U14↔Q14、U15↔Q15、U16↔Q16、U18↔Q17；U17、U19、U20 为本设计新增（spec 无对应 Q）。

---

# 十二、附录

## 12.1 术语表

| 术语 | 定义 | 对应实现 |
|---|---|---|
| 阶段（Stage） | 任务生命周期中的 a/b/c/d/e 五个离散状态 | `pkg/harnessloop/fsm` |
| finish 信号 | 阶段完成的唯一合法入口（结构化工具调用） | `fsm/guard.go` 校验 `attempt_id+stage` |
| 阶段验收 Agent | 独立干净上下文的评估实体 | `pkg/harnessloop/acceptance` |
| 确定性验证 | 程序可机械判定的验收项 | `acceptance/checks/registry.go` |
| 回退映射 | 验收失败时的合法目标阶段集合 | `acceptance/rollback.go` |
| 检查点 | 可恢复执行快照 | `pkg/harnessloop/checkpoint` + Mongo |
| 上下文压缩 | 按模板摘要 + 引擎压缩，产出五要素 JSONL | `pkg/harnessloop/compaction` |
| 记忆主题（Topic） | md 内可独立寻址的记忆单元；项目 md 参与 LRU，长期 md 按稳定写入顺序（2026-11-11 修订） | `pkg/memoryfile/format.go` |
| LRU 队首 | **仅项目 md**：`order` 区块第一个 key（文件开头即队首）；长期 md 的 `order` 仅表示写入顺序（2026-11-11 修订） | `pkg/memoryfile/lru.go` |
| 记忆容量软上限 | 长期 md 主题数超过 50 仅发 `memory.capacity_warning`，不删除、不重排（2026-11-11 新增） | `pkg/memoryfile/capacity.go` |
| 代码索引 | 向量 + 倒排 + 图三类索引 | `pkg/codeindex` + `hlp_code` |
| RRF | `Σ 1/(k+rank)` 多路融合 | `pkg/rrf` |
| 知识广场 | 私有知识库接入与检索增强 | `services/knowledge-service` |
| Harness 事件 | 标准化的生命周期事件（带 seq/severity） | `pkg/harnessloop/event` |
| 父任务（Parent Task）（2026-11-10 新增） | 持有 a~e 五阶段 FSM、预算池与最终汇聚验收职责的任务实例 | `pkg/harnessloop/fsm` + `hlp_task.task_runs` |
| 子任务（Subtask）（新增） | 由 b 阶段 DAG 拆分出的可独立执行/验收/恢复的工作单元，7 态状态机，不含 a~e | `pkg/harnessloop/subtask` + `hlp_task.subtasks` |
| 交接（Handoff）（新增） | 子任务产物移交另一 Agent 的结构化契约（产物引用 + 五要素摘要 + 未完成项 + 约束） | `pkg/harnessloop/handoff` + `hlp_task.handoff_records` |
| 结果汇聚（Aggregation）（新增） | d 阶段对全部子任务产物做存在性/哈希/结论汇总并检测冲突 | `orchestrator/aggregate.go` |
| 模型注册表（Model Registry）（新增） | 模型窗口/单价/能力的唯一事实源（配置文件 + 进程内快照） | `pkg/harnessloop/modelregistry` + `config/harness_loop/models.yaml` |
| 能力匹配（Capability Match）（新增） | 判定模型是否满足阶段必需的工具调用/窗口/结构化输出的校验 | `modelregistry/capability.go` |
| 工具权限标签（新增） | 工具的四类访问属性 read/write/exec/network | `policy/registry.go`、`proto/mcp` 的 `permission_tags` |
| 沙箱根目录（Workspace Root）（新增） | 任务唯一文件访问根，所有文件类调用必须解析到其允许子路径内 | `policy/sandbox/pathguard.go` |

## 12.2 论文机制 → 需求 → 设计章节映射

| 《Harness Loop》机制 | 需求编号 | 设计章节 | 实现位置 |
|---|---|---|---|
| 五阶段 FSM 与转移表 | REQ-001~005 | 4.1.1 / 4.1.2 | `pkg/harnessloop/fsm/transitions.go` |
| finish 是唯一完成入口 | REQ-006 | 4.1.1 / 4.1.6 | `fsm/guard.go` |
| 仅三种停止条件 | REQ-007/008/027/029 | 4.1.4 | `fsm/terminal.go` |
| 独立验收 Agent + 干净上下文 | REQ-011~014 | 4.2.1 | `acceptance/context.go` |
| 确定性验证优先 | REQ-016/017 | 4.2.4 | `acceptance/checks/*` |
| 结构化验收结论 | REQ-015 | 4.2.3 | `acceptance/schema.go` |
| 验收失败回退（d→c / d→b） | REQ-018 | 4.2.5 | `acceptance/rollback.go` |
| 异常分类与重试上限 | REQ-021~029 | 4.3.1 / 4.3.2 | `retry/*` |
| 无 finish 静默检测 | REQ-023/024 | 4.3.3 | `retry/finish_reason.go` |
| 两类预算隔离 | REQ-028 | 4.3.4 | `retry/budget.go` |
| 检查点触发与恢复 | REQ-031~037 | 4.4 | `checkpoint/*` |
| 上下文压缩：**自研模板摘要单通道 + 五要素**（2026-11-10 修订，C 方案；引擎位仅预留） | REQ-038~046 | 4.5（4.5.2 / 4.5.9） | `compaction/{template,jsonl,pairing}.go` |
| markdown 记忆：**项目 md 30 主题 LRU / 长期 md 软上限 50 + 告警（不 LRU、不淘汰）**（2026-11-11 修订） | REQ-047~057 | 4.6 | `pkg/memoryfile` |
| **长期记忆不参与 LRU（2026-11-11 修订）** | REQ-050/051/057/062/104 | 4.6.1 / 4.6.4 / 4.6.7 / 4.6.8 / 4.7.3 | `pkg/memoryfile` |
| PG 全文 + pgvector 混合召回 | REQ-058~065 | 4.7 | memory-service `pgindex` |
| Tree-sitter 四步增量索引 | REQ-066~080 | 4.8 | `pkg/codeindex` |
| 单事务 + 哈希比对 | REQ-069~075 | 4.8.5 | `codeindex/writer.go` |
| RRF 融合与稳定排序 | REQ-081~086 | 4.9 | `pkg/rrf` |
| 知识广场混合召回 + rerank | REQ-087~095 | 4.10 | knowledge-service |
| CLI + runtime 同语义 | REQ-096~102 | 4.11 | `cmd/harness-cli` |
| 事件 schema + 大屏 | REQ-103~114 | 4.12 | `pkg/harnessloop/event` + 前端 |
| 平台集成与兼容 | REQ-115~122 | 2.3 / 3.2 / 5.4 | 全局 |
| **多 Agent 协作：父子任务 + 子任务 DAG**（2026-11-10 新增） | REQ-123~127 | 4.13.1 / 4.13.2 | `pkg/harnessloop/orchestrator/{planner,dag}.go`、`subtask/*` |
| **多 Agent 协作：调度、配额、熔断、公平**（新增） | REQ-128~130 / REQ-138 | 4.13.3 / 4.13.8 | `orchestrator/{scheduler,quota,circuit,fairness}.go` |
| **handoff 契约与禁止隐式共享**（新增） | REQ-131/132 | 4.13.4 | `pkg/harnessloop/handoff/*`、`a2a-service` |
| **d 阶段汇聚与失败传播**（新增） | REQ-133/134 | 4.13.5 / 4.13.6 | `orchestrator/{aggregate,propagation}.go` |
| **角色与能力、复用边界、禁止项**（新增） | REQ-135~137 / 140/141 | 4.13.7 / 4.13.9 / 4.13.11 | `subtask/registry.go`、`pkg/agent/registry.go` |
| **父子事件关联与大屏聚合视图**（新增） | REQ-139 | 4.13.10 / 7.2 / 7.4 | `pkg/harnessloop/event` + `SubagentTree.tsx` |
| **模型注册表、策略、阶段覆盖、能力匹配**（2026-11-10 新增） | REQ-142~146 | 4.14.1~4.14.4 | `pkg/harnessloop/modelregistry/*`、`config/harness_loop/models.yaml` |
| **运行时切换、回退链、成本联动**（新增） | REQ-147~150 / 155 | 4.14.5~4.14.7 | `modelregistry/{fallback,selector}.go` |
| **embedding 维度、密钥安全、热更新、可观测**（新增） | REQ-151~154 | 4.14.1 / 4.14.8 / 4.14.9 | `modelregistry/{secret,hotreload}.go` |
| **工具权限标签、双层名单、阶段矩阵**（2026-11-10 新增） | REQ-156/157/161 | 4.15.1 / 4.15.2 / 4.15.4 | `policy/{registry,matrix,engine}.go` |
| **目录沙箱、路径规范化、写入白名单**（新增） | REQ-158~160 / 169 | 4.15.3 / 4.15.10 | `policy/sandbox/{realpath,pathguard,escape}.go` |
| **exec 白名单与审批联动**（新增） | REQ-162/163 | 4.15.5 / 4.15.6 | `policy/sandbox/execguard.go`、`pkg/agent/approval` |
| **权限优先级、越界错误、审计、性能**（新增） | REQ-164~168 / 170 | 4.15.7~4.15.11 | `policy/{resolver,cache,audit}.go` |

## 12.3 需求模块覆盖自检

| 模块 | REQ | 覆盖章节 | 状态 |
|---|---|---|---|
| 5.1 FSM 状态机 | 001~010 | 4.1 / 5.1 / 9.2 | 已覆盖 |
| 5.2 阶段验收 | 011~020 | 4.2 / 9.3 | 已覆盖 |
| 5.3 异常与重试 | 021~030 | 4.3 | 已覆盖 |
| 5.4 检查点 | 031~037 | 4.4 | 已覆盖 |
| 5.5 上下文压缩 | 038~046 | 4.5 / 9.4 | 已覆盖 |
| 5.6 markdown 记忆 | 047~057 | 4.6 | 已覆盖（**项目 md LRU / 长期 md 软上限 50 告警，2026-11-11 修订**） |
| 5.7 记忆检索注入 | 058~065 | 4.7 | 已覆盖 |
| 5.8 代码索引 | 066~080 | 4.8 / 9.5 | 已覆盖 |
| 5.9 混合检索 RRF | 081~086 | 4.9 / 9.6 | 已覆盖 |
| 5.10 知识广场 | 087~095 | 4.10 / 9.6 | 已覆盖 |
| 5.11 CLI | 096~102 | 4.11 / 6.2 | 已覆盖 |
| 5.12 事件与大屏 | 103~114 | 4.12 / 7 | 已覆盖 |
| 5.13 集成兼容 | 115~122 | 2.3 / 3.2 / 5.4 / 6.4 | 已覆盖 |
| **5.14 多 Agent 协作**（2026-11-10 新增） | **123~141** | **4.13（全节）/ 5.1.2 / 5.1.3 / 6.2 / 7.2 / 9.9** | **已覆盖** |
| **5.15 模型选择**（2026-11-10 新增） | **142~155** | **4.14（全节）/ 5.1.4 / 6.2 / 7.4 / 9.10** | **已覆盖** |
| **5.16 工具权限与沙箱**（2026-11-10 新增） | **156~170** | **4.15（全节）/ 5.1.5 / 6.2 / 6.3 / 9.11** | **已覆盖** |
| DFX-P | 01~12（2026-11-10 扩至 12） | 9.7 / 10.6 / **4.15.11（P-10）/ 4.13.3（P-11）/ 4.14.9（P-12）** | 已覆盖 |
| DFX-R | 01~08（扩至 8） | 4.1.4 / 4.4 / 4.8.5 / 9.8 / **4.13.3（R-08 父子最终一致）** | 已覆盖 |
| DFX-S | 01~08（扩至 8） | 4.1.6 / 4.2.6 / 4.7.4 / 4.12.8 / 6.3 / **4.15（S-07/S-08）** | 已覆盖 |
| DFX-M | 01~05 | 4.5.8 / 4.12.1 / 10 | 已覆盖 |
| DFX-C | 01~05 | 2.3 / 5.3 / 6.4 | 已覆盖（DFX-C-05 记忆文件字段兼容已按双轨治理复核，2026-11-11 修订） |

> **2026-11-11 覆盖复核（v1.3）**：5.6 的记忆容量治理已从"两类文件统一 30 主题 LRU"更正为"**项目 md 30 主题 LRU / 长期 md 软上限 50 仅告警、不淘汰**"，对应 spec v1.2 的 REQ-050/051/057/062/104 与已关闭问题 Q10；覆盖章节为 4.6.1（治理差异）、4.6.3（分类顺序治理）、4.6.4（双轨算法）、4.6.7（事件范围）、4.6.8（实现影响与迁移）、4.7.3（召回排序键）、9.6（专项验证）。

## 12.4 交付物清单

| 阶段 | 交付物 | 位置 |
|---|---|---|
| 本文档 | 技术设计说明书 | `D:\Desktop\生涯\老公毕业论文\design.md` |
| P0 | FSM 库 + 服务 + CLI 最小闭环 + 测试报告 | 仓库内（见 8.1） |
| P1 | 记忆/压缩/索引/TUI/大屏/知识增强 + 指标基线 | 仓库内 + `bench/` 报告 |
| P2 | 多语言索引/rerank/异步化/运维 + 压测与容量报告 + **公平调度与沙箱模糊测试报告（2026-11-10 新增）** | 仓库内 + docs |
| P0（新增） | **权限矩阵测试报告 + 模型能力匹配测试报告** | `bench/policy-report/`、`bench/model-report/` |
| P1（新增） | **DAG 并发与熔断报告 + 模型回退成本报告 + 权限决策聚合报告** | `bench/dag-concurrency/`、`bench/model-fallback/`、`bench/policy-audit/` |

---

> 文档结束。本设计不含实现代码；DDL / proto / JSON Schema 均为草案，落地时以迁移脚本与 proto 编译结果为准。