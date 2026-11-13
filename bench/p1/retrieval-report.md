# P1 代码检索模式基准报告（T-P1-T4）

- 生成时间：2026-11-13
- 被测：`pkg/codeindex.Search`（fulltext / keyword+vector / +graph）、`pkg/rrf` 融合
- 基准程序：`bench/retrieval/run.go`（`go run ./bench/retrieval`）
- 查询集：`bench/retrieval/queries.jsonl`（50 条）
- 环境：Windows 10 x64，Go 1.22.12，PG `pgvector/pgvector:pg16` 容器 `hlp-pg-test:55432`
- DSN：`postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable`
- 执行：
  ```bash
  HARNESS_TEST_PG_DSN="postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable" \
  go run ./bench/retrieval
  # 重新生成查询集：
  go run ./bench/retrieval -build
  ```

## 一、标注口径（必读）

**本查询集为「自动采样，非人工标注」。**

- `queries.jsonl` 由 `run.go -build` 扫描仓库 Go 源码、解析 AST 得到 function/method chunk 后自动生成；
- ground truth 为 **文件 + 函数**（`file` / `symbol` 字段）；
- query 文本 =「函数名拆词 + 函数体内 3 个其它标识符」；
- 每条记录带 `"label_source":"auto-sampled:go-ast"` 与 `"note":"自动采样，非人工标注"` 字段，报告与 stdout 均重复声明；
- **没有人工筛选、人工改写或人工相关性判断**。

由此带来的已知偏差（必须与指标一起阅读）：

1. **词汇泄漏**：query 词来自 ground truth 函数自身，而 chunk 的 `content_tsvector` 也由该函数文本生成，全文命中天然容易饱和；
2. **单相关文档**：每条 query 只有 1 个 ground truth，Recall@K 等价于「是否进前 K」的命中率；
3. **向量非语义**：1024 维词袋哈希伪向量（hashing trick + L2 归一化），不是真实 embedding，vector 路只反映词重合度。

## 二、语料规模

| 项 | 值 |
|---|---|
| 索引文件 | 104（50 条 query 的 ground-truth 文件去重 + 60 个干扰文件，`-distractors 60`） |
| chunks | 3937 |
| edges | 12503（import + call） |
| 索引总耗时 | 1m31s – 2m0s（两次运行区间，含 HNSW 插入，非 T3 门槛项） |
| 采样范围 | 仓库 `*.go`，排除 `*_test.go`、`.git`/`vendor`/`node_modules`/`frontend`/`testdata` 等，≤256KB |

## 三、四模式对比（验收命令实测输出）

| 模式 | Recall@5(文件) | Recall@20(文件) | Recall@20(函数) | MRR(文件) | MRR(函数) | P50 | P95 |
|---|---|---|---|---|---|---|---|
| fulltext | 0.980 | **1.000** | 1.000 | **0.950** | 0.866 | 57.7–83.4 ms | 135.9–198.9 ms |
| vector | 0.980 | **1.000** | 1.000 | 0.856–0.866 | 0.729–0.735 | 45.9–50.1 ms | 72.2–84.9 ms |
| rrf（keyword+vector） | 0.980–1.000 | **1.000** | 1.000 | 0.927–0.931 | 0.850–0.856 | 148.2–175.4 ms | 310.4–314.2 ms |
| rrf+graph（+call/import 扩展） | **1.000** | **1.000** | 1.000 | 0.894 | 0.737–0.747 | 177.7–214.2 ms | 300.3–354.6 ms |

- 区间覆盖 2026-11-13 的三次独立运行（首跑、子代理复跑、验收复跑）；差异来自 pgvector HNSW 近似检索、索引构建顺序与本机负载，指标稳定在窄区间内。
- 唯一跨运行变化超过 0.001 的是 `rrf` 的 Recall@5（0.980 ↔ 1.000），对应 50 条中 1 条 query 在前 5/前 6 名之间浮动；报告保留区间而非取单次最优。
- Recall@20(文件) 已饱和到 1.000，说明在当前自动采样集上四个模式都能把目标文件检索进前 20。

### RRF 增益（相对最佳单路）

最佳单路按 `max(fulltext, vector)` 逐指标取：

| 对比 | ΔRecall@20(文件) | ΔMRR(文件) |
|---|---|---|
| rrf vs 最佳单路 | **+0.0%** | **-2.0% ~ -2.4%** |
| rrf+graph vs 最佳单路 | **+0.0%** | **-5.9%** |

## 四、门槛对照（如实判定）

| 门槛 | 要求 | 实测 | 结论 |
|---|---|---|---|
| Recall@20（文件，rrf+graph） | ≥ 0.75 | 1.000 | PASS |
| Recall@20（文件，rrf） | ≥ 0.75 | 1.000 | PASS |
| Recall@5（文件，参考「记忆 K=5 ≥ 0.8」，该门槛属 T-P1-05 记忆召回） | ≥ 0.80 | 0.980 | PASS（仅参考，不作为本任务门槛） |
| MRR（文件，fulltext 单路） | ≥ 0.55 | 0.950 | PASS |
| MRR（文件，vector 单路） | ≥ 0.55 | 0.862 | PASS |
| MRR（文件，rrf） | ≥ 0.65 | 0.927 | PASS |
| MRR（文件，rrf+graph） | ≥ 0.65 | 0.894 | PASS |
| **RRF 增益** | **≥ +5%** | **+0.0%（Recall）/ -2.0%~-2.4%（MRR）** | **FAIL** |

### 增益未达标的 gap 与原因（不作美化）

- Gap：Recall@20 增益 0.0 个百分点，MRR 增益 -2.0~-2.4 个百分点（rrf）与 -5.9 个百分点（rrf+graph），距 +5% 目标差 5.0–10.9 个百分点。
- 原因 1（天花板效应）：fulltext 的 Recall@20 与 Recall@20(函数) 已是 1.000，任何融合都无法在 Recall 上再提升，指标没有上升空间。
- 原因 2（同源信号）：伪向量与全文都来自同一 chunk 的词袋，两路高度相关，RRF 无法带来互补信息；融合后排序被相关性略低的向量路扰动，MRR 反而下降。
- 原因 3（graph 稀释）：`graphSearch` 把 `target_symbol` 的调用方 chunk 插入候选，在单相关文档的评估下这些扩展命中是「正确但非目标」，会把精确命中往后挤，进一步压低 MRR。
- 原因 4（标注偏差）：自动采样存在词汇泄漏，使 fulltext 近乎作弊地容易命中；真实用户查询（自然语言、不含函数名）下各模式差距会显著不同，本表不能外推。

## 五、结论

1. 四模式在真实 PG + pgvector 上全部可执行，`go run ./bench/retrieval` 一键产出对比表，复现路径明确。
2. Recall@20（≥0.75）、MRR（≥0.55/0.65）、Recall@5（≥0.8，参考）均达标；**RRF 增益 ≥+5% 未达标，已如实记录**，并给出天花板效应、同源信号、graph 稀释、自动采样词汇泄漏四条原因。
3. 指标仅代表「自动采样、伪向量、104 文件」的基线，**非人工标注、非真实 embedding、非大规模仓库**，不能作为线上检索质量的结论。

## 六、后续改进建议（按影响排序）

1. **人工/半人工标注**：至少 50 条真实开发者查询（自然语言描述、不含函数名），人工确认文件/函数级 ground truth，消除词汇泄漏。
2. **真实 embedding**：接入生产 embedding 模型替换伪向量，使 vector 与 fulltext 提供互补信号，RRF 增益才有测量意义。
3. **多相关文档**：每条 query 标注 2–5 个相关函数，用 nDCG@K / Recall@K 替代单文档命中率，避免 Recall 饱和。
4. **扩大干扰集**：语料提升到千级文件、万级 chunk，重新测 P95 与 HNSW 近似误差。
5. **graph 权重可调**：当前 graph 与 keyword/vector 等权 RRF，建议对 graph 降权或只做扩展不参与排序，观察 MRR 变化。