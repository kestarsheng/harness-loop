# 图扩展方向诊断：caller / callee / both

- 日期：2026-11-16
- 分支：`feature/p2-graph-direction`（worktree `hlp-p2-graph-direction`，基线 `cfe87ba`）
- 文件域：`pkg/codeindex/{graph.go,search.go}`（只加 direction 维度）、`bench/retrieval/{run.go,graph_direction.go}`、本报告
- 未触碰：`bench/p2/perf-suite.md` 与 k6 脚本（B 线独占）、`pkg/pgindex/**`（D 线独占）
- 上游依据：`bench/p2/graph-seed-weight.md`（种子权重 A/B/C，结论"图路无正增益、稀释来自无关加分"），本报告接过它的下一步问题：**稀释到底来自哪一侧**

## 0. 这是诊断，不是调参

本轮只做一件事：把图扩展的**方向**从写死的"双向"拆成 caller / callee / both 三档，测哪一侧在拖指标。

以下全部**冻结不动**：种子上限（固定 B 组 top-3）、种子衰减（等权 0）、图路权重（默认 0.8）、
融合公式（`Σ w_r·c_r/(k+rank_r)`）、RRF 的 k（60）、topK（20）、语料与 query 集。
没有任何一组参数是"看着结果再改"的——决策规则在跑之前就写死在 `decideDirection()` 里，跑完只做判定，不做回调。

## 1. 结论先行

**caller 臂是拖累源，callee 臂与基线等价。** 函数级 MRR 相对基线：caller **−6.4%**、callee **+0.3%**、both **−4.8%**。

三组数据（B 组 top-3 等权，每组 4 次重复，同集同机连续）：

| 组 | 方向 | MRR(函数) 区间 | 中位 | Δ vs 基线 | MRR(文件) | Δ vs 基线 | 图参与 top5 | 扩展 caller/callee |
|---|---|---|---|---|---|---|---|---|
| rrf-baseline | — | 0.853 ~ 0.853 | 0.853 | 基准 | 0.926 | 基准 | 0.0% | 0 / 0 |
| caller-only | caller | 0.798 ~ 0.798 | 0.798 | **−6.4%** | 0.930 | +0.5% | 9.0% | 152 / **0** |
| callee-only | callee | 0.856 ~ 0.856 | 0.856 | +0.3% | 0.942 | **+1.8%** | 4.9% | **0** / 128 |
| both | both | 0.812 ~ 0.812 | 0.812 | **−4.8%** | 0.941 | +1.6% | 13.1% | 112 / 128 |

（扩展计数为 4 次累计；`caller-only` 的 callee 侧恒为 0、`callee-only` 的 caller 侧恒为 0，可反向证明方向开关真的传到了 SQL。）

**决策：按规则 3 处理——拆开关，默认只开 callee。**

命中规则 3 而不是规则 1 的关键在于：callee 的 +0.3% 落在预先定义的噪声带 `[0, +1%)` 内。
按规则它算"≥ 基线"，但不构成"图路有效"的证据，所以结论只能是"默认单路、留开关"，
不能写成"callee 提升了 0.3%"。第 5 节补了跨进程复跑的漂移证据，这个 +0.3% 比漂移还小。

## 2. 实验设计

| 项 | 值 |
|---|---|
| 语料 | 103 文件 / 4134 chunks / 13091 edges，每轮重建（同一 tenant/project 先清后建） |
| query 集 | `bench/retrieval/queries.jsonl`，自动采样（**非人工标注**），49/50 条 |
| 向量 | 1024 维词袋哈希伪向量（**非语义 embedding**） |
| 组数 | 4 组：rrf-baseline / caller-only / callee-only / both |
| 重复 | 每组 4 次，同进程、同 query 顺序、连续交替执行（外层 rep、内层 query、最内层组） |
| 主指标 | 函数级 MRR（同文件 + 同符号命中） |
| 辅助指标 | 文件级 MRR、图参与 top5（前 5 条里"来源含 graph"的占比，分母固定 5×49） |
| 排除 | `q012` 的 ground truth 指向 `.codeartsdoer/temp/` 下已被清理的临时文件，本轮剔除并打印 |

两点口径说明：

- **为什么固定 B 组**：上一轮已证明 A（全量等权）与 C（top-5 衰减）都在稀释带里，换 seed 策略会和方向维度混在一起，
  无法归因。B 组是唯一"文件级转正、函数级还没跑偏到不可比"的配置，作为载体最干净。
- **为什么 4 次几乎必然一致**：伪向量 + 稳定排序让同进程内的检索完全确定（见第 5 节），
  重复的作用不是求平均，而是**证明区间不是单次抖动**。区间宽度为 0 说明链路确定，不说明 Δ 显著。

## 3. 三组数据：逐次明细

| 组 | rep1 | rep2 | rep3 | rep4 |
|---|---|---|---|---|
| rrf-baseline | 0.853 | 0.853 | 0.853 | 0.853 |
| caller-only | 0.798 | 0.798 | 0.798 | 0.798 |
| callee-only | 0.856 | 0.856 | 0.856 | 0.856 |
| both | 0.812 | 0.812 | 0.812 | 0.812 |

延迟（本轮只记录、不判定，归 013 线复跑）：

| 组 | P50 | P95 |
|---|---|---|
| rrf-baseline | 189.2 ms | 348.0 ms |
| caller-only | 220.3 ms | 343.0 ms |
| callee-only | 225.0 ms | 383.2 ms |
| both | 238.4 ms | 377.6 ms |

单路没有把延迟砍半：图扩展的成本在 `graphEdges` 的分区排序与 `chunksBySymbols` 回表，边少了但这两步仍在，
所以 P50 只从 189 ms 抬到 220~225 ms。图路本身就是这段开销的主体，方向只影响幅度。

## 4. 为什么 caller 有害、callee 中性

机制推断，与数据一致：

- **caller-only（−6.4%）**：调用方是"谁在用我"，与 query 描述的"我是什么/我做什么"语义距离远。
  它的文件级只有 +0.5%，说明连"同文件红利"都没吃到——调用方通常位于别的文件，
  图路把一批**语义无关但结构相关**的条目塞进融合池，只加分不换血，纯稀释。
- **callee-only（+0.3% / 文件 +1.8%）**：被调用方与实现强相关（函数体里就写着这些调用），
  拉上来的多半落在 ground truth 文件内，于是文件级明显受益；但**正确的那个函数**没被顶到更前，
  函数级几乎不动。这与上一轮"图路越强、文件级与函数级差距越大"的观察是同一件事，这次被方向拆开直接证实。
- **both（−4.8%）**：13.1% 的图参与 = caller 的稀释面积（9.0%）+ callee 的文件级红利（4.9%），
  两者抵消后净负。注意 both **不是两侧简单叠加**：双向时两侧争抢同一份 per-seed 额度，
  caller 边从 38 掉到 28（每 rep），callee 仍是 32——caller 先占额度，把 callee 的部分名额挤掉了。
- **图独占 top5 三组全为 0%**：与上一轮一致，图扩展不提供新条目，只给已被 keyword/vector 命中的条目加分。
  这条决定了"调权重"永远救不了它——score 加在一个语义无关的条目上，权重再小也只是稀释得慢一点。

## 5. 一个必须写下来的读表前提：跨进程不可直接比

我在同一台机器上用**同一条命令**跑了两轮完整 4 次重复，两轮的绝对值和 Δ 如下：

| 轮次 | 基线 MRR(函数) | caller | callee | both |
|---|---|---|---|---|
| 第 1 轮 | 0.867 | 0.809（−6.7%） | 0.870（+0.3%） | 0.823（−5.1%） |
| 第 2 轮（存档） | 0.853 | 0.798（−6.4%） | 0.856（+0.3%） | 0.812（−4.8%） |

进程内 4 次完全一致，**跨进程基线漂了 0.014（约 1.6%）**。原因可查：`code_chunks.chunk_id` 是
`uuid PRIMARY KEY DEFAULT gen_random_uuid()`，每次重建语料都换一批随机 id；向量是词袋哈希，碰撞多、大量候选距离相同；
向量检索走 HNSW（`hnsw (embedding vector_cosine_ops)`）近似图。随机 id 改变索引构建顺序，
同距离候选的次序随之改变，top-20 边界上换一两条就会挪动一条 query 的名次。

三个后果：

1. **只有"同一进程内连续跑的四组"可比**，所以实验设计把四组塞进同一个进程、同一个 query 循环；
2. callee 的 **+0.3% 小于跨运行漂移（约 1.6%）**，所以它不能被读成增益，只能读成"与基线不可区分"；
3. 方向之间的排序在两轮里完全稳定（caller < both < 基线 ≈ callee），**结论对漂移不敏感**——
   这是本报告敢下决策的依据，而不是那 0.3%。

## 6. 决策（规则预先写死）

规则与 `bench/retrieval/graph_direction.go:decideDirection()` 里的实现一一对应，跑之前定好，
"基线"取无图 rrf 的函数级 MRR 中位数：

| 规则 | 条件 | 动作 |
|---|---|---|
| 1 | 任一路 ≥ 基线 | 只保留该路（两路都 ≥ 取更高者） |
| 2 | 两路都 < 基线 | 默认关闭图路 |
| 3 | 单路略正（0 ≤ Δ < +1%）而双向综合为负 | 拆开关，默认只开单路 |

本轮实测：caller −6.4%（< 基线）、callee +0.3%（≥ 基线且落在 +1% 带内）、both −4.8%（< 基线）。

- 命中**规则 3**：拆开关，`GraphDirection` 默认 `callee`，`caller` / `both` 保留为可选值；
- 不命中规则 2（因为 callee 没掉到基线以下），不命中规则 1（因为那 0.3% 不具备显著意义，且双向为负）。

落地边界：`pkg/codeindex` 已支持三档方向，**服务层 `services/harnessloop-service/internal/service/graph_expand.go`
尚未透出该开关**（不在本线文件域内），所以当前线上行为仍是双向、零变化。要让"默认 callee"真正生效，
需要在服务层 `GraphPolicy` 加一个 direction 字段——留给后续任务，本线不动。

## 7. 改动

| 文件 | 改动 |
|---|---|
| `pkg/codeindex/graph.go` | 新增 `GraphDirectionBoth` 常量与 `NormalizeGraphDirection()`（空串/未知值回退 both）；`GraphExpandOptions.Direction`；`graphEdges` 按方向拼 UNION 臂——单臂时该侧独占 per-seed 额度，双臂时沿用原排序与原额度 |
| `pkg/codeindex/search.go` | 新增 `SearchOptions.GraphDirection`，透传到 `GraphExpandOptions`；`SearchStats.GraphCallers/Callees` 沿用既有统计 |
| `bench/retrieval/graph_direction.go` | 新增方向诊断跑批：`-graph-direction -repeats N`，输出区间/逐次/决策，`-json` 逐行输出 |
| `bench/retrieval/run.go` | 注册 `-graph-direction` / `-repeats`；语料遍历改为共享 `corpusSkipDirs`，排掉 IDE/agent 临时目录；新增 `dropMissingGroundTruth()` 剔除 ground truth 已消失的样本 |

**零行为变化保证**：`Direction` 零值为空串 → 归一化为 `both` → UNION 双臂、排序、额度与改动前逐字一致；
服务层未传该字段，线上路径不变。

两个顺带修掉的评测脆弱点（都在 `bench/retrieval/**` 内，不影响 `pkg/`）：

- 语料遍历原先会走进 `.codeartsdoer/temp/`，那里的文件可能在采样与读取之间被清理，
  直接让整轮评测死掉（本次实测中断过一次）；
- `queries.jsonl` 里 `q012` 的 ground truth 就是这样一个已消失的临时文件。
  处理方式是**剔除该条并打印**，不重写 `queries.jsonl`——评测集是提交物，不该为了跑通而静默换集。

## 8. 后续（都不在本线）

1. 服务层 `GraphPolicy` 加 direction，把"默认 callee"落到线上；同时补 `graph_expand.go` 的方向统计测试。
2. **013（D 线）合入后只复跑延迟与 MRR 复核，不重做方向判定**——方向结论已经定死，013 只回答"换了索引之后这三个数字是否还站得住"。
3. 想把 callee 从"不可区分"变成"确有增益"，需要人工标注集；继续在自动采样集上调 seed/衰减/权重只会重演上一轮的稀释，
   这条线上已经证明过一次，不再重复。

## 附录 复现

```bash
# 方向诊断（4 组 × 4 次重复）
go run ./bench/retrieval -graph-direction -repeats 4

# 只要逐行 JSON（含 p50/p95、各次明细）
go run ./bench/retrieval -graph-direction -repeats 4 -json

# 验收
go test ./pkg/codeindex/...
go build ./...
go run ./scripts/depcheck .
```

环境：Windows（Git Bash）、Go 1.22、容器 `hlp-pg-test`（`pgvector/pgvector:pg16`，127.0.0.1:55432）。
