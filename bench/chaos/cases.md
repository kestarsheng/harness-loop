# T-P2-T2 混沌与故障注入用例集

- 依据：design 9.8（混沌与故障注入）、8.4（P2 验收门槛）、REQ DFX-R-01 ~ DFX-R-08
- 驱动：`bench/chaos/inject.sh`（docker 层注入） + `bench/chaos/probe`（真实客户端探针） + `bench/chaos/chaos_test.go`（应用层判定）
- 证据目录：`bench/chaos/out/`（`results.jsonl` 逐用例结论、`suite-full.log` 完整输出、`pg-tx.log` kill -9 事务日志）

## 0. 为什么要有 probe

混沌演练最常见的假通过是"容器停了，但客户端因为连接池缓存/懒连接返回了零值"，报告上一切正常。
因此本套件把 **注入**（docker）和 **观测**（平台真实客户端）拆开：所有探测都走
`pkg/pgindex`（pgx）、`pkg/mongodb`（mongo-driver）、`pkg/redis`（go-redis），
并且把"预期失败"写死成判定条件——停机期间如果调用**成功**了，用例直接 FAIL。

| 判定原则 | 落地方式 |
|---|---|
| 故障必须被显式识别 | 探测输出 `"result":"degraded"` 且带 driver 原始错误 |
| 不允许静默放行 | 停机期间返回 ok / 空值 → 用例 FAIL |
| 不允许数据损坏 | 停机前后对同一批夹具做 `digest + rows` 比对，必须逐字节一致 |
| 不允许假通过 | 基线摘要拿不到时直接 FAIL（"没测到"不等于"通过"） |

## 1. 用例清单

| # | 用例 | 注入方式 | design 9.8 期望 | 判定依据 |
|---|---|---|---|---|
| 1 | `pg-pause` | `docker pause hlp-chaos-pg` | 记忆降级 md；索引 API 503；事件缓冲重试；任务不中断 | 停机中 ping 报错；恢复后 8 行夹具摘要不变 |
| 2 | `pg-outage` | `docker stop/start` | 同上（真停机，连接被拒） | 连接拒绝错误；恢复后摘要不变 |
| 3 | `mongo-outage` | `docker stop/start` | 检查点保存失败 → 任务继续但标 `checkpoint_degraded` | 停机中 server selection 报错；恢复后 5 文档摘要不变 |
| 4 | `redis-outage` | `docker stop/start` | 队列/缓存降级不阻塞任务 | 停机中 dial 报错；恢复后 5 key 摘要不变 |
| 5 | `kill9-pg` | 事务打开后 `docker kill -s KILL` | 进程 kill -9 → 重启恢复；不重复计费；事件续号 | 未提交事务行必须不存在，摘要与停机前一致 |
| 6 | `disk-full` | `--tmpfs /data:size=1m` 中写 8MB | 写入失败明确报错，不产生半写 md | 必须 `ENOSPC` + `no_corrupt:true`，目录里不得残留半写文件 |
| 7 | `workspace-drift` | 验收后由外部进程改写产物 | drift 事件 + 按策略处理 | 汇聚报告 `artifact_drift` 且 `complete=false`，不许静默通过 |
| 8 | `subtask-hang` | 子任务不返回（时钟推进越过 deadline） | `subtask.failed(category=timeout)`，父按传播策略推进 | 超时事件 + fail-fast 取消同集 + `d→c` 回退 |
| 9 | `quota-full` | 同租户 2 父任务 × 14 子任务打满 20 slot | 新子任务排队 + `subtask.queued`；不失败、不抢占 | running == 20、无 failed、无 cancelled、每父不独占整池 |
| 10 | `models-down` | 回退链内模型全部标记 unavailable | 回退链逐级尝试；全不可用 → 按 timeout/quota 分类，父不静默卡住 | 返回 `FallbackError`、`Exhausted=true`、5s 内返回、尝试次数有界 |
| 11 | `approval-down` | 审批人永远不裁决（服务不可达） | 高危调用保持挂起并告警（**不放行**）；非高危不受影响 | `WaitForApproval` 返回 ctx 超时或 timeout，请求仍是 `pending` |

## 2. 怎么跑

```bash
# 准备：起独立混沌容器（55440/27018/56379，不碰共享开发库）
bench/chaos/inject.sh preflight
bench/chaos/inject.sh up
bench/chaos/inject.sh build      # 预编译探针，单次省 ~30s

# 单用例（便于定位）
bench/chaos/inject.sh case pg-outage
bench/chaos/inject.sh case kill9

# 全量（会重建容器、重建夹具、逐用例记录到 out/results.jsonl）
bench/chaos/inject.sh suite

# 收尾
bench/chaos/inject.sh down
```

应用层用例也可以单独跑（不依赖 docker）：

```bash
HARNESS_CHAOS=1 go test -count=1 -v ./bench/chaos/
```

## 3. 已知边界与遗留项

1. **`checkpoint_degraded` 标记未落在本线文件域**。design 9.8 要求 Mongo 停机时任务继续并标
   `checkpoint_degraded`；当前仓库的 checkpoint 存储层（`pkg/harnessloop/checkpoint`）没有该
   标记/事件字段，`pkg/harnessloop/event` 也没有对应事件类型，二者都不在本线独占范围
   （本线独占 `pkg/harnessloop/orchestrator/**`、`bench/chaos/**`、`bench/p2/chaos-report.md`）。
   本次演练验证的是**同等强度的底线**：Mongo 不可达时检查点调用必须显式报错（不是静默成功），
   服务不因等待 Mongo 而挂住。标记位接入点建议：`checkpoint.Store.Save` 返回错误时由 runtime
   发 `checkpoint.degraded`（需事件字典 owner 增加该类型）。
2. **LLM 真实供应商**未在本地接入。`models-down` 走的是真实回退链代码
   （`modelregistry.FallbackChain`）但调用方是注入的失败 caller，等价于"供应商整体 5xx/拒连"。
3. **磁盘满**在 1MB tmpfs 中复现（`ENOSPC`），未在宿主机真盘上打满；判定条件与真盘一致
   （写失败必须显式返回且不留半写文件）。
4. **PG/Mongo/Redis 使用独立混沌容器**，与开发共享库（`hlp-pg-test` 等）隔离；因此
   "记忆降级 md""索引 API 503"这类**服务层**行为没有在本套件里端到端复现，只验证了
   存储层故障的显式暴露与恢复一致性。
