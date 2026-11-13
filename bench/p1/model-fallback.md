# T-P1-T5 · 模型回退与成本报告（design 9.10 / 8.4 P1）

- 套件：`pkg/harnessloop/modelregistry/model_suite_test.go`
- 用例：**23 个 `TestModelSuite*`**（9.10 主表 20 行 + 回退性能 1 个 + 辅助断言）
- 真实依赖：PostgreSQL 55432（`hlp_task.model_selections`、008 DDL 的 CHECK/FK）；
  模型事件经 `orchestrator.PublisherEmitter` → 真实 `event.Publisher` 转发，
  按 `task_id` 断言 `seq = 1..N` 连续无缺号。
- 执行命令：

```bash
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
HARNESS_REQUIRE_PG=1 \
go test ./pkg/harnessloop/modelregistry/ -run 'TestModelSuite' -count=1 -v
```

- 结论：**20 行中 17 PASS / 3 PARTIAL / 0 FAIL**（G22 修复后；原 16/3/1）。

## 逐条对照（编号取自 design 9.10 原表的「用例组 / 用例」原文）

| 编号（9.10 用例组 / 用例） | 断言 | 结论 | 备注 |
|---|---|---|---|
| 注册表 / 登记缺 `context_window` 的模型 | `E_MODEL_META_INCOMPLETE` | PASS | 错误信息同时指出 `context_window` |
| 注册表 / 修改某模型单价 | 新任务按新单价；存量 `model_selections` 历史行不变 | PASS | 真实 PG：cheap 0.0005→0.005，旧行 `input_price=0.0005`、新行 `0.005`；估算成本同步上升 |
| 注册表 / 停用某模型 | 引用它的任务下一次调用前回退 + `model.switched` | PASS | 显式选择被 `E_MODEL_CAPABILITY_MISMATCH` 拒绝；回退链跳过 disabled；Execute 切到 cheap 并发 `model.switched(from=strong)` |
| 注册表 / 加载非法配置（`max_output > window`） | 整份快照拒绝，服务仍以旧快照运行 + 告警 | PASS | `HotReloader.Reload` 返回错误，旧 `Version`/模型仍可读；**“告警”未实现**（无 alert 钩子），仅返回错误 |
| 策略 / 同一需求分别 cost_first / quality_first / balanced | 记录 `model_id` + `strategy` + `candidates[].score`，同输入同输出 | PASS | 三策略分别 cheap/strong/cheap；两次调用候选分数逐项相同 |
| 策略 / 平局（差值 < 0.02） | `reliability → window → id` 稳定排序 | PASS | 三段分别验证 reliability、window、字典序 |
| 阶段覆盖 / 仅 d 配置强模型 | a/b/c/e 用任务级模型；d 用覆盖模型；事件含 stage + model | PARTIAL | d 走 override=strong；a/b/c/e 结果为 cheap，但**实现走 `strategyConfigured` 分支按策略打分**，并未使用 `defaults.task_model`；当前 registry 下结果与 design 语义一致，机制不同 |
| 能力匹配 / c 阶段选非工具调用模型 | `E_MODEL_CAPABILITY_MISMATCH` + `suggested_models` 全部满足；阶段不启动 | PASS | 所有 suggested 均 `ToolCall=true`；返回错误即阶段不进入 |
| 能力匹配 / 窗口小于 system+goal+stage_budget | 同上，`violations` 含 `context_window_too_small` | PASS | Reason 中含该标识 |
| 切换 / c 阶段中途请求换模型 | `E_MODEL_SWITCH_FORBIDDEN` | PASS | `ValidateSwitch` 无合法 reason 即拒绝 |
| 切换 / 主模型限流触发回退 | 允许 + `model.switched(primary, fallback, reason)` | PASS | payload from=cheap/to=strong/reason/trigger 齐全，另有 `model.fallback` |
| 切换 / 未经审批的模型升级 | 拒绝 + 审计 | PARTIAL | `manual_upgrade` 无 approval → `E_MODEL_SWITCH_FORBIDDEN`，带 `Approved+ApprovalID` 才放行；**modelregistry 无 audit 端口，`hlp_event.audit` 无写入** |
| 回退 / 单阶段连续 4 次回退 | 第 4 次不再发起，按异常分类处理 | PASS | 口径：`MaxPerStage=3`，每次回退候选 `StageFallbacks+1`；第 4 次 Execute 只尝试 primary 即 `E_MODEL_FALLBACK_EXHAUSTED` |
| 回退 / 回退调用 token/费用 | 计入任务预算池，不超上限 | PASS | 两次调用共 800 tokens 计入同一 `Budget`；超 500 上限立即 `E_MODEL_BUDGET_EXCEEDED`，只发生 1 次尝试 |
| 回退 / 回退列表为空 | 直接按 timeout/quota 处理，不挂起 | PASS | 同步返回 `E_MODEL_FALLBACK_UNAVAILABLE`，`Exhausted=true`，无重试 |
| 可用性 / 连续 5 次失败 | `status=unavailable`，三策略均不再选中；恢复后 `model.recovered` 且重新可选 | PASS | 第 5 次失败发 `model.unavailable`；三策略均不选 strong；`ObserveSuccess` 发 recovered，显式选择成功 |
| 成本联动 / 32k → 128k 窗口切换 | `threshold_tokens` 重算且触发点后移；预算估算按新单价 | PASS | threshold 26214 → 104857；strong 估算成本 > cheap |
| embedding / 768 维模型写入 1536 维索引 | `E_EMBEDDING_DIM_MISMATCH`，无部分写入 | PASS | G22 修复：新增 `CodeEmbeddingDimMismatch` 与 `Registry.ValidateEmbeddingWrite/ValidateEmbeddingVector`；768 模型写 1536 索引、1536 长度向量、enforce_dim 不符均返回该码，校验在写入前完成。加载期 `enforce_dim` 校验保留（`E_MODEL_META_INCOMPLETE`）。**codeindex 写入点尚未调用该守卫**（边界外，见边界 1） |
| 密钥 / 事件/日志/任务详情/`GET /models` | 无密钥明文；`api_key_ref` 不出现在响应 | PASS | `View` JSON 与 `model.selected` payload 均无 `api_key_ref`/`SUITE_KEY`；`ResolveCredential` 仅供运行时取 ref。**`GET /models` 是 service 层，未在本包验证** |
| 热更新 / 运行中改 d 阶段模型 | 当前调用用原模型完成；下一次用新配置；无报错 | PASS | 旧 Selector 仍返回 strong；`Reload` 报告 `stage_overrides.d` 变更；新 Selector 返回 cheap |

## 8.4 P1 门槛对照

| 8.4 条目 | 断言 | 结论 | 备注 |
|---|---|---|---|
| 模型三策略与回退 | 上述 9.10 行全量回归 | PASS（PARTIAL/FAIL 见上） | 三策略、阶段覆盖、回退链、可用性、成本联动已实测 |
| 必须留档产物：模型回退与成本报告 | 本文件 | PASS | 成本与回退延迟见下 |

## 性能口径与实测区间

**回退（rate_limit → 1 个 fallback 成功）**
- 口径：每次新建 `FallbackChain`（避免 StageFallbacks 累积），`Execute` 一次
  “primary 失败 + fallback 成功”的墙钟耗时；100 次采样取 P95。
- 宿主（Windows，4 次）：P95 **0s（<1ms 计时粒度）**，max 0–1.02ms。
- 容器（`golang:1.22-bookworm`，4 次）：P95 **29.7–35.7µs**，min 2.6µs，max 60.6–216.2µs。
- 阈值：design 9.10 未给回退时延阈值；套件设 100ms 挂死保护，实测远低于。
- 说明：数字只含**选择 + 两次回调**，不含真实 LLM 网络与 token 计费。

**成本联动**
- 32k 窗口：threshold 26,214 tokens；128k 窗口：threshold 104,857 tokens（均按 0.80 压缩比）。
- cheap(0.0005/0.0015) 1000 in + 200 out → 0.0008；strong(0.02/0.06) 同 token → 0.032。
- 切换后 `CompactionBudget = task_budget × 0.10`，预算仍记在任务池，不新建子账户。

## 发现的问题与边界

1. **embedding 维度守卫已实现、调用点未接线**：`ValidateEmbeddingWrite` / `ValidateEmbeddingVector`
   返回 `E_EMBEDDING_DIM_MISMATCH` 并在写入前拒绝；但 `pkg/codeindex` 不属于本轮允许范围，
   **索引写入路径仍需调用该守卫**，否则生产写入仍不会触发此码。
2. **未审批升级无审计**：`ValidateSwitch` 拒绝生效，但模型模块没有 audit sink，
   无法满足“拒绝 + 审计”。
3. **`model_selections` 写入口径**：`pkg` 测试受 Go internal 规则限制，无法 import
   `services/harnessloop-service/internal/repository`；套件在测试侧按 008 DDL 复刻列写入，
   并真实验证 CHECK（非法 stage/strategy → 23514）与 FK（未知 task → 23503）。
   **未直接调用生产 `ModelSelectionRepo`。**
4. **阶段覆盖机制偏差**：a/b/c/e 由策略打分选出，非 `defaults.task_model` 直取；
   若策略权重或候选集变化，结果可能与“任务级模型”不一致。
5. **`GET /models` 未验证**：属于 service 层，本包无 HTTP 入口；只验证了
   `Registry.Views()` 与事件 payload 不泄露。
6. **“告警”未实现**：非法快照只返回错误与 `model.registry_changed(result=failed)`，
   没有通知/告警通道。
7. 本报告证明的是**模型选择库契约**；不表示运行时已开启模型桥（
   `ModelBridge` 默认开关关闭），也不表示真实 provider 调用已验证。