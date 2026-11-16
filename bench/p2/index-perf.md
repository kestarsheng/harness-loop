# P2 索引线：全量索引并发化与压测报告

- 分支：`feature/p2-index-batch`（worktree `hlp-p2-index-batch`）
- 基线：`3b69019`（与 `develop` 同基）
- 文件域：`pkg/codeindex/{batch.go,writer.go}`、`internal/cli/cmd/index.go`、`internal/cli/cmd/root.go`（+1 行注册）、`bench/repo-500/**`、`bench/p2/index-perf.md`；`go.mod` 未动。

## 1. 结论

| 验收项 | 门槛 | 实测 | 结果 |
|---|---|---|---|
| 单文件 P95 | ≤ 2 s | **60 ms** | PASS |
| 单文件 P99 | ≤ 5 s | **64 ms** | PASS |
| 100 文件批量 P95 | ≤ 60 s | **55 ms**（整批 1.05 s） | PASS |
| 批量期间检索成功率 | 100% | **12/12 = 100%**（对照跑 191/191） | PASS |
| 1000 文件首次全量可查 | 全量后可检索 | 17.4 s 建完 11 997 chunks，收尾检索命中 5 | PASS |

首次全量 500 文件 6.20 s（80.7 文件/s），相比优化前写路径（串行 + 逐 chunk INSERT）的 95.82 s
快 **15.5×**；100 文件批量从 24.92 s 降到 1.05 s（**23.6×**）。

## 2. 改了什么

### 2.1 `pkg/codeindex/batch.go`（新增）

批量索引器，把「并发度」和「批量大小」做成两个可调旋钮：

| 字段 | 含义 | 默认 |
|---|---|---|
| `Concurrency` | 并行索引的 worker 数 | 4 |
| `BatchSize` | 每批文件数（进度回报 / checkpoint 粒度） | 32 |
| `InsertBatch` | 单条多行 INSERT 的行数 | 200 |
| `EmbedBatch` | 单次 embedding 调用的文本条数 | 沿用 writer 默认 64 |

- `CollectBatchFiles` 确定性遍历（相对路径字典序），沿用 `DefaultIgnore` 的目录/后缀/大小规则，跳过符号链接。
- `IndexBatch` 是通用入口；`IndexFull` = 强制重写全部文件（`Force` + 跳过 `indexed_files` 预检，每文件省一次往返）。
- 并发度会被夹到 `min(16, 连接池上限-1)`：**永远留一条连接给检索**，这是「批量期间检索成功率 100%」的结构性保证，而不是运气。
- 单文件失败不中断整批：记进 `stats.Failures`，由调用方决定 exit code。
- `PoolStatus()` 暴露连接池占用，CLI 与压测脚本都用它判断余量。

### 2.2 `pkg/codeindex/writer.go`（改）

| 改动 | 原因 |
|---|---|
| `IndexOptions{Force, SkipStateLookup, InsertBatch, EmbedBatch}` + `IndexFileWith` | 全量模式跳过预检往返；批量参数逐文件生效，不需要临时改 writer 状态 |
| `insertChunks` / `insertEdges` 改多行 INSERT | 单文件几十个 chunk 时往返次数从 N 降到 ⌈N/InsertBatch⌉，是吞吐的主要来源 |
| `embedTexts(texts, batch)` | embedding 批量可配；失败仍降级为 NULL 向量，行为与历史一致 |
| `SetEmbedBatch` / `SetInsertBatch` | 运维侧可调，不需要重编译 |

`IndexFile` 签名与语义保持不变（内部委托 `IndexFileWith`），既有调用方与契约测试零改动。

**没有改的部分同样重要**：写入仍是「单文件单事务」——同一事务里 `DELETE` 该文件的 chunks/edges →
`INSERT` 新版本 → upsert `indexed_files` → `COMMIT`，并带 `pg_advisory_xact_lock(tenant|project|path)`。
全量过程没有 `TRUNCATE`、没有跨文件的批量 `DELETE`、没有表级锁。

### 2.3 `internal/cli/cmd/index.go`（新增）+ `root.go`（+1 行）

```bash
harness-cli index --path . --project-id proj-x --full \
  --concurrency 4 --batch-size 32 --insert-batch 200 \
  --probe-query handler --verify-query handler
```

- 直连 PG（`--dsn` / `HARNESS_PG_DSN`），不走单文件 gRPC：全量 1000 文件时逐次 RPC 会把吞吐压死。
- `--full` 全量重写；不带则按 `content_hash` 跳过未变更文件（实测二次跑 22 个文件全 skipped、92 ms）。
- `--probe-query` 在索引期间按 `--probe-interval` 反复检索，现场统计成功率——这是「读旧索引不被阻塞」的**取证**，不是推断。
- `--verify-query` 收尾检索一次并要求至少 1 条命中——「首次全量可查」的判据。
- 退出码复用既有约定：0 成功 / 2 有文件失败 / 4 参数错 / 5 连不上。

## 3. 为什么「全量期间读旧索引不阻塞」

三层机制叠加，缺一层都不成立：

1. **MVCC**：每个文件的 chunks 在同一事务里被替换，提交前读方看到的仍是上个版本的完整行集，
   提交瞬间原子切换——不存在「删了还没写」的中间态。
2. **无跨文件批量操作**：`DELETE` 只针对当前 `file_path`，没有 `TRUNCATE`、没有 `DELETE ... WHERE tenant_id=...`，
   所以不会把整张表锁在写事务里。
3. **连接池留余量**：并发度上限 = `maxConns - 1`。检索永远能拿到连接，不会卡在等连接上。

探针用的就是业务检索同一条 `CodeWriter.Search`（keyword ∪ vector ∪ graph + RRF）。全量 500 文件期间
12~191 次探针全部成功，`success_rate = 100%`。

## 4. 压测方法：固定快照 + 固定硬件

夹具与脚本在 `bench/repo-500/`，两道闸门：

- **仓库快照固定**：500 个文件的 sha256 记在 `testdata/repo/snapshot.json`，整体 digest
  `sha256:a19bf169…d1a59`。`-mode check` 逐条比对，任一文件漂移直接拒绝出数。
- **硬件固定**：`testdata/repo/hardware.json` 记录逻辑核数 / OS / Go / PG / `GOMAXPROCS` / 并发度 / 批量；
  脚本 `export GOMAXPROCS=8`，核数或 GOMAXPROCS 不符时结果标 `hardware_match=false`。

压测环境：

| 项 | 值 |
|---|---|
| CPU | Intel Core Ultra 5 125H（18 逻辑核 / 14 物理核） |
| 内存 | 32 GB |
| OS / Go | windows/amd64 · go1.22.12（`GOMAXPROCS=8`） |
| PG | 16.15 + pgvector（独立实例 `127.0.0.1:55432`） |
| 夹具 | 500 文件 / 1 437 169 B / 5 994 chunks / 15 482 edges |
| 参数 | concurrency=4 · batch=32 · insert-batch=200 · 探针 500 ms |

## 5. 数字

### 5.1 正式跑（500 文件，`bash bench/repo-500/run.sh`）

| 阶段 | 文件 | 墙钟 | 吞吐 | P50 | P95 | P99 | max | chunks / edges |
|---|---|---|---|---|---|---|---|---|
| `full_index` | 500 | 6.20 s | 80.7 files/s | 42 ms | 83 ms | 126 ms | 158 ms | 5 994 / 15 482 |
| `single_file_sample` | 20 | 0.82 s | 24.3 files/s | 37 ms | 60 ms | 64 ms | 64 ms | 237 / 611 |
| `batch_100` | 100 | 1.05 s | 94.9 files/s | 38 ms | 55 ms | 66 ms | 72 ms | 1 195 / 3 085 |
| `search_probe`（全量期间） | 12 次 | — | — | 43 ms | 60 ms | — | 60 ms | 成功率 100% |

`verify_hits = 5`：全量结束后用自然词 `handler` 检索，5 条命中——首次全量即可查。

### 5.2 A/B 对照：优化前写路径（concurrency=1 + insert-batch=1）

| 指标 | 优化前 | 优化后 | 倍数 |
|---|---|---|---|
| 500 文件全量 | 95.82 s（5.2 files/s） | 6.20 s（80.7 files/s） | **15.5×** |
| 100 文件批量 | 24.92 s（4.0 files/s） | 1.05 s（94.9 files/s） | **23.6×** |
| 单文件 P95 | 364 ms | 60 ms | 6.1× |
| 全量期间检索 | 191/191 成功 | 12/12 成功 | 均 100% |

基线里的 `insert-batch=1` 就是等价的「每 chunk 一条 INSERT」——和优化前的代码路径一致，所以这组对照是
同机、同快照、同 PG 实例下的纯写路径差异。

### 5.3 1000 文件首次全量（临时夹具 `.bench/repo-1000`，不入库）

`full_index` 1000 文件 **17.41 s**（57.4 files/s），P50 56 ms / P95 108 ms / P99 176 ms，
产出 11 997 chunks / 30 991 edges；期间 34 次探针 **34/34 成功**；收尾检索命中 5 条。

1000 文件吞吐比 500 文件低（57.4 vs 80.7 files/s）的原因：`code_chunks` 的 GIN/HNSW 索引随行数增长，
每条 INSERT 的索引维护成本上升，同时探针在更大的表上做 `ts_rank_cd` 排序——这正是压测要暴露的规模效应。

## 6. 已知边界（不是回归，但要知道）

1. **全量耗时与检索压力强相关**：同一份 500 文件夹具，探针查询命中 0 行时全量 4.2 s，命中约 3 000 行时
   6.2~8.8 s。全量与检索共享 CPU 与连接池，这是设计预期（宁可索引慢一点，也不能让检索失败）；
   若要更激进，调大 `GOMAXPROCS` 或减小 `--probe-interval` 的频次即可，脚本里都是显式参数。
2. **PG 实例是共享的**：`127.0.0.1:55432` 上还有其它分支遗留的 tenant 数据（`code_chunks` 数万行）。
   本报告的三个阶段都在各自独立 tenant/project 下跑，且跑前 `purge` 过；但绝对延迟会受实例整体负载影响，
   严格横比请在独占实例上跑。
3. **CPU 型号字符串不参与硬件判定**：Windows 只能取到 `Intel64 Family 6 Model 170 Stepping 4`，
   与 `hardware.json` 里的人类可读型号必然不一致，故只做提示。硬闸门是「逻辑核数 + OS + GOMAXPROCS」。
4. **embedding 未接真实模型**：CLI 与压测都以 `nil` embedder 运行，向量列为 NULL，检索走 keyword/graph 两路。
   接真实 embedding 后单文件延迟会上升（取决于模型侧 P95），本报告不覆盖这一段。
5. **夹具是合成仓库**：函数体积均匀、边密度固定，比真实仓库「更规整」。真实仓库会因大文件
   （`function_part` 切分）和跨包依赖拉高方差。

## 7. 复现

```bash
export HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable'

# 1) 快照必须逐条匹配（不匹配则拒绝出数）
go run ./bench/repo-500/cmd -mode check -dir bench/repo-500/testdata/repo

# 2) 正式跑（结果写 bench/repo-500/last-run.json）
bash bench/repo-500/run.sh          # Linux/macOS/Git Bash
pwsh bench/repo-500/run.ps1         # Windows PowerShell

# 3) A/B 基线（模拟优化前写路径）
go run ./bench/repo-500/cmd -mode run -dir bench/repo-500/testdata/repo \
  -dsn "$HARNESS_TEST_PG_DSN" -tenant tenant-bench-baseline -project proj-bench-baseline \
  -concurrency 1 -insert-batch 1 -sample 20 -out .bench/baseline.json

# 4) CLI 冒烟（全量 + 探针 + 收尾检索）
GOMAXPROCS=8 go run ./cmd/harness-cli index --path pkg/codeindex --project-id proj-x --full \
  --concurrency 2 --batch-size 8 --probe-query keyword --verify-query search
```

回归：

```bash
go build ./... && go run ./scripts/depcheck .
HARNESS_TEST_PG_DSN=... HARNESS_REQUIRE_PG=1 go test ./pkg/codeindex -run TestSuite -count=1   # 47.7 s 全绿
```

> 套件耗时对共享实例敏感：压测期间库里有 30 000+ 行 bench 数据时同套件要 62 s，清掉 bench tenant 后 47.7 s，
> 空库基线约 23 s。用例本身全绿，差异来自实例上的存量数据，与本线改动无关。压测与套件跑完后已清理
> `tenant-bench-*` / `tenant-cli-smoke` 数据。
