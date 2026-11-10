# harness_loop 配置目录

运行期只读配置快照，由各服务启动时加载，禁止在业务代码中硬编码阈值、名单与矩阵。

计划的配置文件（随对应任务落地）：

| 文件 | 用途 | 落地任务 |
|---|---|---|
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