# P1 记忆混合召回基准报告（T-P1-05）

- 生成时间：2026-11-11
- 被测：`services/memory-service/internal/pgindex`（`Recall`）、`pkg/rrf`
- 环境：PG `pgvector/pgvector:pg16`（`hlp-pg-test:55432`），3 条主题、1024 维假向量
- 执行：`HARNESS_TEST_PG_DSN=... go test -run TestRecall ./services/memory-service/internal/pgindex/`

## 一、功能验证

| 用例 | 验证点 | 结果 |
|---|---|---|
| `TestRecallFulltextSatisfiedSkipsEmbedding` | 全文命中 ≥ topK 时 **embedding 调用次数 = 0** | PASS |
| `TestRecallTriggersVectorForSemanticQuery` | 语义型查询触发向量路由，`trigger=semantic` | PASS |
| `TestRecallMergesRoutesWithoutDuplicates` | 两路同时贡献、合并后 `topic_key` 无重复、命中带 `sources` | PASS |
| `TestRecallRespectsFiltersAndTopK` | topK 截断、category 过滤、跨租户为空 | PASS |
| `TestRecallLatencyBudget` | 单次召回 < 1s | PASS |

## 二、延迟观测（本机，热连接，3 条数据）

| 场景 | `stats.LatencyMs` |
|---|---|
| 纯全文（3 条命中） | 8–13 ms |
| 全文 + 向量 + RRF | 9–12 ms |
| 全量测试（含建表/setup） | 90–280 ms |

- 目标 P95 ≤ 300ms：**当前数据量下远低于阈值**。
- 局限：测试集仅 3 条主题、假向量（1024 维单位向量），**不能代表 HNSW 在大规模数据下的表现**；真实 P95 需在千级主题 + 真实 embedding 下重测，留待 T-P1-T4（检索指标基线）。

## 三、实现要点

- 全文：应用层分词后以 **OR** `to_tsquery` 查询（修复了语义标记词 `how/如何` 进入 AND 导致空结果的问题）
- 向量触发：语义标记 / token > 8 / 全文不足 topK；否则不调用 embedding
- 融合：RRF `k=60` 可配，重叠主题只出现一次、分数累加、保留各路排名
- 过滤：租户、项目、scope、category、`decayed=false`

## 四、结论

混合召回链路（分词 → 全文 → 按需向量 → RRF → 过滤/截断）在真实 PG + pgvector 上跑通，功能门槛通过；**性能结论仅限小数据集**，已如实标注。