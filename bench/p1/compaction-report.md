# T-P1-T2 压缩保真度与 C 方案专项报告

- 基线：`develop` = `2aec72b`
- 范围：仅新增 `pkg/harnessloop/compaction/fidelity_test.go`，未改实现代码
- 日期：2026-11-13

## 1. 验证命令与结果

| 场景 | 命令 | 结果 |
|---|---|---|
| 本地单测 | `go test ./pkg/harnessloop/compaction/... -count=1` | `ok agent-platform/pkg/harnessloop/compaction 3.362s` |
| 容器竞态检测 | `docker run --rm -v <repo>:/src -v <gomodcache>:/go/pkg/mod -w /src -e CGO_ENABLED=1 golang:1.22-bookworm go test -race ./pkg/memoryfile/... ./pkg/harnessloop/compaction/... -count=1` | `ok memoryfile 1.369s` / `ok compaction 2.193s` |

本机 `CGO_ENABLED=0` 且无 gcc，`-race` 使用宿主已有 `golang:1.22-bookworm` 容器执行，未拉取新镜像。

## 2. 核心指标

| 指标 | 样本 | 实测 | 阈值 | 结论 |
|---|---|---|---|---|
| 五要素齐全率 | 20 | 20/20 = 100% | 100% | 通过 |
| 工具配对合法率 | 20 样本 × 3 条路径（compacted/L1/L2） | 60/60 = 100% | 100% | 通过 |
| JSONL 可解析率 | 20 | 20/20 = 100% | 100% | 通过 |
| 20 样本抽检失真率 | 20 | 0/20 = 0% | ≤ 10% | 通过 |
| 默认配置 `degraded` | 20 | 0 次 | 0 | 通过 |
| 默认配置 `engine_fragment` | 20 | 0 个 | 0 | 通过 |

失真判定口径（确定性自动检查，非人工/模型语义标注）：`goal` 必须与原始目标逐字相等；`done/todo/constraints/background` 必须包含原始输入中的每一条。另有 `TestFidelityDistortionDetectorCatchesRewrite` 验证检测器能发现 goal 被改写与 done 条目丢失。

## 3. 专项断言

| 用例 | 断言 | 结果 |
|---|---|---|
| `TestFidelityFiveElementCompletenessRate100` | 每个样本五类各恰好 1 条，`ValidateFiveElements` 通过 | 通过 |
| `TestFidelityPairingLegalityRate100` | `ApplyCompaction`、`DegradeL1`、`DegradeL2` 产物配对问题均为空 | 通过 |
| `TestFidelityJSONLParseabilityRate100` | 逐行 `json.Unmarshal` 成功，`type/phase/summary/source_refs/timestamp` 齐全 | 通过 |
| `TestFidelityTwentySampleDistortionRateAtMost10Percent` | 20 样本失真率 0% | 通过 |
| `TestFidelityDefaultConfigProducesNoDegradedAndNoEngineFragment` | 默认 `NewEngineSlot(false,nil)` 返回 `ErrEngineDisabled`，`IsEngineFailureDegraded(false, …)=false`；20 样本 `engine_fragment=0`；默认 run 不降级 | 通过 |
| `TestFidelityTemplateFailureFallsBackToL1ThenL2` | 模板失败 → L1(`template_failed`)，token 下降；L1 仍超预算 → L2(`emergency`)，token 不增长，五要素可读、配对完整 | 通过 |
| `TestFidelityMockContextEngineToggle` | 关闭：`ErrEngineDisabled` 且引擎调用 0 次；开启：调用 1 次且输出可解析；开启失败：`IsEngineFailureDegraded=true`；关闭后失败不算降级 | 通过 |
| `TestFidelityNoExternalEngineImports` | 源码 import 白名单校验；`go list -deps .` 第三方依赖仅 `gopkg.in/yaml.v3`，无 `compress/engine/llmlingua/recomp/headroom` 关键词 | 通过 |

## 4. A/B 指标

数据来源：10 组确定性合成运行（非真实 LLM 调用），`BuildRun` → `SummarizeRuns` → `CompareVariants`。两个变体使用同一 `TemplateV1`，`variant` 仅作为 A/B 记录维度。

| 变体 | 样本 | tokens_after（均值） | ratio（均值） | downstream_acceptance_rate（均值） | degraded |
|---|---|---|---|---|---|
| `template_v1` | 10 | 2775 | 0.277 | 0.880 | 0 |
| `template_v2` | 10 | 2310 | 0.231 | 0.940 | 0 |

对比结论：`template_v2` 胜出（`AcceptanceDelta > 0`）。`CompactionRun` 与 `SummarizeRuns` 均包含 `tokens_after/ratio/downstream_acceptance_rate`；`CompactionEventPayload` 只含 `tokens_after/ratio` 等事件字段，不含 `downstream_acceptance_rate`。

## 5. 必须如实说明的边界

1. **20 样本抽检是确定性自动检查**：由测试内 stub completer 产出与输入一致的 JSON，再验证编码/解码链路不丢字段；它不能替代人工或模型对“语义失真”的抽检结论。报告中的 0% 仅代表该自动检查口径下未发现丢失。
2. **A/B 的 `template_v2` 不是已实现模板**：代码只有 `TemplateV1`，`VariantTemplateV2` 是记录维度标签；上表数据为合成对照，不代表真实模型 A/B 结果。
3. **引擎失败并入 L1 的路径一期不可达**：`ContextEngine` 只是预留位，生产无装配点；测试验证的是开关契约与 `IsEngineFailureDegraded` 判定，不能表述为“引擎降级已端到端生效”。
4. **`engine_fragment` 与默认降级**：默认配置下未产生 `engine_fragment`，也未因引擎位为空而产生降级；这是设计要求，不是遗漏。