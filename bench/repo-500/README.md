# repo-500：P2 索引线全量压测夹具与脚本

这个目录回答一个问题：**全量索引 500 / 1000 个文件时，索引本身多快、检索有没有被堵住。**
为了让不同时间、不同人跑出来的数字可比，这里把两件事钉死——仓库快照与硬件。

## 目录

| 路径 | 作用 |
|---|---|
| `cmd/main.go` | 夹具生成 / 快照校验 / 压测三合一（`-mode gen｜check｜run`） |
| `testdata/repo/` | 500 个文件的仓库快照（放在 `testdata` 下，`go build ./...` 会忽略它） |
| `testdata/repo/snapshot.json` | 快照指纹：每文件 sha256 + 整体 digest + 生成器版本 |
| `testdata/repo/hardware.json` | 基准硬件与运行时参数（GOMAXPROCS / 并发度 / 批量） |
| `run.sh` / `run.ps1` | 压测入口：先校验快照，再按固定参数出数 |
| `last-run.json` | 最近一次压测的完整结果（脚本运行时覆盖写出） |
| `../p2/index-perf.md` | 交付报告：数字、A/B 对照、结论 |

## 两道闸门

1. **仓库快照固定。** 文件内容只由 `(index, seed)` 决定，`snapshot.json` 记录每个文件的
   sha256 与整体 digest。`-mode check` 会逐条比对，任一处不一致直接失败、拒绝出数。
   改了夹具就必须重新 `-mode gen` 并更新 `snapshot.json`——不允许「悄悄变快」。
2. **硬件固定。** `hardware.json` 记录逻辑核数、OS、Go 版本、PG 版本、`GOMAXPROCS` 与并发度。
   跑之前把 `GOMAXPROCS` 固定到记录值（脚本里已经 `export GOMAXPROCS=8`）；
   核数 / OS / GOMAXPROCS 任一不符时结果会标 `hardware_match=false`，数字不可直接对比。
   CPU 型号字符串在不同系统上取到的形式不同（Windows 只给 `Family/Model`），只做提示、不参与判定。

## 三个测量阶段

| 阶段 | 怎么测 | 对应验收 |
|---|---|---|
| `full_index` | 500（或 1000）个文件一次性全量，`--full` 语义；期间每 500ms 打一次检索探针 | 首次全量可查 + 全量期间读旧索引不阻塞 |
| `single_file_sample` | 顺序重写 20 个文件，取单文件耗时分位 | 单文件 P95 ≤ 2s / P99 ≤ 5s |
| `batch_100` | 100 个文件一批，取批内单文件耗时分位 | 100 文件批量 P95 ≤ 60s |

`search_probe` 记录全量期间的检索成功率与延迟；成功率必须 100%——这是「批量期间检索不失败」的取证，
不是推断：探针走的就是真实 `CodeWriter.Search`，与业务检索同一条 SQL。

## 复现

```bash
export HARNESS_TEST_PG_DSN='postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable'
bash bench/repo-500/run.sh              # Linux / macOS / Git Bash
pwsh bench/repo-500/run.ps1             # Windows PowerShell
```

对照实验（把并发与多行 INSERT 关掉，模拟优化前的写路径）：

```bash
go run ./bench/repo-500/cmd -mode run -dir bench/repo-500/testdata/repo \
  -dsn "$HARNESS_TEST_PG_DSN" -tenant tenant-bench-baseline -project proj-bench-baseline \
  -concurrency 1 -insert-batch 1 -sample 20 -out .bench/baseline.json
```

1000 文件首次全量（不进仓库的临时夹具）：

```bash
go run ./bench/repo-500/cmd -mode gen -dir .bench/repo-1000 -files 1000
go run ./bench/repo-500/cmd -mode run -dir .bench/repo-1000 -files 1000 -dsn "$HARNESS_TEST_PG_DSN" \
  -tenant tenant-bench-1000 -project proj-bench-1000
```

## 夹具形态

每个文件 6–12 个函数，函数间固定互相调用（稳定产生 call 边），文件按 5 个一组落在同一个包里。
所以 500 个文件大约产出 6k chunks / 15k edges——与真实中小型 Go 仓库量级相当，且完全可复现。
