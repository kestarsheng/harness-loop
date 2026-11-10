# P0 模型能力负向测试报告（T-P0-T4 · model）

- 生成时间：2026-11-11
- 被测包：`pkg/harnessloop/modelregistry`
- 执行命令：`go test ./pkg/harnessloop/modelregistry/... -run TestNegative`
- 结果：**PASS**

## 一、元数据完整性

| 场景 | 期望 | 结果 |
|---|---|---|
| 缺 `context_window` | E_MODEL_META_INCOMPLETE | 拒绝 |
| 缺单价字段（`price_in_per_1k`） | E_MODEL_META_INCOMPLETE | 拒绝（修复：指针区分“缺失”与 0） |
| `max_output_tokens > context_window` | E_MODEL_META_INCOMPLETE | 拒绝 |
| embedding 缺 `embedding_dim` | 拒绝 | 拒绝 |
| chat 带 `embedding_dim` | 拒绝 | 拒绝 |
| 重复模型 ID | E_MODEL_DUPLICATE_ID | 拒绝 |

## 二、能力匹配

| 场景 | 期望 | 结果 |
|---|---|---|
| 非工具模型 + 阶段需工具（qwen-lite） | E_MODEL_CAPABILITY_MISMATCH | 拒绝 |
| 窗口不足（required 100000，qwen-max 32768） | E_MODEL_CAPABILITY_MISMATCH | 拒绝 |
| 未知模型 | E_MODEL_CAPABILITY_MISMATCH（未知模型原因） | 拒绝 |
| embedding 模型用于 chat 阶段 | 拒绝 | 拒绝 |
| `CheckStageCapability` 非工具模型 | 拒绝并给候选 | 通过 |

**候选约束**：`suggested_models` 中每一个都满足“支持工具调用 + 窗口达标”；策略解析出的 `Candidates` 同样全部满足约束，无越权候选。

## 三、凭据安全

- `Views()` 只暴露 `HasCredential=true`，**不含** `api_key_ref` 明文
- 凭据仅通过 `ResolveCredential` 返回 `env:...` 引用
- `ListModels` 响应无密钥字段（handler 层同样使用 View）

## 四、修复记录

原实现把单价声明为 `float64`，缺失时被 YAML 解码成 0，无法区分“免费模型”和“未填单价”。已改为 `*float64`：
- `nil` → 元数据不完整
- 显式 `0` → 合法免费模型
并同步 `View()` 与排序逻辑，避免指针外泄。

## 五、结论

P0 模型能力门槛：**通过**。元数据不完整、能力不匹配、凭据泄露三类风险均已封堵，且候选集约束可验证。