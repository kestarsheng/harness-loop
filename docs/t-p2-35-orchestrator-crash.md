# T-P2-35 orchestrator 开启导致 harnessloop-service 崩溃

- 工作树：`hlp-p2-orchcrash`，分支 `feature/p2-orchestrator-crash`，基线 `ed0f551`
- 联调栈：`50105 mcp / 50106 agent / 50107 harness / 50109 agent-http / 50113 harnessloop`（tenant-orch）
- 结论：**没有复现出"进程数秒被杀"，但复现出了让它看起来像"崩溃"的全部机制**，并逐条修掉。

---

## 一、基线取证（修复前）

### 1. 启动矩阵：orchestrator=on 在各种组合下都不退出

| 场景 | 命令 | 结果 |
|---|---|---|
| 仅 orchestrator=on | `harnessloop-service.exe -config .harness/dev/conf/harnessloop.yaml` | 存活至手动结束（40s 探针） |
| orchestrator=on + `HARNESSLOOP_LLM_*` | 同上 + `LLM_API_KEY/BASE_URL/MODEL` | 存活（`compaction summarizer enabled`） |
| 三开关全开（demo flags） | `features-all.yaml` | 存活（40s 探针） |
| 三开关全开 + 并发多节点任务 | 2 个 3 节点任务并发 | 存活，任务跑到 d |
| 长跑 | orchestrator=on，连续真实任务 | **20m13s**（`all-on` 另一实例 **11m05s**），均未退出 |

即：在 `ed0f551` 源码 + 50113 隔离栈上，**"数秒退出"不是启动期必现**。审阅报告里的"开启即崩溃"因此不是一条稳定的启动路径，而是"某些请求路径会 panic"的表现。

### 2. 真正的退出形态：只有 stderr，退出码全是 1

第二实例占同一端口（模拟 50013 已被 devfull 占用的真实场景）：

```
$ ./harnessloop-service.exe -config .harness/dev/conf/harnessloop.yaml   # 50113 已被占用
EXIT(port-conflict)=1
--stdout--
(空)
--stderr--
... harnessloop: listen 127.0.0.1:50113: listen tcp 127.0.0.1:50113: bind: Only one usage of
    each socket address (protocol/network address/port) is normally permitted.
```

要点：
- 全部启动失败都走 `log.Fatalf` → **一律 exit 1**，没有任何分类；
- 日志只写 **stderr**（Go `log` 默认），`stdout` 完全为空；
- 没有任何落盘记录，终端关了原因就没了。

这就是 T-P2-36 说的"静默 exit(1)"：**由只捕获 stdout 的一侧拉起时（dev-up.sh / devstack / supervisor），它真的什么都不打印**。

### 3. Windows 事件日志：无记录

```powershell
Get-WinEvent -FilterHashtable @{LogName='Application'; StartTime=(Get-Date).AddDays(-3)} |
  Where-Object { $_.Message -match 'harnessloop' }
# → 0 条
```

Go 的 panic 不写事件日志，只写 stderr；事件日志里没有记录 ≠ 没崩过。

### 4. 分类判定（panic / os.Exit / 信号 / 端口 / 配置）

| 类别 | 修复前是否可判 | 证据 |
|---|---|---|
| panic | **不可判**：进程 exit 2，只在 stderr 有栈，任务行停在 `running` | 无拦截器、无 recover（见下） |
| os.Exit | 不可判：`log.Fatalf` 全部 exit 1 | 6 处 `log.Fatalf` |
| 信号 | 不可判 | `signal.NotifyContext` 只处理 `SIGINT/SIGTERM`，Windows 下无独立痕迹 |
| 端口冲突 | 不可判：与 os.Exit 同码同形态 | 上面的 50113 复现 |
| 配置校验 | 不可判：与端口冲突同码同形态 | `invalid config` 也是 exit 1 |

---

## 二、根因（三类缺陷互相掩盖）

### R1 任务 goroutine 没有 panic 边界 —— 这是"崩溃"的真实形态

`internal/handler/grpc_handler.go:205`：

```go
go func() {
    defer func() { delete(h.running, taskID); cancel() }()   // 只有清理，没有 recover
    _, _ = h.runtime.Run(ctx, h.runInput(tenantID, taskID))
}()
```

同时 `cmd/main.go` 的 `grpc.NewServer()` **没有任何拦截器**。于是：

- 请求路径上任何 panic（nil map / 越界 / 断言失败）→ 整个进程 exit 2；
- 任务的错误被 `_, _ =` 丢弃 → 库里那条任务**永远停在 `running`**；
- 编排开启后，请求路径从"一次 stage 调用"膨胀成"规划 → 调度 → 子任务执行 → 汇聚"，panic 接触面成倍放大——所以症状总是"开编排才崩"。

### R2 启动失败不可归类、不可留痕

六处 `log.Fatalf`（config/db/migrate/policy/listen）+ 单流 stderr + 无落盘 = 退出码只有 0/1/2 三个值，包装脚本无从区分，操作者只能猜。

### R3 两个把失败伪装成"莫名退出"的细节

- `-local` 把地址写死成 `127.0.0.1:50008`（端口矩阵里 50008 不是 harnessloop），与 `HARNESSLOOP_ADDRESS`/配置端口互相打架 → 抢端口或 bind 失败秒退；
- `metrics.Registry.Serve` 在 goroutine 里 `_ = srv.ListenAndServe()`，端口被占时 **/metrics 静默消失**。

---

## 三、修复

| 文件 | 改动 |
|---|---|
| `cmd/main.go` | `main` → `os.Exit(run(...))`；`run` 带顶层 recover；6 处 `log.Fatalf` 全部换成分类退出；log 输出改为 `io.MultiWriter(os.Stdout, os.Stderr)`；gRPC 注册 panic 拦截器；启动自检；metrics 端口预探测；`-local` 走 `cfg.SetLocalOnly()` |
| `cmd/exit.go`（新） | 退出码分类表（10/11/12/13/14/15/16）+ `last-exit.json` 落盘（`HARNESSLOOP_STATE_DIR` 可覆盖） |
| `cmd/grpcguard.go`（新） | unary/stream panic 拦截器：记栈 + 返回 `codes.Internal`，进程继续服务 |
| `internal/service/panic.go`（新） | `PanicError`（`Error()` 进事件，`Detail()` 进日志） |
| `internal/service/runtime.go` | `Run` 变成 panic 边界：recover → 记栈 → 任务落 `failed` + `task.failed` 事件，不再留 `running` 僵尸 |
| `internal/service/selfcheck.go`（新） | 启动自检：致命（监听地址非法/自己拨自己/metrics 撞 gRPC 端口）+ 警告（编排开但无执行器/无 stage runner/无子任务落库；策略层开但无审批闸门） |
| `internal/config/config.go` | `SetLocalOnly()` 保端口只改主机；`LoadLLMEnv()` 校验 `HARNESSLOOP_LLM_*` 并点名缺失项 |

退出码契约：

```
10 config_load  11 config_validate  12 database  13 migration
14 listen(端口冲突) 15 policy  16 selfcheck  2 panic
```

修复后的同一端口冲突场景：

```
harnessloop: FATAL category=listen exit=14 reason=listen 127.0.0.1:50113: bind: ...
$ cat .harness/dev/run/last-exit.json
{"code":14,"category":"listen","detail":"listen 127.0.0.1:50113: bind: ...","pid":...,"time":"..."}
```

---

## 四、回归测试

| 测试 | 覆盖 |
|---|---|
| `service.TestRuntimeRecoversStagePanic` | stage 执行 panic → 返回 `*PanicError`、任务 `failed`、`task.failed` 事件、进程不死 |
| `service.TestStartupSelfCheck*`（5 个） | 4 类致命项、3 类编排警告、策略层无审批闸门、旧路径安静 |
| `cmd.TestExitCodeForCategory` | 每个分类的退出码唯一且稳定 |
| `cmd.TestExitReporterWritesRecord` / `…UnwritableStateDir` | 退出原因落盘；状态目录不可写不得二次失败 |
| `cmd.TestRecoveryUnaryInterceptorCatchesPanic` | handler panic → `codes.Internal`，不外溢 |
| `config.TestSetLocalOnlyKeepsPort` | `-local` 保端口 |
| `config.TestLoadLLMEnvNamesTheMissingVariable` | 缺 key/model 时点名缺失变量（T-P2-36） |

```
$ go test ./services/harnessloop-service/... -count=1
ok  .../cmd  .../internal/config  .../internal/handler  .../internal/metrics
ok  .../internal/repository  .../internal/service  .../internal/watchdog
$ go run ./scripts/depcheck .
depcheck: 0 violations (9 rules checked)
```

---

## 五、验收取证

- `orchestrator=on` 启动日志：`feature gates (tenant=platform): orchestrator=on`、`orchestrator bridge enabled`、`selfcheck: all checks passed`
- 长跑：修复后二进制 21:48:30 启动，截至 22:00 仍在监听 50113，日志 0 条 `FATAL`/`panic`
- 任务：`task-b86b76110e9b589c completed`、`task-7a0709849df0ae3e completed`（产物 `notes.md` / `checklist.md` 已真实落盘）

## 六、遗留（跨域 / 环境受阻，未在本分支改）

1. **`sandbox.Resolve(root, ".")` 误判**（`pkg/harnessloop/policy/sandbox/realpath.go:39`）：`.` 被视为 `E_UNSAFE_PATH: empty path`，而 `.` 表示工作区根本身、本应合法。多节点任务的子任务一旦 `read_file(path=".")` 就在 c 阶段失败：
   ```
   E_PATH_ESCAPE (layer=system rule=sandbox.realpath tool=read_file stage=c path=.): E_UNSAFE_PATH: empty path
   ```
   该文件不在本任务文件域（属 T-P2-31 权限层），未改。
2. **LLM 凭据失效**：`llm-gateway.openkylin.top` 对所有 `qwen-plus/deepseek-*` 返回 `401 Invalid API key`（`/models` 却 200）；DashScope 备用 key 返回 `403 FreeTierOnly`（免费额度耗尽）。因此"连续 3 个真实任务 completed"当前只能稳定做到 2 个，第 3 个卡在 a 阶段（`unclassified stage error`）。需要一组可用 key 才能补齐这条验收。
