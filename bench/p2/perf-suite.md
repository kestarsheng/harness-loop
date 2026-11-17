# P2 性能压测报告（T-P2-T1）

- 分支：`feature/p2-t1-perf`（worktree `hlp-p2-t1-perf`）
- 文件域：`bench/p2/perf-suite.md`、`bench/p2/k6/**`、`bench/p2/scripts/**`
- 未改：`pkg/codeindex/**`（一行未动）、`bench/repo-500/**`（只读取夹具与快照，输出一律写 `runtime/`）
- 需求范围：DFX-P-01 ~ DFX-P-09（design 9.7）

## 1. 结论

七类场景（索引两类合并计）逐条对照门槛，**五类达标、三类存在偏差**（其中第 7 项由「未测」转为「部分达标」），偏差均已定位到具体原因，其中一条是既有实现的缺陷而非环境限制。

| # | 场景 | 门槛 | 实测 | 结论 |
|---|---|---|---|---|
| 1 | 单文件增量索引 | P95 ≤ 2s / P99 ≤ 5s | **P95 53ms / P99 57ms**（20 次顺序重写） | PASS |
| 2 | 100 文件批量索引 | P95 ≤ 60s，检索不阻塞 | **P95 90ms**（整批 1.55s）；索引期间探针 **14/14 成功** | PASS |
| 3 | 代码混合检索 | P95 ≤ 500ms | **P95 36ms / P99 46ms**（50 query × 10 轮 = 500 次，0 失败） | PASS |
| 4 | 记忆召回 | P95 ≤ 300ms | 未测到端到端 | **偏差**（缺 embedding 服务，见 4.2） |
| 5 | 阶段验收 | P95 ≤ 10s（程序路径） | **P95 13.58s**（含 `go build`）／**< 1ms**（纯进程内判据） | **偏差 + 归因**（见 4.3） |
| 6 | 事件端到端 | P95 ≤ 1s | **P95 785ms，200/200 事件全收**（50 任务，T-P2-17 修复后复跑） | PASS（见 4.1） |
| 7 | 50 卡片大屏 | 单帧 ≤ 16ms，无内存增长 | 静止/轮询刷新 **P95 8.60ms**（>16.7ms 占 0.88%）；滚动换行 **P95 39.32ms**；筛选切换 **P95 88.04ms**；JS 堆 120s **+2.95MB** | **部分达标**（见 4.4） |
| 8 | 20 并发内存 | RSS ≤ 4GB；单任务 ≤ 256MB | **峰值 40.67MB；单任务增量 0.59MB** | PASS |

一句总结：**索引、检索、内存三类余量充足（一到两个数量级），验收卡在子进程开销上；事件流原先是「漏」不是「慢」（11% 收集率），T-P2-17 换租户级 `global_seq` 游标后复跑 200/200、P95 785ms，已消除。**

> 修订记录：
> 1. T-P2-17（事件游标语义修复）合入后，本报告复跑了 3.3 的事件场景并重写了 4.1（原「P0 未修复」→「已修复」）。
> 2. T-P2 质量收口轮补齐了 4.4 的前端帧率实测（原「未测」→「实测」），并同步第 1 节结论表第 7 行；同时修掉 4.4 里「`frontend/` 无 `node_modules`、装依赖超文件域」这条已不成立的原因。其余章节维持 T-P2-T1 当时的实测，未重新计时。

## 2. 基准环境（可复现的前提）

| 项 | 值 |
|---|---|
| OS / CPU | Windows 11 / Intel Core Ultra 5 125H（18 逻辑核 / 14 物理核） |
| 内存 | 32 GB |
| Go | go1.22.12 windows/amd64 |
| PostgreSQL | 容器 `hlp-pg-test`（`pgvector/pgvector:pg16`，含 pgvector），`127.0.0.1:55432` |
| GOMAXPROCS | 固定 **8**（非 18，让小规格机器的索引/检索抢 CPU 更接近线上） |
| 被测服务 | `harnessloop-service:50008`、`gateway:9000`、`scripts/fakeagent:50002` |
| 夹具 | `bench/repo-500`：500 文件 / 1 437 169 字节 / 5 994 chunks / 15 482 edges |
| 快照指纹 | `sha256:a19bf169980db008e20916f06d0fcfad087e3d16247769ed3ff33c25129d1a59`（`-mode check` 逐文件校验通过） |

快照与硬件都钉死后才开始出数：夹具任一文件 sha256 不符、或 GOMAXPROCS 与 `hardware.json` 不一致，脚本会拒绝或标注 `hardware_match=false`。

## 3. 各场景实测

### 3.1 索引（DFX-P-01 / DFX-P-02）

`bench/repo-500 -mode run`，并发 4 / batch 32 / insert-batch 200。

| 阶段 | 文件 | 耗时 | P50 | P95 | P99 | 吞吐 |
|---|---|---|---|---|---|---|
| 全量 500 | 500 | 7.04 s | 42 ms | 121 ms | 269 ms | 71 文件/s |
| 单文件采样 | 20 | 824 ms | 38 ms | **53 ms** | **57 ms** | 24 文件/s |
| 批量 100 | 100 | 1.55 s | 54 ms | **90 ms** | 101 ms | 65 文件/s |

全量期间检索探针跑 14 次、成功 14 次（**100%**），命中最慢一次 347ms。这证明「批量索引期间读旧索引可用」是结构性的：单文件单事务 + MVCC + 并发度夹到 `连接池-1`，不是靠运气。

### 3.2 代码检索（DFX-P-03）

50 个 query 取自夹具里的真实符号名（`Handler00000` 这类），不是人工挑的好查词；50 × 10 轮 = 500 次调用。

| 指标 | 值 |
|---|---|
| P50 / P95 / P99 / Max | **21.2 / 35.7 / 45.7 / 76.5 ms** |
| 失败 / 空结果 | 0 / 0 |
| 平均命中 | 5.7 条（top_k=20） |
| 路命中累计 | fulltext 3 280、graph 1 710、**vector 0** |

vector 路为 0 不是 bug：夹具索引时 embedding 传 nil，向量列全为 NULL。所以**本项实测覆盖的是 keyword + graph 两路经 RRF 合并**，三路齐活的情形没有数据集可测——已在 4.5 列为限制。

### 3.3 事件端到端（DFX-P-07）

链路：`harnessloop-service 写 hlp_event.events` → `gateway WatchEvents(gRPC)` → `MonitorStream(SSE)` → 客户端。延迟 = 本地收到时刻 − 事件自带 `timestamp_ms`（同机同钟，无偏差）。

| 规模 | 事件（收/应发） | P50 | P95 | P99 | Max |
|---|---|---|---|---|---|
| 5 源任务 | 20/20（100%） | 248 ms | **539 ms** | 539 ms | 550 ms |
| 50 源任务 | **200/200（100%）** | 475 ms | **785 ms** | 865 ms | 883 ms |

上一版这里 50 任务那行是 **22/200（11%）**：`seq` 是 per-task 序号却被当跨任务游标用，新任务的事件被 `seq > since` 整段滤掉。T-P2-17 把游标换成租户级 `global_seq` 后复跑，**200 条全收、`seq` 单调无跳号、补拉不重不漏**，延迟仍留 21% 余量。复跑与判定口径见 4.1。

延迟本身主要来自服务端 500ms 的 `WatchEvents` 轮询 ticker（`grpc_handler.go:348`），均匀分布下均值约 250ms，与实测 P50 = 475ms 的量级吻合（50 任务时数据库侧还要分摊 50 个任务的并发写入）。也就是说这条链是**轮询驱动的伪推送**，P95 的余量来自 tick 周期而非网络。

### 3.4 验收（DFX-P-06）

确定性路径：`Assemble → ProgramStageVerifier.Verify → checks.Registry 执行判据`，全程不调模型。判据两条：`artifact_exists(main.go)`（进程内）+ `build_pass`（真起 `go build` 子进程）。

| 判据组合 | P50 | P95 | P99 | Max | 结论 |
|---|---|---|---|---|---|
| 含 `build_pass` | 8.13 s | **13.58 s** | 13.58 s | 19.98 s | **超 10s 预算** |
| 仅进程内判据 | 0 ms | **< 1 ms** | < 1 ms | 0.53 ms | 远低于预算 |

20 次迭代 verdict 全部 `pass`，0 错误。超标不在验收实现：两组相差三个数量级，说明**耗时 100% 来自 `go build` 子进程**。

单独测纯 `go build ./...`（stdlib-only 工程）：real 6.3–7.2s，而 user 时间仅 0.18–0.22s，`/tmp` 与桌面目录无差别。即时间花在进程启动/文件系统而不是编译——这是本机 Windows 工具链的固有开销，不是代码问题。

### 3.5 20 并发内存（DFX-P-09）

20 个任务并发创建（gRPC `CreateTask`），每 200ms 采一次 `harnessloop-service` 进程 RSS（Windows 走 psapi `GetProcessMemoryInfo`，非 `tasklist` 子进程）。

| 指标 | 值 |
|---|---|
| 冷启动基线 | 28.82 MB |
| 峰值 | **40.67 MB** |
| 增量 | 11.84 MB |
| 单任务增量 | **0.59 MB** |
| 创建 P95 | 244 ms |
| 20 任务全部终态 | 2.65 s（20/20 `completed`，0 失败） |

RSS 曲线（间隔约 200–400ms，20 任务全程在跑）：

```
28.8 ─ baseline
38.7 ─ 20 running  (t=0.3s)
39.5 ─ 20 running  (t=0.7s)
40.3 ─ 20 running  (t=1.4s)
41.3 ─ 19 running  (t=2.3s)
41.8 ─ 15 running  (t=2.7s)
40.7 ─ 0 running, GC 前  (t=2.97s)
```

峰值 40.67MB 对 4GB 门槛是 **1%**，单任务 0.59MB 对 256MB 是 **0.2%**。

口径必须说清：负载是 `scripts/fakeagent`（每阶段回 `stage_finish`，不接真实 LLM），所以测到的是**状态机 + 事件 + 检查点的框架开销**，不含真实模型上下文。`capacity-report.md` 里的「20 × 256MB = 5GB」是设计预算上限的推算；实测框架开销比预算低两个数量级，说明 256MB 的预算大头留给了消息历史与压缩缓冲，而这两项本轮没有真实负载可压。

## 4. 偏差与缺陷（按严重度）

### 4.1 已修复（原 P0）｜事件流整段丢失：`seq` 是 per-task 的，却被跨任务当游标用

**状态**：T-P2-17 修复并复跑通过（50 任务 **200/200，100%**）。以下保留原始定位过程，作为「怎么把 11% 找出来」的记录。

**原现象**：50 个任务并发，预期 200 条 `stage.transition`，SSE 只收到 **22 条（11%）**，且分布在 9 个 task 上、每个 task 只有第一条（`seq=1`）能过。

**根因**（两处代码合起来才成立）：

1. `services/harnessloop-service/internal/repository/event_repo.go:130`
   `SELECT MAX(seq) FROM hlp_event.events WHERE tenant_id=$1 AND task_id=$2`
   —— **seq 是每个任务各自从 1 开始的序号**，不是租户级全局序列。
2. 而 `WatchEvents`（`grpc_handler.go:344`）把 `since` 跨任务推进：
   ```
   events, next := h.events.WatchFrom(ctx, tenantID, taskIDs, since, 200)
   if next > since { since = next }        // next 是「本批最大 seq」，跨任务混合
   ```
   `WatchFrom` 的 SQL 是 `AND seq > $2`。于是 `since` 一旦被推到 5，任何 `seq ≤ 5` 的新任务事件就被永久滤掉——新任务的 `seq` 恰好是 1、2、3、4。

网关侧 `MonitorStream`（`harness_loop_handler.go:600`）同理：catch-up 阶段循环 `if e.Seq > since { since = e.Seq }`，同样把 per-task 序号当全局游标，连接建立时就把 `since` 顶到历史最大值。

**为什么 5 任务能全收**：首个任务的事件把 `since` 推到 4 之前，其它任务的 `seq=1` 事件已经在同一批里被取走；任务数一多、`since` 被顶高，后续任务的低 `seq` 事件就全被吞。

**影响**：DFX-P-07 直接不达标（不是延迟，是完整性）；DFX-P-08 的 50 卡片大屏将以同样比例丢事件，卡片状态会停在半路。**这是大屏可用性问题，不是性能问题。**

**修复（T-P2-17，方案 A：租户级全局序列）**：

1. `014_event_global_seq.sql` 给 `hlp_event.events` 加 `global_seq bigint`，按 `ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY ts, event_id)` 回填存量行，并加唯一约束 `(tenant_id, global_seq)`；迁移幂等、列保持可空（滚动升级期旧版本仍可写入）。
2. `event_repo.Append` 在事务内先取 `pg_advisory_xact_lock(hashtext(tenant))`，再用 `INSERT ... SELECT COALESCE(MAX(global_seq),0)+1` 分配，写入在租户维度严格单调；`task_repo` 的状态转移事件走同一把锁、同一口径。**没有应用层缓存**。
3. `List` / `WatchFrom` 改成 `WHERE tenant_id=$1 AND global_seq > $2 ORDER BY global_seq`；`seq` 仍是任务内序号（唯一约束 `(task_id, seq)` 与 Checkpoint 的 `event_seq_watermark` 不动）。
4. proto 的 `since_seq` / `next_since_seq` / `HarnessEvent.seq` 字段名保留、语义改为 `global_seq`，注释已写明；网关 `last_seq` 随之语义一致，前端字段不用改。

**复跑证据**（`bash bench/p2/scripts/run-events.sh -n 50`，同一台机、同一 PG 容器）：

| 指标 | 修复前 | 修复后 |
|---|---|---|
| 收到 / 应发 | 22 / 200（**11%**） | **200 / 200（100%）** |
| 覆盖 task 数 | 9 / 50 | **50 / 50** |
| P95 | 527 ms | 785 ms |
| `seq` 单调 | —（per-task 混读） | 单调、**0 跳号** |
| 补拉（`last_seq` 回放最后 3 条） | 语义错位，未测 | 正好 3 条、连续、不重不漏 |

复跑时 `events` 程序的分母不再是「任务数 × 4」的硬编码，而是 `SELECT count(*) FROM hlp_event.events WHERE tenant_id=$1 AND task_id = ANY($2)`；分子按 `event_id` 去重。`seq` 跳号判定跑在**未按 task 过滤**的原始流上——大屏订阅的是整个租户，子任务的事件本来就占号，用「只留本轮任务」的样本判跳号会误报（上一轮就误报过 11 次）。

### 4.1.1 事件游标仍有的边界（不阻塞验收）

- **租户内子集订阅会出现「跳号」**：`WatchEvents` 支持按 `task_ids` 过滤，此时客户端只看到部分任务的事件，`global_seq` 自然不连续。这不是漏事件——是别的任务占号。大屏与 CLI 都是整租户订阅，不受影响。
- **`global_seq` 与 `ts` 不保证同序**：并发写入时两个事务的提交顺序与 `ts` 先后可能相反；`global_seq` 的语义是「分配顺序」，用 `ts` 排序的报表不要拿它当时间轴。
- **回滚会烧号**：事务回滚会把分配过的 `global_seq` 一起回滚（`MAX+1` 从表里读，不落计数器），所以正常情况下连续；但如果写入路径将来改成独立计数器表，就要接受「回滚留下空洞」。当前实现不留空洞。

### 4.2 记忆召回（DFX-P-05）未取得端到端数字

**原因**：`memory-service` 的向量路（`repository/memory_repository.go:117 Search`）要求 Qdrant；而 `Save`/`Recall` 的语义触发都依赖**真实 embedding 调用**（DashScope `text-embedding-v3`），本机无可用 API key。Qdrant 镜像虽在本地（`qdrant/qdrant:latest`），但没有向量数据可查，起服务也只能跑全文路。

**可用替代证据**：`bench/p1/recall-report.md` 已有 P1 实测（PG + pgvector，3 条主题、1024 维假向量）：纯全文 8–13ms、全文+向量+RRF 9–12ms，`TestRecallLatencyBudget` 断言单次 < 1s 通过。但那是 `stats.LatencyMs` 的包内计时，样本 3 条，**不能替代 300ms 门槛下的 P95**。

**结论**：DFX-P-05 本轮**不下结论**。要补齐需：可用的 embedding 端点（真实或本地 mock 服务）+ ≥1000 条主题数据，跑 50 query 取 P95。

### 4.3 验收超标，但归因明确（非实现问题）

P95 13.58s > 10s，但同工程下「仅进程内判据」P95 < 1ms。差值全部是 `go build` 子进程（real 6.3–7.2s，user 0.2s）。三条可选处置：
1. 判据分层：把 `build_pass` 这类子进程判据的预算单列（例如 20s），10s 预算只约束进程内判据；
2. 工具链预热：CI 上 warm build cache 后再测，可把 7s 压到亚秒级；
3. 换判据：`static_check`（`go vet`，同样起子进程，开销类似）不解决问题，本质是「起 Go 工具链」贵。

本轮不改门槛也不改判据，只把两组数字并列，让后续判断有依据。

### 4.4 大屏前端帧率（DFX-P-08）：已实测

DFX-P-08 要求「50 卡片 × 1s × 10min，主线程单帧 ≤ 16ms，无内存增长」。上一版这里只压了服务端数据面、前端帧率**未测**；本轮补齐了渲染面实测，原始数据在 `bench/p2/runtime/board-frames-50.json`。

**怎么量的**（`bench/p2/scripts/cmd/boardperf`，chromedp 驱动真 Chrome）：

- 全栈由 `scripts/dev-up.sh --install` 拉起（PG `hlp-pg-test` + harnessloop-service:50013 + gateway:9000 + vite:5174），测的就是 `http://localhost:5174/harness-loop/monitor` 这个真实看板；
- 卡片数据用 `bench/p2/scripts/seed-board-tasks.sql` 灌 50 条 `hlp_task.task_runs`（幂等），库内当时共 59 条任务、看板全量可见；
- 页面内用 `requestAnimationFrame` 自采样 120s 帧间隔（14 014 帧），并用 `PerformanceObserver('longtask')` 记长任务；交互用「派发事件 → 等两次 rAF」量到绘制完成；JS 堆在窗口前后各取一次（`--enable-precise-memory-info` + `--expose-gc`）；
- 视口 1920×1080，`headless=new`，有头 Chrome 未跑（见末尾「未覆盖」）。

**帧间隔（120s，14 014 帧）**

| 指标 | 实测 | 门槛 |
|---|---|---|
| P50 | **8.30 ms** | — |
| **P95** | **8.60 ms** | ≤ 16 ms ✅ |
| P99 | 16.30 ms | — |
| Max | 41.70 ms | — |
| > 16.7 ms 占比 | **0.88 %** | — |
| 反推刷新率 | ≈ **120 Hz** | — |

**交互延迟（各 20 次）**

| 交互 | P50 | **P95** | Max | 净耗时（减 raf-baseline） |
|---|---|---|---|---|
| `raf-baseline`（只等两帧） | 6.65 ms | 11.92 ms | 12.30 ms | — |
| 状态筛选切换 | 61.65 ms | **88.04 ms** | 90.60 ms | ≈ 76 ms |
| 滚动虚拟列表一屏 | 22.55 ms | **39.32 ms** | 39.80 ms | ≈ 27 ms |

**内存与长任务**：JS 堆 44.83 MB → 47.78 MB，**120s 增 2.95 MB**（无持续增长迹象，但 2 分钟的窗口不足以证明「10 分钟不涨」）；长任务 6 次 / 316 ms，最大 54 ms，折合 **0.05 次/秒**。

**怎么读这组数**

1. **静止与数据刷新达标**：P95 8.60 ms，14k 帧里只有 0.88 % 超过 16.7 ms。看板每 10s 重拉一次任务列表、SSE 到达时局部更新，这段时间的开销都包含在这个分位数里。
2. **本机是 120 Hz，门槛其实是宽的**：P50 8.3 ms 反推 120.5 Hz，真实帧预算 8.3 ms。按 60 Hz 的 16 ms 口径判「达标」，但按屏幕预算算 P95 已经贴着上限——报告同时给两个口径，避免数字被误读成「余量一倍」。
3. **「50 卡片」在 DOM 上不是 50 张**：`AgentCardGrid` 是虚拟列表（`CARD_HEIGHT=148` + 可视区 + 2 行 overscan），首屏只渲染 **12** 张；脚本额外滚遍全表，`aria-label` 去重覆盖 **59/59**，证明 50 张卡都渲染过，只是不是同时。所以上表的帧数据对应的是「12 张同屏 + 滚动换行」的真实成本，不是「50 张同屏」。
4. **两处超预算，归因明确**：滚动换行 P95 39.32 ms（净 27 ms）——一次换行要挂载 6 张卡、丢弃 6 张卡，React 提交 + antd 卡片内部布局都在这一次帧里；筛选切换 P95 88.04 ms（净 76 ms）——`Segmented` 切换会重建整个 `states` 数组并重渲染可视区。两者都超过 16 ms，**如果 DFX-P-08 的口径包含交互帧，这两项就是不达标**；若只约束「静止展示帧」，则达标。判据边界需要产品侧确认，本报告不替它下结论。

**未覆盖（不要当成已验）**：① 10 分钟长跑只跑了 120s，内存曲线是短窗口；② 1s 节拍的持续更新没有真造——看板自身是 10s 轮询 + SSE 事件驱动，脚本没有合成 1s 心跳，所以「每秒全量重渲染 50 张卡」这一最坏情形未测；③ 只用 headless Chrome，有头 Chrome 的合成器路径可能不同；④ 未测 4K/小屏等其它视口下的列数变化。

**复现**：`scripts/dev-up.sh --install` → `bash bench/p2/scripts/run-board-perf.sh -n 50 -s 120 -i 20`。

**数据面另计**：`bench/p2/k6/monitor.js`（50 VU × 1s 轮询，阈值 `p(95)<200ms`、`http_req_failed<1%`）覆盖的是接口侧；k6 无原生 SSE 模块（需 xk6-sse 扩展），故 SSE 主路径用 Go 的 `events` 程序测，脚本注释里写明了这个分工，不混口径。

### 4.5 其它限制

- **vector 路缺数据**：代码检索与记忆召回的向量路都因夹具/环境没有 embedding 而空跑（见 3.2、4.2）。
- **事件流长度**：`WatchEvents` 是 500ms 轮询，P95 天然带 500ms 量级的下限；若未来要求 P95 < 200ms，必须改成真推送（LISTEN/NOTIFY 或内存扇出），不能靠调小 tick。
- **断线补齐**：游标语义修好后已实测——用倒数第 3 条之前的 `last_seq` 重连，网关 catch-up 回放的正好是最后 3 条、`global_seq` 连续、`synced` 帧把 `last_seq` 推到最新（见 4.1 复跑表）。「5s 内补齐」的时延本身没有单独压测，但补齐路径是一次 `ListEvents`，5 任务场景同一接口 P95 在 200ms 量级。
- **内存口径**：见 3.5，桩负载，不含真实模型上下文。

## 5. 复现

```bash
cd hlp-p2-t1-perf
export HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable'
export GOMAXPROCS=8

# 七类场景一次跑完（索引→检索→验收→事件 5/50→内存）
bash bench/p2/scripts/run-all.sh

# 或单项
bash bench/p2/scripts/run-search.sh -q 50 -r 10
bash bench/p2/scripts/run-acceptance.sh -n 20
bash bench/p2/scripts/run-events.sh -n 5
bash bench/p2/scripts/run-events.sh -n 50
bash bench/p2/scripts/run-memload.sh -n 20

# 大屏前端帧率（先起全栈，再量渲染面；与上面的数据面脚本互不依赖）
scripts/dev-up.sh --install
bash bench/p2/scripts/run-board-perf.sh -n 50 -s 120 -i 20
```

首次跑需要 `bench/p2/scripts/runtime/harnessloop.yaml` 与 `gateway.yaml`（`runtime/` 已 gitignore）：前者填 PG DSN，后者须给 `services.harness_loop: 127.0.0.1:50008` 与 `http_port: 9000`（`NewChatHandler` 用空 target 会直接 panic）。`lib-service.sh` 会从当前源码重建 harnessloop-service / gateway / fakeagent 三个二进制——网关也在被测链路上，用上一轮遗留的 `gateway.exe` 会测到旧游标语义。

产物一律写 `bench/p2/scripts/runtime/`：

| 文件 | 内容 |
|---|---|
| `index-baseline.json` | 全量/单文件/批量索引 + 探针 |
| `search.json` | 代码检索 500 次分位 |
| `acceptance.json` / `acceptance-light.json` | 验收全量 / 仅进程内（归因用） |
| `events-5.json` / `events-50.json` | 事件端到端 5 任务 / 50 任务 |
| `memload-20.json` | 20 并发 RSS 曲线 |
| `board-frames-50.json` | 大屏帧间隔 / 交互延迟 / JS 堆（`bench/p2/runtime/`） |

脚本自解释：`lib-service.sh` 负责构建二进制、冷启动服务、等端口就绪、收尸；每个场景脚本只描述「测什么、门槛多少」。服务生命周期由脚本自己管——不是洁癖，而是基线内存必须来自刚启动的进程，否则上一轮没回收的堆会把 `per_task` 算小。

## 6. 改动清单

| 路径 | 类型 | 说明 |
|---|---|---|
| `bench/p2/perf-suite.md` | 新增 | 本报告 |
| `bench/p2/k6/monitor.js` | 新增 | 50 VU × 1s 大屏轮询压测（k6） |
| `bench/p2/scripts/lib-service.sh` | 新增 | 构建/启停/就绪探测公共库 |
| `bench/p2/scripts/run-all.sh` | 新增 | 七类场景总入口 |
| `bench/p2/scripts/cmd/boardperf` | 新增 | chromedp 大屏帧率/交互/内存采集器 |
| `bench/p2/scripts/seed-board-tasks.sql` | 新增 | 50 个看板任务种子（幂等） |
| `bench/p2/scripts/run-board-perf.sh` | 新增 | 大屏帧率场景入口 |
| `bench/p2/runtime/board-frames-50.json` | 新增 | 帧率实测原始数据（留档） |
| `bench/p2/scripts/run-memload.sh` | 新增 | 20 并发内存 |
| `bench/p2/scripts/run-events.sh` | 新增 | 事件端到端 |
| `bench/p2/scripts/run-search.sh` | 新增 | 代码检索 |
| `bench/p2/scripts/run-acceptance.sh` | 新增 | 验收（含归因对照） |
| `bench/p2/scripts/cmd/memload/` | 新增 | Go 压测程序（`main.go` + `rss_windows.go` / `rss_other.go`） |
| `bench/p2/scripts/cmd/events/` | 新增 | SSE 端到端测量程序 |
| `bench/p2/scripts/cmd/searchbench/` | 新增 | 代码检索压测程序 |
| `bench/p2/scripts/cmd/acceptbench/` | 新增 | 验收压测程序 |
| `bench/p2/scripts/testdata/acceptfix/` | 新增 | 验收判据用的最小可构建工作区 |
| `bench/p2/scripts/runtime/*.yaml` | 新增 | 压测运行时配置（DSN、端口、关闭记忆与审批依赖） |

T-P2-17 复核时的改动（第 2 次修订）：

| 路径 | 类型 | 说明 |
|---|---|---|
| `bench/p2/scripts/cmd/events/main.go` | 修改 | 增加收集率（分母取服务端 `count(*)`，分子按 `event_id` 去重）、`seq` 单调/跳号判定、`last_seq` 补拉探针 |
| `bench/p2/scripts/lib-service.sh` | 修改 | 构建列表补上 `gateway.exe`（原来漏了，复跑会用到上一轮的旧二进制） |
| `bench/p2/scripts/run-events.sh` | 修改 | 头部注释去掉「必须 purge 才能绕开缺陷」的旧说明 |

未触碰：`pkg/codeindex/**`、`bench/repo-500/**`（只读）、`services/**`、`go.mod`。

平台相关实现说明：RSS 采样在 Windows 走 `psapi!GetProcessMemoryInfo`（`syscall` 标准库直连，不引入 `x/sys` 以免动 `go.mod`），其它平台读 `/proc/<pid>/status`，两个实现语义一致（物理常驻集）。
