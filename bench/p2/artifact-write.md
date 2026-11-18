# 子任务产物落盘与契约校验联调报告（T-P2-28）

- 分支：`feature/p2-artifact-write`（基线 `87f25c3`）
- 联调栈：`../hlp-p2-multinode/.harness/dev`，端口隔离 50105（mcp）/50106（agent）/50107（harness）/50113（harnessloop）
- 租户：`tenant-artifact`；workspace：`.harness/dev/ws-artifact`
- 记录时间：2026-11-18

## 1. 结论先说

三件事缺一不可，缺任何一条 `output artifact "cmd/server/healthz.go" is missing` 都会复现：

| # | 断点 | 事实 | 修法 |
|---|------|------|------|
| 1 | 工具不存在 | `file_write` / `file_read` 根本没在 mcp-service 注册；调用落在 `executeRemoteTool`，返回 `Tool not found: file_write` | 注册两个内置工具，带 `fs.write` / `fs.read` 权限标签 |
| 2 | 工具不落盘 | 两个工具的实现是 stub，只回一段「File writing is restricted for security」文本，且 `AllowedDir` 从来没被用来解析路径 | 真正写盘；root 取自每次调用的 `tool_config.__policy_context__.workspace_root` |
| 3 | 策略词表不一致 | harness 预检（stage_matrix）只认 `write_file`，MCP 暴露的是 `file_write`，工具名直传 ⇒ `E_UNKNOWN_TOOL deny-by-default` | agent-service 在**下发的 tool_call 名字**上做归一（`file_write → write_file`），MCP 侧调用名不变 |

第 3 条是这轮联调才暴露的：文件已经写进 workspace 了，阶段还是被判失败，事件里是
`{"code":"E_UNKNOWN_TOOL","tool":"file_write","rule_id":"registry.unknown"}`。

## 2. 目标根 = 任务 workspace，不是进程 cwd

`file_write` 的 root 只有两个来源，按优先级：

1. `CallToolRequest.tool_config.__policy_context__.workspace_root`（agent-service 从 `__workspace_root__` 变量搬运，harnessloop 与 `collectSubtaskArtifacts` 用的是同一个值）；
2. `MCP_WORKSPACE_ROOT`（仅给非 harness 调用方兜底）。

两者都没有 ⇒ **fail closed**，直接报 `E_UNSAFE_PATH: workspace root is not configured for this call`，绝不退化到进程 cwd 或暂存目录。单测 `TestFileWriteToolTargetsWorkspaceNotProcessCwd` 同时断言「root 下有」和「cwd 下没有」。

## 3. 写盘与断言共用同一套解析

`tools.resolveWorkspacePath` 直接调 `policy/sandbox.Resolve`，因此：

- 相对路径 → `root + rel`，与 `output_contract` 校验时的 `filepath.Join(root, rel)` 同解；
- `../x`、绝对路径、UNC、`http://` 一律拒绝（`E_PATH_ESCAPE` / `E_UNSAFE_PATH`）；
- 先落盘、后断言：临时文件 + rename，然后从磁盘回读比对长度并算 `sha256`，工具才返回 `status=written`。

`pkg/harnessloop/policy/sandbox/workspace_parity_test.go` 把「`Resolve` 与契约拼接同解」钉成回归用例。

## 4. 验收输出

### 4.1 真实任务：`file_write cmd/server/healthz.go` 后 workspace 存在该文件

```
$ harness-cli run --server 127.0.0.1:50113 --tenant tenant-artifact \
    --goal-file .harness/dev/goal-artifact.txt \
    --workspace .../.harness/dev/ws-artifact --no-tui --json
{
  "ok": true,
  "task_id": "task-27213d98730f705a",
  "status": "completed",
  "terminal_reason": "completed",
  "current_stage": "e",
  "stages": [
    { "stage": "a", "attempts": 1, "accepted": true },
    { "stage": "b", "attempts": 1, "accepted": true },
    { "stage": "c", "attempts": 1, "accepted": true },
    { "stage": "d", "attempts": 1, "accepted": true }
  ]
}
```

```
$ ls -la --time-style=+%H:%M:%S .harness/dev/ws-artifact/cmd/server/healthz.go
-rw-r--r-- 1 王可安然 197121 529 17:15:32 .harness/dev/ws-artifact/cmd/server/healthz.go
```

### 4.2 契约校验读到了产物（不是被跳过）

DB 里子任务的契约、状态与产物引用：

```
output_contract | {"type": "files", "paths": ["cmd/server/healthz.go"]}
status          | accepted
artifact_refs   | [{"hash":"sha256:0cdd5d31d4e5d1cde98937540d5a605c518daaf45bb1fd2b1e64aa6ee8696f90",
                    "path":"cmd/server/healthz.go","size":529}]
```

磁盘上同一文件：

```
$ sha256sum .harness/dev/ws-artifact/cmd/server/healthz.go
0cdd5d31d4e5d1cde98937540d5a605c518daaf45bb1fd2b1e64aa6ee8696f90
```

哈希逐字节相同 ⇒ 校验读到的就是工具写下的那份文件。聚合事件同一轮：

```
aggregation.completed {"total":1,"accepted":1,"failed":0,"complete":true}
```

`output artifact ... is missing` 在整轮 harnessloop 输出中零出现。

### 4.3 越权写 root 外被拒且留痕

走真实 gRPC（`bench/p2/scripts/cmd/mcpprobe`）：

```
$ go run ./bench/p2/scripts/cmd/mcpprobe -root C:/tmp/ws-probe -path "../escape.txt" -content leak
is_error=true
content=E_PATH_ESCAPE: "../escape.txt" escapes workspace root

$ go run ./bench/p2/scripts/cmd/mcpprobe -root C:/tmp/ws-probe -path C:/Windows/Temp/evil.txt
content=E_UNSAFE_PATH: absolute path "C:/Windows/Temp/evil.txt" is not allowed

$ ls /c/tmp/escape.txt
ls: cannot access '/c/tmp/escape.txt': No such file or directory
```

审计留痕（`MCP_TOOL_AUDIT_FILE=.harness/dev/logs/tool-audit.jsonl`，同时打 stderr）：

```
{"ts":"2026-11-18T09:19:34.3980028Z","tool":"file_write","decision":"deny","code":"E_PATH_ESCAPE",
 "path":"../escape.txt","workspace_root":"C:/tmp/ws-probe",
 "reason":"E_PATH_ESCAPE: \"../escape.txt\" escapes workspace root"}
{"ts":"2026-11-18T09:20:09.6970325Z","tool":"file_write","decision":"deny","code":"E_PATH_ESCAPE",
 "path":"C:/Windows/Temp/evil.txt","workspace_root":"C:/tmp/ws-probe",
 "reason":"E_UNSAFE_PATH: absolute path \"C:/Windows/Temp/evil.txt\" is not allowed"}
```

正向同一条通道（证明拒绝不是「一律拒绝」蒙对的）：

```
$ go run ./bench/p2/scripts/cmd/mcpprobe -root C:/tmp/ws-probe -path cmd/server/healthz.go -content "package main"
is_error=false
content={"status":"written","path":"cmd/server/healthz.go","bytes":12,
         "sha256":"sha256:5128...fbe7","workspace_root":"C:/tmp/ws-probe"}
```

### 4.4 单测

```
$ HARNESS_REQUIRE_PG=1 HARNESS_TEST_PG_DSN="postgres://agent:...@127.0.0.1:55432/agent_platform?sslmode=disable" \
  go test ./services/mcp-service/... ./pkg/harnessloop/policy/... -count=1
ok  	agent-platform/services/mcp-service/internal/service	1.462s
ok  	agent-platform/services/mcp-service/internal/tools	2.749s
ok  	agent-platform/pkg/harnessloop/policy	6.354s
ok  	agent-platform/pkg/harnessloop/policy/sandbox	6.870s
```

`policy` 包的 T-P1-T5 套件按设计在缺 PG 时 Fatal（不静默 Skip），所以验收命令必须带
`HARNESS_REQUIRE_PG=1` + `HARNESS_TEST_PG_DSN`。

## 5. 部署与联调注意

- 50105 栈里 `harnessloop-service.exe`（15:21）与 `agent-service.exe`（13:07）都比源码 HEAD 旧，
  表现为「阶段 a 每次 ~500ms 连败、agent 侧零日志」。按源码重建三个二进制后阶段链路才通。
  这类「rebuilt 后正常」的死锁状态不要误判成本次改动引入。
- harness-cli 的 `--server` 默认值随版本变（本次重建后默认 `50013` = devfull 栈）。
  跑 50105 栈务必显式 `--server 127.0.0.1:50113`，否则任务会被 devfull 栈接管。
- 阶段矩阵是真的会拦：a/b 阶段不允许 `write_file`。目标描述必须写清「c 阶段才落盘」，
  否则模型在 a 阶段就动手，会被 `stage_matrix.a.write` 正当拒绝。
