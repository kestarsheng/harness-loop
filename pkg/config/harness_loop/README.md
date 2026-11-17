# harness_loop 配置目录

运行期只读配置快照，由各服务启动时加载，禁止在业务代码中硬编码阈值、名单与矩阵。

计划的配置文件（随对应任务落地）：

| 文件 | 用途 | 落地任务 |
|---|---|---|
| `features.yaml` | 运行时开关（见下） | T-P1-31 / T-P2 |
| `retry.yaml` | 异常分类与每类重试上限 | T-P0-13 |
| `models.yaml` | 模型注册表（窗口/单价/能力） | T-P0-20 |
| `stage_matrix.yaml` | 阶段权限矩阵 | T-P0-19 |
| `stage_allow.yaml` / `stage_deny.yaml` | 工具白名单/黑名单 | T-P0-19 |
| `exec_allow.yaml` | exec 命令白名单 | T-P0-19 |
| `criteria_templates.yaml` | 验收标准模板 | T-P0-10 |
| `health.yaml` | 健康度计算权重 | T-P1-28 |
| `compaction.yaml` | 压缩阈值与策略 | T-P1-07 |
| `approval.yaml` | 审批规则 | T-P1-25 |
| `subtask.yaml` | 子任务并发与配额 | T-P1-18 |

## features.yaml：开关语义

`flags[].default` 是兜底值；`rules[]` 按「先匹配先返回」求值，匹配项默认视为开启，
显式写 `enabled: false` 才会关。可用的匹配维度：`tenants` / `projects` / `task_types`
/ `users` / `percentage`（0..100，按 `tenant+user+flag+规则序号` 稳定哈希分桶）。

三条 P2 新链路开关默认全关，**关掉即回到旧路径**：

| 开关 | 打开后的链路 | 关闭后的旧行为 |
|---|---|---|
| `harness.loop.orchestrator.enabled` | Planner/Scheduler/Aggregator 多 Agent 编排 | `pkg/agent/engine.go` 单 Agent |
| `harness.loop.model_strategy.enabled` | 模型策略选择 + 阶段覆盖 + 回退链 + 成本记录 | `pkg/config.LLM.Model` 单模型兜底 |
| `harness.loop.policy_layers.enabled` | 四层权限解析 + 审计 + 高危审批（MCP 工具强制也在这一层） | 旧 `policy.Engine` 一次性判定 |

两个注意点：

1. **启动时解析一次**。三个开关在 `services/harnessloop-service/cmd` 里解析成 bridge
   的 `enabled` 位，解析上下文的租户是 `platform`（bridge 是进程级单例）。所以租户规则
   要生效，`tenants` 里必须包含本服务所属租户；只写别的租户不会在启动时打开链路。
   请求级的租户差异目前只影响 `harness.loop.fsm.enabled`（handler 每次请求求值）。
2. **名字有新旧两套**。`harness.loop.model_strategy.enabled` 与
   `harness.loop.policy_layers.enabled` 是 P2 的规范名，早期的
   `harness.loop.model.strategy.enabled` / `harness.loop.policy.resolver.enabled`
   作为别名继续被读取（规范名优先）。

本地演示想看新链路：`scripts/dev-up.sh --demo`（或 `HLP_DEMO_FLAGS=1`）会把三条开关
临时置为 `default: true`，不动仓库里的这份文件。