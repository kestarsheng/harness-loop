# P2 CLI 多终端与端到端联动报告（T-P2-T5）

- 生成时间：2026-11-16
- 分支/worktree：`feature/p2-t3-cli` / `hlp-p2-t3-cli`，基线 `3b69019`
- 用例文件：`internal/cli/tui/multi_terminal_test.go`（新增，仅此一个 Go 文件）
- 复跑：`go test ./internal/cli/tui/ -run TestMultiTerminal -v`

## 一、链路与断言

目标链路：**两个终端 → 同一看板 URL → 同租户两个 agent → 看板列出两条可区分的任务 → 事件实时 + 断线续传**。

| 用例 | 断言 | 级别 |
|---|---|---|
| `TestMultiTerminalSameBoardURL` | 两终端解析出的看板 URL 逐字符相同（含尾斜杠差异归一）、路径为 `/harness-loop/monitor`、不带 `?#`；且该 URL 真的出现在两个终端顶栏 | 真实 `tui.URLFor` + `Model.View` |
| `TestMultiTerminalTasksAndLabels` | 两终端**并发** `CreateTask`（goroutine 同时发）→ 两任务同租户、任务号不同、`terminal_label` 不同；`ListTasks(tenant)` 返回 2 条且各带非空 `session_id`/`terminal_label`；`ListTasks(其他租户)` 返回 0 条 | 进程内 gRPC + 真实 pb，服务端为桩 |
| `TestMultiTerminalStreamAndReconnect` | 两终端同时收到同一实时事件（seq 1）；B 断流期间 A 收到 seq 2；B 重连后 own 序列为 `[1 2]`（无缺口、无重复）；重复投递同一 seq 不产生第二行；末态两终端事件序列一致 | 真实 `Model` 状态机 + `tui.Source` 替身 |

## 二、实测输出

```
$ go test ./internal/cli/tui/ -run TestMultiTerminal -v
=== RUN   TestMultiTerminalSameBoardURL
--- PASS: TestMultiTerminalSameBoardURL (0.00s)
=== RUN   TestMultiTerminalTasksAndLabels
--- PASS: TestMultiTerminalTasksAndLabels (0.01s)
=== RUN   TestMultiTerminalStreamAndReconnect
--- PASS: TestMultiTerminalStreamAndReconnect (0.00s)
ok  	agent-platform/internal/cli/tui	1.267s
```

```
$ go test ./internal/cli/...
ok  	agent-platform/internal/cli/cmd	2.884s
ok  	agent-platform/internal/cli/output	0.749s
ok  	agent-platform/internal/cli/tui	1.021s
```

## 三、TUI 渲染记录（**未在真实终端目视**）

如实声明：本次**没有**在真实终端里手动接管两个 `harness loop` 会话做目视检查（无可靠的双终端交互录制手段），因此以固定 120×40 的 `Model.View()` 渲染快照代替截图。快照是真实渲染函数的输出，但不是「人眼看过的终端」。

终端 A（task-a）与终端 B（task-b）在同一看板地址下渲染：

```
 Monitor http://localhost:5174/harness-loop/monitor   o open   shared board
 Harness Loop · task task-a · running
 Stage      e 复盘沉淀           attempt #1   elapsed 29:38
 Acceptance b:pending  e:pending
 Events
  10:00:01 #1 stage.transition c  b→c
  10:00:02 #2 stage.transition d  c→d
  10:00:03 #3 stage.transition e  d→e
 q quit   a abort   l logs   s subtasks   x models   h health   c artifacts   k knowledge   ? help
```

```
 Monitor http://localhost:5174/harness-loop/monitor   o open   shared board
 Harness Loop · task task-b · running
 Stage      e 复盘沉淀           attempt #1   elapsed 29:38
 Acceptance b:pending  e:pending
 Events
  10:00:01 #1 stage.transition c  b→c
  10:00:02 #2 stage.transition d  c→d
  10:00:03 #3 stage.transition e  d→e
 q quit   a abort   l logs   s subtasks   x models   h health   c artifacts   k knowledge   ? help
```

两点观察：顶栏 URL 完全一致（同一看板），任务号与终态视图各自独立（两个 agent）；`elapsed 29:38` 来自零值 `StageStartedAt`，属测试替身未设时间戳，非产品缺陷。

## 四、边界与遗留（必须与上面的 PASS 一起读）

1. **服务端是桩**：`boardServer` 跑在 `bufconn` 上，只实现 `CreateTask`/`ListTasks`。它验证的是 **pb 字段与网关契约**（`session_id=9/22`、`terminal_label=10/23` 能原样进出一趟），**没有**覆盖真实 `harnessloop-service` + PG 仓储。
2. **CLI 侧缺口（本任务域外）**：`internal/cli/cmd/loop.go` 目前建任务时**没有**填 `SessionId`/`TerminalLabel`，只有 gateway HTTP 层读了 `X-Session-ID`/`X-Terminal-Label`。用例因此显式构造带标签的请求来验证契约；**真实两个终端跑 `loop` 时，看板上两条任务目前拿不到不同标签**。这是 T-P2-11/12 的收尾项，不在本次可改文件域（只允许 `internal/cli/tui/multi_terminal_test.go`）。
3. **事件链路是 `Source` 抽象**：`TestMultiTerminalStreamAndReconnect` 驱动的是 `Model`（真实实现 `grpcSource` 的同一接口），覆盖「实时接收 + 退避 + 补齐 + 重新订阅 + seq 去重」状态机；SSE（`/api/v2/harness-loop/monitor/stream`）那一层由 gateway 既有用例覆盖，本次没有跨服务起真链路。
4. **重连退避未等真实时钟**：用例直接注入 `reconnectMsg` 跳过 `reconnectMin`（1s）退避，避免测试空转；退避本身由 `scheduleReconnect` 的既有逻辑保证。
5. **未做双终端目视**：见 §三。

## 五、结论

- 「两终端 → 同一 URL → 同 tenant → ListTasks 两条不同标签」与「事件实时 + 断线重连无缺口无重复」在用例层全部通过，`go test ./internal/cli/...` 全绿。
- 缺标签的真正根因不在测试而在 CLI 未发送终端身份，已作为遗留项 2 明确到 T-P2-11/12，避免用「测试通过」掩盖链路没接上。
