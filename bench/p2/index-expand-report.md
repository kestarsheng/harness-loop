# P2 索引线交付报告（T-P2-01 / T-P2-02 / T-P2-04 / T-P2-06）

- 分支：`feature/p2-index`（worktree `hlp-p2-index`）
- 基线：`a9b6758`
- 文件域：`pkg/codeindex/**`、`services/harnessloop-service/internal/service/{index_service,graph_expand}.go`、`services/knowledge-service/**`、`go.mod/go.sum`；未触碰 `internal/cli/cmd`。

## 1. 交付内容

| 任务 | 新增 | 修改 |
|---|---|---|
| T-P2-01 多语言解析 | `parser/lang.go`、`parser/regex.go`、`parser/treesitter_cgo.go`、`parser/treesitter_stub.go`、`parser/multilang_test.go` | `parser/parser.go`（注册表改为 full→regex→degraded 三级）、`parser/go_ast.go`（补 Capability）、`codeindex/chunk.go`（`ParseResult.Capability`、`ChunkClass`）、`parser/parser_test.go`（不支持语言样例改为 `.rb`） |
| T-P2-02 图扩展召回 | `codeindex/graph.go`、`service/graph_expand.go`、`service/graph_expand_test.go` | `codeindex/search.go`（图路走 GraphExpand、RRF 加权、路名 `fulltext/vector/graph`）、`service/index_service.go`（策略装配 + Capabilities）、`codeindex/incremental_suite_test.go`、`service/index_service_test.go`（路名断言） |
| T-P2-04 HTTP rerank | `knowledge-service/internal/rerank/http_reranker.go`、`rerank/metrics.go`、`rerank/http_reranker_test.go`、`service/search_pipeline_test.go` | `internal/service/knowledge_service.go`（top20→top5、降级标记、来源引用收口）、`cmd/main.go`（rerank 模式选择） |
| T-P2-06 异步索引 | `knowledge-service/internal/indexing/worker.go`、`worker_test.go` | `internal/service/knowledge_service.go`（上传走 pending + Enqueue、Index 实现）、`internal/repository/document_repository.go`（`SavePendingDocument`/`SaveChunks`/nil 客户端防护）、`cmd/main.go`（worker 装配与关闭） |

行为契约：

- 解析能力写入 `indexed_files.parser_id`：`go_ast` / `ts_tree_sitter` / `py_tree_sitter` / `ts_regex` / `py_regex` / `fallback:*`；`IndexService.Capabilities()` 返回 `full|regex` 视图。
- 检索 `sources` 路名对齐 design 4.9.2：`fulltext`（原 `keyword`）/`vector`/`graph`；图路权重默认 0.8、每符号 ≤5、总量 ≤50。
- rerank 失败（超时/5xx/协议不符）→ 返回融合顺序并写 `metadata["rerank"]="degraded"`；成功写 `applied` + `rerank_score`。
- 文档四态 `pending|processing|ready|failed`；只有 `ready` 可检索；`failed` 保留 `failure_reason`，可 `Retry` 回到 `pending`。

## 2. 依赖清单与版本

| 依赖 | 版本 | 说明 |
|---|---|---|
| `github.com/smacker/go-tree-sitter` | `v0.0.0-20240827094217-dd81d9e9be82` | TS/TSX/Python 语法；C 运行时与 grammar 源码随模块 vendored，不依赖系统 `libtree-sitter` |
| `github.com/stretchr/testify` | `1.9.0`（go.sum 由 1.8.4 上抬） | 传递依赖，smacker 的 go.mod 要求 |

构建环境（容器验收）：

- 镜像 `golang:1.22-bookworm`，Go `go1.22.12 linux/amd64`，`gcc (Debian 12.2.0-14) 12.2.0`。
- **未安装** `build-essential` / `pkg-config` / `libtree-sitter-dev`：镜像自带 gcc，smacker 自带 C 源，实测 `-tags=treesitter` 可直接编译。若后续换官方 `tree-sitter/go-tree-sitter` 绑定，则需 `libtree-sitter-dev`。
- 容器访问宿主 PG 需 `--add-host host.docker.internal:host-gateway`；模块代理 `GOPROXY=https://goproxy.cn,direct`（Docker DNS 偶发超时，必要时加 `--dns 223.5.5.5`）。

## 3. 验收结果

| 环境 | 命令 | 结果 |
|---|---|---|
| 主机（`CGO_ENABLED=0`，无标签） | `go test ./pkg/codeindex/... ./services/harnessloop-service/internal/service/ ./services/knowledge-service/...` | 全绿（另有全仓 `go test ./...`，见 §5 环境噪声） |
| 容器 无标签 | 同上，`golang:1.22-bookworm` | `pkg/codeindex` 54.3s、`parser` 0.025s、knowledge 四个包全绿；harnessloop service 仅 `TestModelBridgeHotReloadSwapsSnapshot` 抖动（见 §5） |
| 容器 `-tags=treesitter` | 同上 + `go build ./...` | **全绿**：`pkg/codeindex` 57.8s、`parser` 0.107s、harnessloop `service` 8.4s、knowledge `indexing/repository/rerank/service` 0.068/0.035/0.374/0.477s |
| 构建 | `go build ./...`（主机 + 容器） | `BUILD_OK` |
| 依赖规则 | `go run ./scripts/depcheck .` | `0 violations (9 rules checked)` |

关键用例：

- `-tags=treesitter` 下 `parser` 用例断言 `ParserID=ts_tree_sitter/py_tree_sitter`、`Capability=full`；无标签断言 `ts_regex/py_regex` + `regex`，两套同时通过 → 说明两条路径都真实执行。
- `TestGraphExpand*`：查函数名命中直接调用方并带 `graph` 来源；`sources` 三路齐全；节点上限生效；跨租户/项目为空。
- `TestHTTPRerank*`：重排 + top20→top5、裸数组/TEI 字段兼容、5xx 重试 1 次、4xx 不重试、超时预算生效、降级写 `rerank=degraded`。
- `TestIndexWorker*`：`pending→processing→ready`；失败 3 次后 `failed` 且保留原因；手动重试回到 `ready`；取消丢弃排队任务；`failed` 文档被检索过滤（`filterSearchableResults`）。

## 4. 性能数据

图扩展（`HARNESS_PERF=1 go test ./services/harnessloop-service/internal/service -run TestGraphExpandLatencyP95`，n=20）：

| 版本 | P95 | min | max |
|---|---|---|---|
| 初版（单条 SQL 逐边回表） | 1158 ms | 605 ms | 1210 ms |
| 现版（edges + chunks 两段查询，Go 侧封顶） | **35.7 ms** | 24.2 ms | 36.1 ms |

原因：`code_chunks` 上没有 `(tenant_id, project_id, symbol)` 索引，逐边 `JOIN ... ON c.symbol = e.target_symbol` 会被规划成嵌套循环（实测单种子 114 条边 → 53 ms，20 个种子叠加即超 500 ms）。改为先取一跳邻居符号集合、再用一次 `symbol = ANY($3)` 取 chunk，往返次数固定为 2，规避了逐边回表。目标 ≤500 ms，余量约 14 倍。

rerank（`TestHTTPRerankLatencyP95`，20 候选，桩服务注入 20 ms 延迟，n=20）：P95 **31.6 ms**（目标 ≤2.5 s）；超时用例 60 ms 预算内失败返回，供调用方降级。

worker：默认 `Concurrency=2`、`MaxRetries=3`、退避 `1m/5m/15m`（`Config.Backoff` 单测锁定），队列 256；`Enqueue` 立即写 `pending`，失败重试走 `pending` 回队。

## 5. 已知问题（均为环境/存量，非本线回归）

1. **共享 PG 被跨分支迁移污染**：`hlp-pg-test`（55432）里已有 `012_task_session`（来自 CLI 线），导致 `pkg/pgindex` 的 `TestMigrateIsIdempotent`/`TestSchemaMatchesDesign` 失败（期望 11 条迁移）。在 `develop` worktree 复跑同样失败 → 与本线无关。本线未新增迁移。
2. **P1 索引延迟用例对共享实例敏感**：`TestSuiteIndexLatencyBudget`（单文件 P95 ≤2s）在全包并发跑时出现 2.78 s 失败；单独复跑 51.4 s 通过，`develop` 同用例 89.9 s 通过。DB 内 `code_chunks` 已积累 4578 行、`code_edges` 10727 行（多分支测试共用同一实例）。
3. **两处时序抖动**（各自单独 `-count=3` 均通过，`develop` 同样能复现）：`TestSuiteBurstSavesCollapseToOneTask`（3 s 内未收到 fsnotify 事件）、`TestModelBridgeHotReloadSwapsSnapshot`（3 s 内快照未切换）。
4. 图扩展 P95 断言默认跳过，需 `HARNESS_PERF=1`：并发跑全量测试时同实例慢查询会互相污染（实测把 35 ms 抬到 680 ms+），延迟门槛必须在独占实例上测。

## 6. 复核命令

```bash
# 主机（CGO=0，无标签）
go build ./... && go run ./scripts/depcheck .
HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable' \
  go test ./pkg/codeindex/... ./services/harnessloop-service/internal/service/ ./services/knowledge-service/...

# 图扩展 P95（独占实例）
HARNESS_PERF=1 HARNESS_TEST_PG_DSN=... go test ./services/harnessloop-service/internal/service -run TestGraphExpandLatencyP95 -v

# 容器：treesitter 与无标签各跑一轮
docker run --rm --add-host host.docker.internal:host-gateway \
  -v "$PWD:/src" -v hlp-gomod:/go/pkg/mod -w /src \
  -e CGO_ENABLED=1 -e GOPROXY=https://goproxy.cn,direct \
  -e HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@host.docker.internal:55432/agent_platform?sslmode=disable' \
  golang:1.22-bookworm bash -c 'go build ./... && go test -tags=treesitter ./pkg/codeindex/... ./services/knowledge-service/...'
```
