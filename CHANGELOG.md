# Changelog

本文件记录 harness loop 的阶段性发布。格式参考 Keep a Changelog，版本号遵循语义化版本；阶段里程碑使用 `v0.<阶段>.<补丁>-p<阶段号>` 命名。

## [Unreleased]

## [0.2.0-p1] - 2026-11-13

### Added

- 记忆系统：markdown 记忆布局与格式、项目级 30 主题 LRU、`pinned` 豁免、原子写与外部修改检测、全局记忆稳定写入顺序（T-P1-01~03）。
- 上下文压缩：五要素摘要、JSONL 与工具配对校验、三级降级（L1/L2）、A/B 与保真度基线（T-P1-07~09）。
- 代码索引：AST 分块、四步增量与单事务写入、增量正确性套件、多路检索与 RRF（T-P1-10~14、T-P1-T3）。
- 知识广场：文档四态状态机、真实 BM25、来源引用与 LLM rerank（T-P1-15~16）。
- 子任务：七态状态机与 `subtasks` 表（T-P1-17）。
- 多 Agent 编排：DAG 规划与校验、8/20/20/2 配额调度、熔断与预算均分、handoff 契约、三种失败传播与父级汇聚（T-P1-18~21）。
- 角色与工具权限：角色工具集与阶段矩阵取交集、`permission_tags`、越权/缺标签错误码（T-P1-22）。
- 模型策略：三策略打分、阶段覆盖、回退链、可用性熔断、热更新与成本记录（T-P1-23~24）。
- 权限完整版：四层优先级、系统硬限制、决策缓存与审计、高危工具审批（T-P1-25~26）。
- CLI TUI：Bubble Tea 实时界面、阶段/尝试/验收/预算展示、断线补拉与 `--no-tui` 回退（T-P1-27）。
- 监控大屏：任务监控、多 Agent/模型/权限视图、SSE 适配层与降级轮询（T-P1-28~29）。
- 迁移整合：11 份 DDL 幂等重放、迁移器自建 `vector` 扩展、双写/开关矩阵（T-P1-30）。
- 运行时接线：编排、模型策略与权限/审计统一接入 runtime 与事件发布通道（T-P1-31）。
- 测试与基准：T-P1-T1~T5 五套回归、`bench/p1` 记忆/压缩/索引/检索/DAG/模型/权限报告。

### Changed

- 事件类型从 45 项扩展到 73 项，补 `subtask.*`、`handoff.*`、`aggregation.*`、`model.*`、`tool.denied`、`approval.*`。
- 模型选择记录表对齐设计：`context_window` 等列、单价与回退/审批快照、`(stage, model_id)` 索引。
- 权限决策缓存改为单次哈希 + O(1) LRU。

### Fixed

- 审批管理器并发读写数据竞争：`GetRequest` 返回深拷贝、`WaitForApproval` 锁内快照、Setter 补写锁。
- 补 `E_EMBEDDING_DIM_MISMATCH` 与写入前后维度校验。
- 敏感路径硬限制覆盖密钥与凭证文件，四层全 allow 仍拒绝。
- 模型选择迁移 008 的保留字语法错误与表结构缺失。
- 前端事件词典与后端 73 项逐项对齐。

### Known limitations

- Gateway 仍无 `/monitor/stream` SSE 端点；前端大屏的多 Agent/模型/权限视图数据源仍为 mock，运行时会退化为轮询。
- TUI 整屏交互未做人工目视验证（自动化环境无 TTY）。
- 验收器仍为 program-only，模型验收通道未接入。
- 上下文压缩作用于阶段输入消息，不是 agent 内部完整对话历史。
- 编排、模型策略、权限四层与 MCP 工具强制开关默认关闭，生产启用需部署侧配置。
- 记忆 `promoted`/`evicted` 事件无生产调用方；一次淘汰会产生 `deleted` + `evicted` 两条删除事件，消费方需去重。
- `RRF 增益 ≥5%` 指标未达标（Recall +0.0%、MRR −2.0%~−2.4%），原因见 `bench/p1/retrieval-report.md`。
- `pkg/codeindex` 尚未接入 embedding 维度守卫；Windows 宿主权限性能用例存在偶发抖动。

## [0.1.0-p0] - 2026-11-11

### Added

- 五阶段 FSM、attempt 生命周期、回退与终态语义。
- 验收判定、program 验收器、异常分类与重试策略、检查点保存/恢复/漂移检测。
- 事件与审计存储、seq 单写者、订阅与补拉。
- runtime 主循环（T-P0-17）、三种终态、中止幂等、优雅退出。
- Gateway 路由与鉴权、CLI `run/status/abort/resume/logs`、PG 存储与迁移。
- 工具权限预检、路径沙箱、exec 白名单、模型能力注册表。
- P0 验收测试 T-P0-T1~T4 与 `bench/p0` 报告。

### Known limitations

- 前端监控大屏尚未交付（P1 范围）。
- 真实 LLM 链路以文本协议 `[stage_finish]` 兜底跑通五阶段 `completed`，原生 function calling 受网关限制。
- 验收器仅覆盖 program 通道；Windows 未实测 SIGTERM。

[Unreleased]: https://github.com/kestarsheng/harness-loop/compare/v0.2.0-p1...HEAD
[0.2.0-p1]: https://github.com/kestarsheng/harness-loop/compare/v0.1.0-p0...v0.2.0-p1
[0.1.0-p0]: https://github.com/kestarsheng/harness-loop/releases/tag/v0.1.0-p0