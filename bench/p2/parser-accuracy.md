# P2 三语言解析准确率报告（T-P2-T3 · 解析部分）

- 生成时间：2026-11-16
- 分支/worktree：`feature/p2-t3-cli` / `hlp-p2-t3-cli`，基线 `3b69019`
- 被测：`pkg/codeindex/parser`（`Registry.Parse` 的 full→regex→degraded 三级路径）
- 评测程序：`bench/parser`（`bench/parser/main.go` + `accuracy.go`），标注集 `bench/parser/labels.jsonl`
- 断言口径：**符号 / 导入 / 调用边三族 P/R/F1，按语言取三族最小值作为该语言得分，阈值 `-min-f1 0.90`，不达标脚本以退出码 1 结束（回归阈值写在脚本里，不靠人看）**

## 一、标注口径（重要，先读这段再看数字）

样例是**为本次评测新写的三语言夹具**（`bench/parser/samples/**`，共 6 文件 303 行），标注逐行人工核对，规则如下：

1. **symbols**：只收解析器会产出 chunk 的声明——Go 的函数/方法（`go/ast` 只遍历 `FuncDecl`，类型声明不是 chunk），TS/Python 的 `class` / `function` / `method`；方法统一写 `Receiver.Name` / `Class.Name`，与索引器写 `indexed_files.parser_id` 时的符号命名一致。
2. **imports**：模块字符串原样保留——Go 取 import path，TS 取 specifier（含 `node:` 前缀），Python 取模块名（相对导入保留点号，如 `.checkpoint`）。
3. **calls**：只收「调用者 → 被调名末段」的边，与 indexer 写 graph `call` 边的口径一致（`fmt.Sprintf` 记 `Sprintf`，`self.store.append` 记 `append`）。`new StageRunner()` 这类**构造**按语义不计入 call 边，解析器若多算则计入 precision 损失。
4. 标注**未按解析器输出反向填充**：先按源码人工写，再用 `-v` 复核每条 miss，只有「人工漏标」才修标注；「解析器真错」保留为 miss 并在 §四列出。
5. 夹具刻意避开无法判定的写法（闭包归属歧义、装饰器叠加），这些已知盲区在 §四单独声明，不混进分数。

## 二、规模

| 语言 | 文件 | 行数 | 标注符号 | 标注导入 | 标注调用边 |
|---|---|---|---|---|---|
| Go | 2 | 91 | 7 | 5 | 15 |
| TypeScript | 2 | 94 | 14 | 5 | 26 |
| Python | 2 | 118 | 19 | 9 | 26 |

## 三、两条路径的实测结果

### 3.1 full 路径（容器，`cgo + -tags=treesitter`）

```bash
docker run --rm -v "<repo>:/src" -v hlp-gomod:/go -w /src -e CGO_ENABLED=1 \
  -e GOPROXY=https://goproxy.cn,direct golang:1.22-bookworm \
  sh -c "go build -tags=treesitter -o /tmp/bench-parser ./bench/parser && \
         /tmp/bench-parser parser -v -json bench/parser/out/parser-treesitter.json"
```

镜像 `golang:1.22-bookworm`，`go1.22.12 linux/amd64`，镜像自带 gcc，`smacker/go-tree-sitter` 自带 C 源，无需 `libtree-sitter-dev`。

| 语言 | parser | 符号 P/R/F1 | 导入 P/R/F1 | 调用边 P/R/F1 | 语言得分（三族最小） | 判定 |
|---|---|---|---|---|---|---|
| Go | `go_ast` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 1.000/1.000/1.000 | **1.000** | PASS |
| TypeScript | `ts_tree_sitter` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 1.000/1.000/1.000 | **1.000** | PASS |
| Python | `py_tree_sitter` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 0.963/1.000/0.981 | **0.981** | PASS |

脚本末行：`PASS: every language is at or above the 0.90 threshold`；机器可读结果 `bench/parser/out/parser-treesitter.json`。

### 3.2 regex 降级路径（本机，`CGO_ENABLED=0`、无标签）

```bash
go run ./bench/parser parser -v -json bench/parser/out/parser-local.json
```

`go1.22.12 windows/amd64`，`CGO_ENABLED=0`，`treesitter=false`。

| 语言 | parser | 符号 P/R/F1 | 导入 P/R/F1 | 调用边 P/R/F1 | 语言得分 | 判定 |
|---|---|---|---|---|---|---|
| Go | `go_ast` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 1.000/1.000/1.000 | **1.000** | PASS |
| TypeScript | `ts_regex` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 0.962/0.962/0.962 | **0.962** | PASS |
| Python | `py_regex` | 1.000/1.000/1.000 | 1.000/1.000/1.000 | 0.897/1.000/0.945 | **0.945** | PASS |

两条路径的 `parser_id` / `capability` 逐文件可见（`ts_regex`+`regex` ↔ `ts_tree_sitter`+`full`），说明 **同一份标注真的跑了两条不同的实现**，不是同一路径跑两遍。

## 四、差距清单（已知 miss，均为真错，标注未迁就解析器）

| # | 路径 | 现象 | 定性 |
|---|---|---|---|
| 1 | `ts_regex` | 多出 `runPipeline->StageRunner` | `new StageRunner(options)` 被 `([A-Za-z_$][\w$]*)\s*\(` 命中，构造被误记为 call 边；tree-sitter 路径无此问题（`new_expression` 不算 `call_expression`） |
| 2 | `ts_regex` | 漏 `summarize->from` | `Array.from(...)` 的被调名 `from` 撞上 `callKeywords` 里的 `from` 关键字，被过滤 |
| 3 | `py_regex` | 多出 `timed->wrapper/fn/monotonic` | 按行区间收集调用，嵌套 `def wrapper` 的调用被同时记到外层 `timed` 头上（tree-sitter 按语法树归属，无此问题） |
| 4 | `py_tree_sitter` | 多出 `Checkpoint->field` | dataclass 类体里的 `field(default_factory=dict)` 被当成类作用域内的调用；标注不认为类属性默认值构成函数级 call 边 |

未覆盖率 / 已知盲区（本数据集不覆盖，不能据此认为实现正确）：

- TS 方法返回函数类型（`subscribe(...): () => void`）时 `ts_regex` 的 `tsMethodRe` 不匹配，符号会整体漏掉；
- 嵌套闭包内部被调用的归属（regex 归外层，tree-sitter 归内层）在含多层闭包的夹具上会显著拉低 regex 分数；
- 装饰器叠加、TS 泛型箭头函数重载、Python `global`/条件导入等写法未入夹具。

## 五、结论

- **准确率门槛达成**：full 路径三语言 0.981–1.000，regex 降级路径 0.945–1.000，均 ≥ 90%；两条路径都留有机器可读 JSON，可逐文件复现。
- 分数差异全部可解释为「正则启发式 vs 语法树」的实现差异，没有一处分差来自标注放水；§四 4 条 miss 是可修项的输入，不在本任务（仅评测）的改动域内。
- 回归方式：`go run ./bench/parser parser`（无 cgo）/ 容器 `-tags=treesitter`，任一族 F1 低于 `-min-f1` 直接退出码 1，可挂 CI。
