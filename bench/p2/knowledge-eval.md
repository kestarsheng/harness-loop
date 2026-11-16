# P2 知识检索评测报告（T-P2-T3 · 知识部分）

- 生成时间：2026-11-16
- 分支/worktree：`feature/p2-t3-cli` / `hlp-p2-t3-cli`，基线 `3b69019`
- 评测程序：`go run ./bench/parser knowledge`（`bench/parser/knowledge.go`）
- 固定数据集：`bench/parser/knowledge/docs.jsonl`（10 篇知识卡）+ `bench/parser/knowledge/cases.jsonl`（6 条 query，人工标注 `relevant` / `claims[].support` / `key_terms`）
- 机器可读结果：`bench/parser/out/knowledge-local.json`
- 回归阈值：`-min-score 0.40`（绝对地板）+ `-max-regress 0.05`（两轮之间单项均值允许下滑幅度），任一违反退出码 1

## 一、先声明被测范围（这条比数字重要）

本报告**没有**跑真实 `knowledge-service` + 真实 LLM。原因与替代方案：

| 环节 | 真实链路 | 本报告的做法 |
|---|---|---|
| 检索 | knowledge-service：PG 全文 + 向量 + graph 三路 RRF → HTTP rerank top20→top5 | 确定性替身检索器：词面 BM25-lite（latin 词 + 中文二元组，idf 加权） |
| 答案 | 真实 LLM 生成 | 声明式抽取基线：取召回文档中与 query 重合度最高的两句 |
| 评测判定 | RAGAS 式 LLM judge | 人工标注：claim → 支撑文档、query → 相关文档、必点术语 |

这样做的取舍：**指标绝对值不代表线上质量，但两轮之间的差值是可归因的**——因为判定标签和答案生成方式完全固定，唯一变化的是检索配置，指标一动就一定是检索层动的。LLM judge 的采样噪声会让「两次对比」失去意义，这是本设计刻意规避的。

⚠️ 由此产生的读数边界（必须与数字一起读）：

1. precision 天花板低（约 0.5）：每条 case 只标 1–2 篇相关文档，而 top3 必有噪声；真实链路的 rerank 会把 precision 拉高，本数据集**不覆盖 rerank 层**。
2. relevance 恒为 1.000：抽取基线的答案直接来自召回文档，术语覆盖率容易饱和；它只对「关键文档完全没召回」敏感（case-05 就抓到了）。
3. faithfulness 才是这套数据集的判别力所在：它按「claim 的支撑文档是否被召回」计分，直接反映召回缺口。

## 二、两轮配置

| 轮次 | 配置 | 说明 |
|---|---|---|
| run1-baseline-lexical | 纯词面召回 top3 | 对应「不加任何扩展」的基线 |
| run2-seed+neighbor | 种子 top2 + 按 tag 邻接补全，上限 4 篇 | 对应索引线的 graph/邻接扩展思路（1 跳、去重、按上限截断） |

## 三、结果

### 3.1 run1-baseline-lexical

| case | 召回 | precision | faithfulness | relevancy |
|---|---|---|---|---|
| case-01 增量索引怎么判断文件变更 | doc-01 | 1.000 | 1.000 | 1.000 |
| case-02 treesitter 标签缺失时的 TS 路径 | doc-02, doc-01 | 0.500 | 1.000 | 1.000 |
| case-03 RRF 三路权重与图扩展上限 | doc-03, doc-09, doc-04 | 0.333 | 1.000 | 1.000 |
| case-04 rerank 超时处理 | doc-04, doc-03 | 0.500 | 1.000 | 1.000 |
| case-05 两个终端如何区分任务 | doc-05, doc-09, doc-08 | 0.333 | 0.667 | 1.000 |
| case-06 SSE 断线重连补事件 | doc-07, doc-06, doc-08 | 0.333 | 1.000 | 1.000 |
| **均值** | | **0.500** | **0.944** | **1.000** |

### 3.2 run2-seed+neighbor

| case | 召回 | precision | faithfulness | relevancy |
|---|---|---|---|---|
| case-01 | doc-01, doc-02 | 0.500 | 1.000 | 1.000 |
| case-02 | doc-02, doc-01 | 0.500 | 1.000 | 1.000 |
| case-03 | doc-03, doc-09, doc-04 | 0.333 | 1.000 | 1.000 |
| case-04 | doc-04, doc-03 | 0.500 | 1.000 | 1.000 |
| case-05 | doc-05, doc-09, **doc-06** | **0.667** | **1.000** | 1.000 |
| case-06 | doc-07, doc-06, doc-05 | 0.333 | 1.000 | 1.000 |
| **均值** | | **0.472** | **1.000** | **1.000** |

### 3.3 差值（可对比性验证）

| 指标 | run1 | run2 | Δ |
|---|---|---|---|
| context precision | 0.500 | 0.472 | **-0.028** |
| faithfulness | 0.944 | 1.000 | **+0.056** |
| answer relevancy | 1.000 | 1.000 | 0.000 |
| overall | 0.815 | 0.824 | +0.009 |

脚本输出（原样）：

```
delta run1-baseline-lexical -> run2-seed+neighbor: precision -0.028 faithfulness +0.056 relevancy +0.000
regressions (1):
  - case-01: context_precision 1.000 -> 0.500 (retrieved [doc-01] -> [doc-01 doc-02])
PASS: above the 0.40 floor and no metric dropped by more than 0.05
```

## 四、回归定位（这是本节要交付的能力，不是副作用）

扩展策略的两面性在一份数据里同时出现了：

- **收益**：case-05 的召回从 `doc-05,doc-09,doc-08` 变成 `doc-05,doc-09,doc-06`，第三篇 claim（「看板卡片标题优先 terminal_label」）的支撑文档 `doc-06` 被邻接补全捞回，faithfulness 0.667 → 1.000，precision 0.333 → 0.667。
- **代价**：case-01 本来只召回 `doc-01`（precision 1.000），补全把 `doc-02` 塞进上下文，precision 掉到 0.500。
- **其余 4 条**：case-02/03/04 召回完全不变；case-06 的第三篇从 `doc-08` 换成 `doc-05`，同样不相关，三项指标均不变——说明扩展只在「种子带 tag 且有邻接文档」时才生效，不是无脑加长上下文。

即：**邻接扩展提升 recall 上限、压低 precision，且顶不住"本来就只该有 1 篇"的查询**。定位能力体现在——脚本按 case 打印 `指标 旧值 -> 新值 (召回 … -> …)`，能直接指到 case-01 这一类，而不是只给一个下降的均值。

## 五、复跑与阈值

```bash
go run ./bench/parser knowledge -json bench/parser/out/knowledge-local.json
# 收紧回归带：
go run ./bench/parser knowledge -min-score 0.45 -max-regress 0.02
```

- 两次运行输出**逐字节一致**（`diff` 无差异），确定性成立，可作为 CI 基线。
- 阈值语义：`-min-score` 是绝对地板（防整体塌陷），`-max-regress` 是两轮漂移带（防悄悄退化）；gate 在脚本里，不在人眼里。
- 新增 case 时只需追加 `cases.jsonl`：`claims[].support` 指向真实写出该结论的文档 id，`key_terms` 必须是合格答案会点到的术语。

## 六、结论与遗留

- faithfulness / relevancy / precision 三项在固定数据集上完成两轮可比对，差值可归因到检索配置，回归可定位到具体 case —— 任务要求的「两次对比」达成。
- 遗留（如实列出，未在本任务域内解决）：真实 knowledge-service + LLM judge 链路未接入；本数据集规模（10 文档 / 6 查询）只够做方向性判别，不足以做统计显著性结论；precision 地板受替身检索器限制，接真实 rerank 后应重跑并重置 `-min-score`。
