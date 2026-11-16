# 图扩展种子权重 A/B/C 对照（T-P2-02 修复稀释）

日期：2026-11-16
分支：`feature/p2-graphweight`（基线 `develop` @ `338eaf6`）

## 结论先行

**图路在当前语料上仍无正增益。** 按排名衰减把全量等权（A）的文件级 MRR 从 −4.7% 修到 −0.5%，
稀释确实被压掉约九成；但没有任何一组在「文件 + 函数」两个口径上同时转正，因此不调参凑绿灯，
结论如实保留。

| 组 | MRR(文件) | Δ vs 无图 | MRR(函数) | Δ vs 无图 | 图参与 top5 |
|---|---|---|---|---|---|
| rrf（无图基线） | 0.927 | — | 0.846 ~ 0.856 | — | 0.0% |
| A 全量等权（改动前） | 0.884 | **−4.7%** | 0.768 ~ 0.778 | **−9.2%** | 27.6 ~ 28.4% |
| B top-3 等权 | 0.942 | +1.6% | 0.806 ~ 0.816 | −4.7% | 12.8% |
| C top-5 + 衰减 0.1（本线默认） | 0.923 | −0.5% | 0.771 ~ 0.781 | **−8.9%** | 15.2% |

每组 4 次，同集、同机、连续运行；MRR(文件) 四次完全一致，函数级差异来自个别 query 在相邻名次间浮动。
Recall@20 四组均为 1.000（已饱和，无区分度），故主指标只取 MRR 与稀释度。

## 一个必须写下来的前提缺陷

改动前「多 seed 扩展」实际上是**单 seed 扩展**：

1. `cleanSeeds` 里 `sort.Strings(out)` 把种子重排成字母序，检索排名信息在这一步就丢了；
2. `graphEdges` 的 `ORDER BY seed` 同样是字母序，`LIMIT perSymbol*len(seeds)*2` 会被第一个
   （字母序最靠前的）种子的边吃光——实测单个高频种子有 197 条边，20 条预算全被它占满，后面的种子一条进不来。

也就是说，A 组「全量 20 seed 等权」在修复前其实只扩展了 1 个种子；修复后 A 组劣化到 −4.7%
（原报告为 −5.9 个百分点、约 −6%），才是「20 个种子真的都参与」的真实代价。
两处都改成按传入顺序 + `WITH ORDINALITY` + `row_number() PARTITION BY seed`。

## 实验设计

- 集：`bench/retrieval/queries.jsonl`，50 条自动采样（**非人工标注**），ground truth 为文件 + 函数；
- 语料：104 文件 / 4109 chunks / 12904 edges，每次运行重建，向量为 1024 维词袋哈希伪向量（非语义 embedding）；
- 四组同一进程内连续跑，避免跨运行环境漂移；
- 稀释度 = 前 5 条中「来源含 graph」的占比（分母固定 5×50）；
- 另记录了「只有 graph 一路来源」的严格口径：**四组均为 0.0%**。

## 为什么 C 还是负的

稀释不是唯一原因，实验把剩下的部分暴露出来了：

| 组 | MRR(文件) − MRR(函数) |
|---|---|
| rrf | 0.071 |
| A | 0.106 |
| B | 0.126 |
| C | 0.142 |

图路越强，文件级与函数级的差距越大。这说明图扩展主要在把**同文件的其它函数**顶上来：
文件级 MRR 因「同文件邻居」受益，函数级 MRR 因「正确函数没被拉上来」受损。
B 组文件级 +1.6% 也是这个机制，不是图路真的更懂相关性。

配合「图独占 top5 = 0%」看：图扩展出的 chunk 几乎全部同时被 fulltext/vector 命中
（共享同一批标识符 token），所以图路几乎不提供新条目，只在给已有条目**加分**；
而「被调用」与「与 query 相关」并不等价，这部分加分与相关性无关，融合后自然掉分。
衰减只是缩小了这份无关加分的面积，没有改变它的方向。

## 改动

- `pkg/codeindex/graph.go`：种子保序 + `SeedLimit` 截断 + `seedDecayWeight(rank, step)`；
  `graphEdges` 用 `WITH ORDINALITY` 透出排名、`row_number() OVER (PARTITION BY seed)` 按 seed 限流；
- `pkg/codeindex/search.go`：`GraphSeedLimit` / `GraphSeedDecay`，`mergeWeighted` 支持逐项系数
  （`score = Σ w_r·c_r/(k+rank_r)`），图路系数承载种子排名衰减；
- `services/harnessloop-service/internal/service/graph_expand.go`：`GraphPolicy.SeedLimit` 默认 5、
  `GraphPolicy.SeedDecay` 默认 0.1；负值显式表示「全量 / 等权」，供 A/B 组对照；
- `bench/retrieval/graph_abc.go`：`-graph-abc -json` 跑四组并输出指标。

## 关于公式的一处不一致

需求写的是 `w = graphWeight * (1 - (rank-1)/5)`，同时注明「rank5 降 40%」。
按前者 rank5 = 0.2（降 80%），与文字描述矛盾。按文字锚点实现为
`w = max(0, 1 - (rank-1)*0.1)`，即 rank1=1.0、rank5=0.6，步长通过 `-decay-step` 可调。
若本意是 0.2，把步长改成 0.2 即可，不需要改代码。

## 后续（不建议在本线继续调参）

1. **索引**：`code_chunks (tenant, project, symbol)` 单独立项（编号 013，等 B 线 012 合入后再加），
   本线未动 `pkg/pgindex`，避免撞号；
2. **方向区分**：caller 与 callee 的相关性未必对称，先分别统计两路对 MRR 的贡献再决定是否加权；
3. **有条件下的扩展**：只有扩展项与 query 存在标识符重叠时才允许加分，否则仅作展示、不参与融合；
4. 自动采样集的 Recall@20 已饱和，函数级标注是唯一有区分度的口径，后续调优应以函数级 MRR 为主指标。

## 复现

```bash
go build -o /tmp/abcbench ./bench/retrieval
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
  /tmp/abcbench -graph-abc -json | grep '^{'
```
