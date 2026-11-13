# P1 代码增量索引验收与延迟留档（T-P1-T3）

- 生成时间：2026-11-13
- 被测：`pkg/codeindex`（`CodeWriter` / `Watcher` / `Queue` / `Debouncer` / `Search`）
- 环境：Windows 10 x64，Go 1.22.12，PG `pgvector/pgvector:pg16` 容器 `hlp-pg-test:55432`
- 用例：`pkg/codeindex/incremental_suite_test.go`（新增，16 个用例）+ `incremental_test.go`、`debounce_test.go`（存量）
- 执行：
  ```bash
  HARNESS_TEST_PG_DSN="postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable" \
  HARNESS_REQUIRE_PG=1 \
  go test ./pkg/codeindex/... -count=1 -v
  ```

## 一、「真跑了」还是「Skip 了」

| 场景 | 行为 |
|---|---|
| 未设置 `HARNESS_TEST_PG_DSN`（默认） | `t.Skip("... -> SKIPPED")`，`-v` 输出可见 `SKIPPED`，包仍为 PASS |
| 未设置 DSN 且 `HARNESS_REQUIRE_PG=1` | `t.Fatalf`，**测试失败**，杜绝验收环境里「以为跑了其实 Skip」 |
| 设置 DSN 且可连通 | `t.Logf("REAL PG: ... (NOT skipped)")`，用例真实读写 `hlp_code` |

本次验收：`HARNESS_REQUIRE_PG=1` + 真实 DSN，`-v` 输出每条集成用例均带 `REAL PG ... (NOT skipped)`，**无任何 SKIPPED**。
`TestSuiteDSNIsReal` 额外断言 `Ping` 成功且 `hlp_code.code_chunks` 存在，避免「DSN 有值但连的是空库/错库」。

> 结论口径：只有同时满足 (1) 设置了 DSN，(2) 显式开了 `HARNESS_REQUIRE_PG=1`，(3) `-v` 输出无 `SKIPPED`，才可认为增量集成测试「真的跑了」。三者缺一即为 Skip 或未验证。

## 二、7 项验收场景

| # | 验收点 | 用例 | 断言要点 | 结果 |
|---|---|---|---|---|
| 1 | touch 不写事务 | `TestSuiteTouchDoesNotWriteTransaction` | 内容不变 + mtime 前进 → `Skipped=true`、`Chunks/Edges=0`；`indexed_at` 不变、`indexed_files.xmin` 行版本不变、chunk_id 集合不变 | PASS |
| 2 | 改单函数整文件替换 | `TestSuiteSingleFunctionChangeReplacesWholeFile` | 只改 `Beta`，新旧 chunk_id 集合交集为空（header/Alpha 也重写），未改动的 `Alpha` 文本逐字不变 | PASS |
| 3 | 删除清理边 | `TestSuiteDeleteCleansEdges` | 删除后 chunks=0、edges=0（含 `target_file` 反向边）、`indexed_files` 行消失 | PASS |
| 4 | 注入失败回滚 + 旧索引可检索 | `TestSuiteInjectedWriteFailureRollsBackKeepsOldIndex` | 在事务内注入 3 维向量触发 `vector(1024)` 维度错误；返回 `insert chunk` 错误，状态仍 `indexed`，旧 chunk_id 原样保留，`Search("Alpha")` 仍命中该文件 | PASS |
| 5 | 并发 10 次最终唯一 | `TestSuiteConcurrent10IndexFinalUnique` | 10 goroutine 同文件 `force=true`：10 次全成功；最终 3 条 chunk、每个 symbol 仅 1 行、`indexed_files` 仅 1 行 | PASS |
| 6 | 1s 内 5 次保存仅 1 次任务 | `TestSuiteBurstSavesCollapseToOneTask` | 5 次保存（间隔 30ms，总 <1s）→ handler 调用 1 次、`Processed()=1`、最终 3 条 chunk | PASS |
| 7 | 全量后增量仅变更文件参与 | `TestSuiteFullThenIncrementalOnlyChangedFile` | 全量 3 文件后只改 `full_b.go`：`ScanMTime` 仅返回 1 个变更；未变更文件 A/C 的 chunk_id 与 `xmin` 均不变；B 被整文件重写 | PASS |

## 三、补充覆盖（同一套件）

| 用例 | 验证内容 | 结果 |
|---|---|---|
| `TestSuiteSearchModesAndStats` | fulltext / keyword+vector / keyword+vector+graph 三档：`Sources` 保留 provenance、RRF 合并命中、graph 命中可回连到正确文件 | PASS |
| `TestSuiteSearchEdgeCases` | 空查询零路由、TopK `0/-1/101` 回落 10、跨租户零泄漏、embedder 失败降级为 keyword-only | PASS |
| `TestSuiteEmbedBatchChunks` | 71 chunk 触发 2 批 embedding（64 + 7），覆盖 `embedBatch` / `vectorLiteral` | PASS |
| `TestSuiteEmbedFailureDegradesToNullVector` | embedding 后端失败 → 索引成功但 `embedding IS NULL`，向量路由零命中、全文仍可用 | PASS |
| `TestSuiteWatcherStartIndexesRealWrite` | 真实 fsnotify + 80ms 轮询：新文件落盘后被 queue 接管并完成索引 | PASS |
| `TestSuiteWatcherRenameEnqueuesBothPaths` | rename 入队 old+new；`.git` / `node_modules` 路径被忽略 | PASS |
| `TestSuiteDeleteFileOnClosedDB` | 连接池关闭时 `DeleteFile` 返回 `begin delete` 错误而非静默成功 | PASS |

## 四、延迟留档（本机，真实 PG 写入 + 1024 维伪向量）

自动采样仓库自身 Go 文件：排除 `*_test.go`、`.git`/`vendor`/`node_modules`/`frontend`/`testdata` 等目录，≤256KB，按绝对路径排序取前 100（同一提交可复现）。

| 指标 | 实测（多次运行区间） | 阈值 | 结论 |
|---|---|---|---|
| 单文件索引 P95（n=20） | **0.499 s**（0.499–1.242 s） | ≤ 2 s | 通过（最差样本仍有 38% 余量） |
| 100 文件逐文件索引 P95（n=100） | **1.438 s**（1.438–2.064 s） | ≤ 60 s | 通过（余量 ≥96%） |
| 100 文件 P50 | 0.258 s（0.229–0.258 s） | — | — |
| 100 文件批次总耗时 | 42.2 s（42.2–49.8 s） | — | 含 100 次事务提交与 HNSW 插入 |

- 用例：`TestSuiteIndexLatencyBudget`（`-short` 模式跳过）。
- 局限（如实标注）：向量为**词袋哈希伪向量**（1024 维 hashing trick），不是语义 embedding；采样偏仓库真实代码但仅 100 文件，**不能代表万级文件仓库**；P95 受本机磁盘与容器 PG 影响，跨机器需重测。

## 五、覆盖率（工具生成）

```bash
HARNESS_TEST_PG_DSN=... HARNESS_REQUIRE_PG=1 \
  go test ./pkg/codeindex/... -count=1 -coverprofile=coverage_codeindex.out
go tool cover -func=coverage_codeindex.out
```

| 包 | 语句覆盖率 |
|---|---|
| `agent-platform/pkg/codeindex` | **86.1%** |
| `agent-platform/pkg/codeindex/parser` | **86.7%** |
| 合计 | **86.2%** |

未覆盖集中在 `DeleteFile`/`IndexFile` 的极端数据库故障分支（连接中途断开、commit 失败）与 `pollLoop` 的部分 select 分支，属于难以稳定复现的故障注入点；正常路径与关键回滚路径均已覆盖。

## 六、测试输出（节选）

```
--- PASS: TestSuiteDSNIsReal
--- PASS: TestSuiteTouchDoesNotWriteTransaction
--- PASS: TestSuiteSingleFunctionChangeReplacesWholeFile
--- PASS: TestSuiteDeleteCleansEdges
--- PASS: TestSuiteInjectedWriteFailureRollsBackKeepsOldIndex
--- PASS: TestSuiteConcurrent10IndexFinalUnique
--- PASS: TestSuiteBurstSavesCollapseToOneTask
--- PASS: TestSuiteFullThenIncrementalOnlyChangedFile
--- PASS: TestSuiteSearchModesAndStats
--- PASS: TestSuiteSearchEdgeCases
--- PASS: TestSuiteEmbedBatchChunks
--- PASS: TestSuiteEmbedFailureDegradesToNullVector
--- PASS: TestSuiteWatcherRenameEnqueuesBothPaths
--- PASS: TestSuiteWatcherStartIndexesRealWrite
--- PASS: TestSuiteDeleteFileOnClosedDB
    incremental_suite_test.go:1054: INDEX_LATENCY single_p95=498.9404ms (n=20) batch_p95=1.4375367s (n=100) batch_p50=258.2113ms batch_total=42.2345241s
--- PASS: TestSuiteIndexLatencyBudget (42.49s)
ok  agent-platform/pkg/codeindex  55.635s
ok  agent-platform/pkg/codeindex/parser  2.510s
```

最终验收：`--- PASS` 44 个、`--- FAIL` 0 个、`SKIPPED` 0 个。

## 七、结论

7 项增量验收场景全部在真实 PG + pgvector 上通过，且「跑了/跳过」有明确开关与输出证据；单文件与 100 文件延迟双阈值均留档达标；`pkg/codeindex` 覆盖率 86.1%。性能与效果结论仅限本机 100 文件采样与伪向量，已在文中标注，不外推。