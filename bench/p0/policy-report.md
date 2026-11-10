# P0 权限安全负向测试报告（T-P0-T4 · policy）

- 生成时间：2026-11-11
- 被测包：`pkg/harnessloop/policy`、`pkg/harnessloop/policy/sandbox`
- 执行命令：`go test ./pkg/harnessloop/policy/... -run TestNegative`
- 结果：**PASS**（容器内 `-race` 亦通过）

## 一、路径逃逸向量（100% 拒绝）

| 向量 | 期望 | 结果 |
|---|---|---|
| `../escape` | E_PATH_ESCAPE | 拒绝 |
| `..\escape`（反斜杠） | E_PATH_ESCAPE | 拒绝 |
| `%2e%2e%2fescape`（URL 编码） | E_PATH_ESCAPE | 拒绝 |
| `/etc/passwd`（绝对路径） | E_PATH_ESCAPE | 拒绝 |
| `\\server\share\file`（UNC） | E_PATH_ESCAPE | 拒绝 |
| `\\?\C:\windows`（扩展 UNC） | E_PATH_ESCAPE | 拒绝 |
| `file:///etc/passwd`（URL scheme） | E_PATH_ESCAPE | 拒绝 |
| `http://evil.example/x` | E_PATH_ESCAPE | 拒绝 |
| 含 NUL 字节 | E_PATH_ESCAPE | 拒绝 |
| 符号链接指向工作区外 | E_PATH_ESCAPE | 拒绝 |
| ToCToU（两次解析） | 结果稳定且在 root 内 | 通过 |

非逃逸形态 `.../a`、`..a/b`、`a/../b`：不产生越界路径，`requested_path` 不回流。

## 二、exec 向量（100% 拒绝）

`&&`、`;`、`|`、`$(...)`、反引号、`$VAR`、`sh -c`、`bash -c`、`cmd /c`、`powershell -c`、`npm install`、绝对路径参数、`../` 参数、`go install`、`go get`、`go mod download/tidy`、带路径的可执行名。

全部返回 `E_EXEC_DENIED`。

## 三、自我提权（新增防护）

| 目标 | 结果 |
|---|---|
| 写 `config/harness_loop/stage_allow.yaml` | **E_SELF_ELEVATION**（rule_id=`policy.self_elevation`） |
| 写 `config/harness_loop/exec_allow.yaml` | 拒绝 |
| 写 `.harness/policy/stage_matrix.yaml` | 拒绝 |
| 写 `features.yaml`（反斜杠形式） | 拒绝 |
| 只读 `config/harness_loop/stage_matrix.yaml` | 允许（仅禁止写） |

## 四、阶段矩阵

| 阶段 | write | run_command（go test） | 说明 |
|---|---|---|---|
| a | 拒绝 | **拒绝**（修复前误放行） | 只读 |
| b | 拒绝 | 拒绝 | 只读 |
| c | 允许 | 允许 | 读写+执行 |
| d | 拒绝 | 允许 | 只读+受限执行 |
| e | 拒绝 | 拒绝 | 全拒 |

修复记录：原实现给 `run_command` 开了“始终允许”的例外，导致 a/b 阶段可执行命令；已改为“阶段矩阵 exec 非空才允许”，并保留命令级白名单二次校验。

## 五、结构化拒绝与副作用

- 每条拒绝均断言 `code` / `layer` / `rule_id` / `tool` / `stage` 非空
- `Guard()` 拒绝时执行函数**未被调用**（无副作用）
- 决策前后请求快照 sha256 一致（`policy_hash` 稳定，不被求值过程修改）

## 六、结论

P0 安全负向门槛：**通过**。路径逃逸与 exec 注入 100% 拒绝，自我提权已封堵，矩阵行为与设计一致。