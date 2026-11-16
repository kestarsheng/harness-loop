# P2 性能压测报告（T-P2-T1）

- 分支：`feature/p2-t1-perf`（worktree `hlp-p2-t1-perf`）
- 文件域：`bench/p2/perf-suite.md`、`bench/p2/k6/**`、`bench/p2/scripts/**`
- 未改：`pkg/codeindex/**`（一行未动）、`bench/repo-500/**`（只读取夹具与快照，输出一律写 `runtime/`）
- 需求范围：DFX-P-01 ~ DFX-P-09（design 9.7）

## 1. 结论

七类场景（索引两类合并计）逐条对照门槛，**四类达标、三类存在偏差**，偏差均已定位到具体原因，其中一条是既有实现的缺陷而非环境限制。

| # | 场景 | 门槛 | 实测 | 结论 |
|---|---|---|---|---|
| 1 | 单文件增量索引 | P95 ≤ 2s / P99 ≤ 5s | **P95 53ms / P99 57ms**（20 次顺序重写） | PASS |
| 2 | 100 文件批量索引 | P95 ≤ 60s，检索不阻塞 | **P95 90ms**（整批 1.55s）；索引期间探针 **14/14 成功** | PASS |
| 3 | 代码混合检索 | P95 ≤ 500ms | **P95 36ms / P99 46ms**（50 query × 10 轮 = 500 次，0 失败） | PASS |
| 4 | 记忆召回 | P95 ≤ 300ms | 未测到端到端 | **偏差**（缺 embedding 服务，见 4.2） |
| 5 | 阶段验收 | P95 ≤ 10s（程序路径） | **P95 13.58s**（含 `go build`）／**< 1ms**（纯进程内判据） | **偏差 + 归因**（见 4.3） |
| 6 | 事件端到端 | P95 ≤ 1s | **P95 402ms**（5 任务全收）／**P95 527ms 但只收到 11% 事件**（50 任务） | **达标但漏事件**（见 4.1，P0 级） |
| 7 | 50 卡片大屏 | 单帧 ≤ 16ms，无内存增长 | 服务端数据面已压；**前端帧率未测** | **偏差**（见 4.4） |
| 8 | 20 并发内存 | RSS ≤ 4GB；单任务 ≤ 256MB | **峰值 40.67MB；单任务增量 0.59MB** | PASS |

一句总结：**索引、检索、内存三类余量充足（一到两个数量级），验收卡在子进程开销上，事件流则不是慢而是漏——这条必须在 P2 出口前修掉。**

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

| 规模 | 事件 | P50 | P95 | P99 | Max |
|---|---|---|---|---|---|
| 5 源任务 | 20/20 全收 | 261 ms | **402 ms** | 402 ms | 417 ms |
| 50 源任务 | **22/200（11%）** | 405 ms | 527 ms | 557 ms | 580 ms |

P95 都在 1s 内，但 50 任务那行的「事件」列才是重点：**应到 200 条，实到 22 条**。延迟数字再好看也不能掩盖丢了 89% 的事件——见 4.1。

延迟本身主要来自服务端 500ms 的 `WatchEvents` 轮询 ticker（`grpc_handler.go:348`），均匀分布下均值约 250ms，与实测 P50 = 261ms 吻合。也就是说这条链是**轮询驱动的伪推送**，P95 的余量来自 tick 周期而非网络。

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

### 4.1 P0｜事件流整段丢失：`seq` 是 per-task 的，却被跨任务当游标用

**现象**：50 个任务并发，预期 200 条 `stage.transition`，SSE 只收到 **22 条（11%）**，且分布在 9 个 task 上、每个 task 只有第一条（`seq=1`）能过。

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

**为什么本任务没修**：修复点在 `services/harnessloop-service/**` 与 `services/gateway/**`，不在 T-P2-T1 的文件域（`bench/p2/**`）。按任务约定停下回报，归实现线处理。可能的修法（供参考，未验证）：
- 方案 A：把 `seq` 改成租户级全局序列（`nextval` 或租户级 `MAX(seq)+1`），游标语义自洽；
- 方案 B：游标改为 `(task_id, seq)` 二元组或 `timestamp_ms + event_id`，SQL 改成 `WHERE (ts, event_id) > ($1, $2)`；
- 方案 C：`WatchFrom` 按 `taskIDs` 分组取 `MAX(seq)`，游标也按任务存 map。

**当前压测如何取得可用数字**：`events` 程序带 `-purge` 先清空本租户事件，使 catch-up 为空、`since` 停在 0，此时实时流不被误滤。5 任务那行（20/20 全收、P95 402ms）就是在这个条件下测到的**真实推送延迟**；50 任务那行的漏事件比例则是缺陷本身的证据。两个数字分别代表「链路速度」与「链路完整性」，不要混读。

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

### 4.4 大屏前端帧率未测

DFX-P-08 要求「50 卡片 × 1s × 10min，主线程单帧 ≤ 16ms」。本轮**只压了服务端数据面**（50 VU SSE + `k6/monitor.js` 的轮询回退路径），前端帧率没有实测。

**原因**：`frontend/` 无 `node_modules`，`npm install` 会在 `frontend/` 下写入依赖目录，超出本任务文件域；且帧率测量需要 headless Chrome 渲染真实页面 10 分钟，属独立工作量。

**已交付**：`bench/p2/k6/monitor.js`（50 VU × 1s 轮询，阈值 `p(95)<200ms`、`http_req_failed<1%`）。k6 无原生 SSE 模块（需 xk6-sse 扩展），故 SSE 主路径用 Go 的 `events` 程序测，脚本注释里写明了这个分工，不混口径。

### 4.5 其它限制

- **vector 路缺数据**：代码检索与记忆召回的向量路都因夹具/环境没有 embedding 而空跑（见 3.2、4.2）。
- **事件流长度**：`WatchEvents` 是 500ms 轮询，P95 天然带 500ms 量级的下限；若未来要求 P95 < 200ms，必须改成真推送（LISTEN/NOTIFY 或内存扇出），不能靠调小 tick。
- **断线补齐未实测**：DFX-P-07 后半句「断线重连 5s 内补齐」依赖 `last_seq`，而 `last_seq` 正是 4.1 里被证明语义错位的那个值。游标语义修好之前，这项测了也没有意义。
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
```

产物一律写 `bench/p2/scripts/runtime/`：

| 文件 | 内容 |
|---|---|
| `index-baseline.json` | 全量/单文件/批量索引 + 探针 |
| `search.json` | 代码检索 500 次分位 |
| `acceptance.json` / `acceptance-light.json` | 验收全量 / 仅进程内（归因用） |
| `events-5.json` / `events-50.json` | 事件端到端 5 任务 / 50 任务 |
| `memload-20.json` | 20 并发 RSS 曲线 |

脚本自解释：`lib-service.sh` 负责构建二进制、冷启动服务、等端口就绪、收尸；每个场景脚本只描述「测什么、门槛多少」。服务生命周期由脚本自己管——不是洁癖，而是基线内存必须来自刚启动的进程，否则上一轮没回收的堆会把 `per_task` 算小。

## 6. 改动清单

| 路径 | 类型 | 说明 |
|---|---|---|
| `bench/p2/perf-suite.md` | 新增 | 本报告 |
| `bench/p2/k6/monitor.js` | 新增 | 50 VU × 1s 大屏轮询压测（k6） |
| `bench/p2/scripts/lib-service.sh` | 新增 | 构建/启停/就绪探测公共库 |
| `bench/p2/scripts/run-all.sh` | 新增 | 七类场景总入口 |
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

未触碰：`pkg/codeindex/**`、`bench/repo-500/**`（只读）、`services/**`、`go.mod`。

平台相关实现说明：RSS 采样在 Windows 走 `psapi!GetProcessMemoryInfo`（`syscall` 标准库直连，不引入 `x/sys` 以免动 `go.mod`），其它平台读 `/proc/<pid>/status`，两个实现语义一致（物理常驻集）。
