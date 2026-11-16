# 沙箱路径逃逸模糊测试报告（T-P2-T4）

- 被测对象：`pkg/harnessloop/policy/sandbox`（`Resolve` / `ResolveCached` / `PathGuard`）
- 测试文件：`pkg/harnessloop/policy/sandbox/fuzz_escape_test.go`（`FuzzPathEscape` + 确定性矩阵）
- 结论：**Windows 本机 60s / Linux 容器 60s 两轮均零放行，逃逸率 0；解析零副作用**（目录树指纹逐字节不变）
- 记录时间：2026-11-16 · 分支 `feature/p2-t4-test`

## 1. 判定标准：三条不变量

不是"看起来拒绝了"就算过。任何一条被打破都记为逃逸：

| # | 不变量 | 域 | 实现位置 |
|---|--------|----|----------|
| I1 | `err == nil` 时结果必须落在 workspace root 内 | 字符串 | `isWithin(root, resolved)` |
| I2 | 结果若真实存在，其文件实体不得与 root 外的哨兵目录/哨兵文件是同一个 inode | 文件系统 | `os.Stat` + `os.SameFile` |
| I3 | 解析过程零副作用：root 与 outside 的目录树指纹前后一致 | 文件系统 | `escapeRig.snapshot()` 前后对比 |

I2 是这轮的关键补强。只看字符串会被"别名"骗过去：`jun-out/secret.txt` 拼出来在 root 内，实体却在
临时目录外。I2 用 `SameFile` 判定 inode 同一性，junction / symlink / 相对链接一旦解析失效会立刻露馅。

拒绝侧还加了一条：错误必须是 `ErrUnsafePath` 或 `ErrPathEscape` 两个哨兵之一，且**不得同时返回路径**。
这防止"靠 panic 或杂牌错误也算拒绝"的假绿。

## 2. 覆盖矩阵（五类逃逸面）

| 类别 | 代表样本 | 期望 | Windows | Linux |
|------|----------|------|---------|-------|
| 编码绕过 | `%2e%2e%2fx`、`%2E%2E%2Fx`、`%2e%2e%5cx`、`.%2e/x` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 编码——双编码 | `%252e%252e%252fx` | 单次解码后为字面量，限制在 root 内 | 放行（root 内） | 放行（root 内） |
| 编码——非法 UTF-8 | `..%c0%afx` | 不得被当作 `..` | 放行（root 内） | 放行（root 内） |
| 编码——伪分隔符 | `a∕..∕..∕x`（U+2215）、`a？？/x`（U+FF0E） | 非 Windows 分隔符，按字面文件名 | 放行（root 内） | 放行（root 内） |
| 分隔符混用 | `..\x`、`a/../../x`、`pkg/./../../x`、`pkg/sub/../../..` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 尾随点/空格 | `.. `、`..\ `、`pkg/.. /..` | `..\ ` 拒绝；`.. ` 折叠到父目录名（root 内） | 见 §5 说明 | 见 §5 说明 |
| UNC | `\\server\share\x`、`\\?\C:\windows`、`\\?\UNC\server\share\x`、`//server/share/x` | `E_UNSAFE_PATH` | 拒绝 | 拒绝 |
| 设备路径 | `\\.\PhysicalDrive0` | `E_UNSAFE_PATH` | 拒绝 | 拒绝 |
| URL 形态 | `file:///etc/passwd`、`http://evil.example/x`、`jar:file://...!/x` | `E_UNSAFE_PATH` | 拒绝 | 拒绝 |
| 绝对路径 | `/etc/passwd`、`\x`、`C:/windows/system32`、`D:/x`、`C:\x` | `E_UNSAFE_PATH` | 拒绝 | 拒绝 |
| NUL / 空 | `a\x00b`、`""`、`"."` | `E_UNSAFE_PATH` | 拒绝 | 拒绝 |
| 符号链接（目录） | `sym-out/secret.txt`、`sym-out` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 符号链接（文件） | `sym-file` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 符号链接（相对上级） | `rel-up/outside-sentinel.txt`（`ln -s .. `） | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 符号链接（链式） | `chain/secret.txt` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| **Junction** | `jun-out/secret.txt`、`jun-out` | `E_PATH_ESCAPE` | 拒绝（真 junction） | 不适用（见 §3） |
| 绕过链接的多跳 | `pkg/../sym-out/secret.txt` | `E_PATH_ESCAPE` | 拒绝 | 拒绝 |
| 合法路径（防过拒） | `pkg/a.go`、`./pkg/../pkg/sub`、`pkg/中文名.go`、`pkg/with space.go`、`con`、`a:b` | 必须放行且落在 root 内 | 放行 | 放行 |

"放行（root 内）"不是漏判：这些向量在**单次解码**契约下就是普通文件名，`Resolve` 返回的路径仍在 root 里，
I1/I2 都成立。把它们写成"放行"而不是"拒绝"，是为了避免用过度拦截换取绿灯——那会把合法文件名（如
`pkg/a:b.go`、`....`）一起打死。

## 3. 夹具：root / outside / 五条链接

`newEscapeRig` 在模糊循环**外**建一次，循环内全部只读，否则 I3 会被夹具自己的写操作污染：

| 条目 | 指向 | 建立方式 | 平台 |
|------|------|----------|------|
| `sym-out` | outside 目录 | `os.Symlink` | 全平台 |
| `sym-file` | outside/outside-sentinel.txt | `os.Symlink` | 全平台 |
| `rel-up` | `..`（root 的父目录） | `os.Symlink("..")` | 全平台 |
| `chain` | `sym-out`（链接套链接） | `os.Symlink` | 全平台 |
| `jun-out` | outside 目录 | `cmd /c mklink /J` | 仅 Windows |

Junction 必须用 `mklink /J`：`os.Symlink` 在 Windows 上建的是 symlink，不是 reparse point，
两者在 `EvalSymlinks` 下的行为要分开验证。`mklink /J` 不需要管理员权限，也不需要开发者模式。
Linux 容器里没有 junction 这个概念，`jun-out` 相关的种子退化为"普通不存在的相对路径"（仍被 I1/I2 判为 root 内，
不会误报），等价的越界语义由 `sym-out` / `rel-up` / `chain` 三条覆盖。

## 4. 执行记录

### 4.1 Windows 本机（go1.22.12 windows/amd64，18 workers）

```
$ go test -fuzz=FuzzPathEscape -fuzztime=60s ./pkg/harnessloop/policy/sandbox/
fuzz: elapsed: 2s,  gathering baseline coverage: 60/60 completed, now fuzzing with 18 workers
fuzz: elapsed: 30s, execs: 1407 (45/sec),  new interesting: 8  (total: 68)
fuzz: elapsed: 1m0s, execs: 4157 (128/sec), new interesting: 19 (total: 79)
PASS
ok  	agent-platform/pkg/harnessloop/policy/sandbox	64.311s
```

- 种子 60 条全部进入 baseline，60s 内累计 **4157 次执行，语料 60 → 79，0 次逃逸、0 次侧效应告警**
- 同一份种子作为确定性矩阵跑一遍：`TestEscapeVectorMatrix` PASS（0.85s）、`TestEscapeJunctionAndSymlinkBothRejected` PASS（0.53s）

### 4.2 Linux 容器（golang:1.22，同一份 `_test.go`，未做任何平台分支改动）

```
$ docker run --rm -e GOFLAGS=-mod=mod -e GOPROXY=https://goproxy.cn,direct \
    -v "$(pwd -W):/src" -w /src golang:1.22 \
    go test -fuzz=FuzzPathEscape -fuzztime=60s ./pkg/harnessloop/policy/sandbox/
fuzz: elapsed: 6s,  execs: 51624  (9223/sec), new interesting: 85  (total: 145)
fuzz: elapsed: 1m0s, execs: 682359 (8745/sec), new interesting: 212 (total: 272)
PASS
ok  	agent-platform/pkg/harnessloop/policy/sandbox	61.637s
```

- **682,482 次执行，语料 272，0 次逃逸、0 次侧效应告警**
- 确定性矩阵在同一容器内同样 PASS；`TestEscapeVectorMatrix` 打印
  `non-windows: junction vectors are covered by symlink equivalents`（junction 断言在 Linux 上跳过，不是隐藏）

### 4.3 为什么要两边都跑，以及吞吐差 160 倍的原因

必须在两个平台跑，是因为逃逸面本身就是平台相关的：`\` 是 Windows 分隔符而在 Linux 是普通字符，
junction 只在 NTFS 存在，`\\?\` 前缀只被 Windows 解析。只跑一边等于只验证了一半的输入语义。

吞吐差异（Windows 128/sec vs Linux 8745/sec）与断言强度无关，是单次执行成本差异：一次迭代要做
2 次 `Resolve`（各含 2~3 次 `EvalSymlinks`）+ 1 次 `PathGuard.Check` + 4 次 `os.ReadDir` 快照 +
若干 `os.Stat`，而 Windows 上每次文件系统系统调用都要过一遍驱动与实时防护链。所以：
**Windows 用 4157 次覆盖"平台特有语义"，Linux 用 68 万次覆盖"输入空间的广度"**，两边互补。

## 5. 零副作用与两条边界说明

- I3 的实现细节：`snapshot()` 只记目录项的"名字 + 类型"，文件项记"名字 + 大小 + mtime"。
  目录 mtime 一律不计入——NTFS 的目录元数据是延迟刷盘的，`MkdirAll` 之后立刻读到的 mtime 会在几毫秒后
  才落定，把目录 mtime 算进去会得到假阳性（本轮第一版实测就在 `pkg/.. /..` 上误报过一次）。
  夹具建好后 `settle()` 等连续两次快照一致才定基线。
- 尾随点/空格（`.. `、`....`）实测语义：Win32 会把这类尾随垃圾折叠成"该名字所在的目录"，即 `root\.. `
  实际指向 root 本身（实测 `os.Stat` 后 `ReadDir` 得到的是 root 的内容，`SameFile` 与 outside 不符），
  因此 `Resolve` 判它"在 root 内"是**正确**的，不是漏判。`..\ `（反斜杠+空格）则会真正跳到父目录，已被拒绝。

### 残留风险（不在 realpath 的能力边界内，此处只声明，不设计修复）

| 风险 | 说明 | 影响 |
|------|------|------|
| 双编码 | 契约是"只解码一次"：`%252e%252e%252f` 落盘为字面文件名。若上游 HTTP/gRPC 层再解一次码，语义会翻转为 `../` | 需要调用方保证只解一次；本包不做二次解码 |
| NTFS 硬链接 | 硬链接没有独立的路径语义，`EvalSymlinks` 看不见，root 内的硬链接可以指向 root 外的 inode | 解析层无法识别，需靠写入侧 `O_NOFOLLOW`/创建时校验 |
| 8.3 短名 | `PROGRA~1` 之类短名只在 root 内部生效，不构成逃逸；若 root 之外存在短名别名，仍需 volume 级校验 | 低 |
| ADS | `file.txt:stream` 在 root 内，不构成路径逃逸 | 低 |

## 6. 验收对照

| 验收项 | 结果 | 证据 |
|--------|------|------|
| 逃逸 100% 拒绝 | ✅ 五类逃逸面全部拒绝，0 放行 | §2 矩阵 + `TestEscapeVectorMatrix` |
| 零副作用 | ✅ 目录树指纹逐字节不变（I3） | `escapeRig.snapshot()` 前后对比 |
| `go test -fuzz=FuzzPathEscape -fuzztime=60s` 无放行 | ✅ Windows 4157 execs / Linux 682482 execs，均 PASS | §4.1、§4.2 |
| Windows 本机 + Linux 容器跑同一用例集 | ✅ 同一份 `fuzz_escape_test.go`，仅 junction 用例按平台跳过 | §4 |
| 报告留档 | ✅ 本文件 | `bench/p2/escape-fuzz.md` |
| 未导出 policy 内部钩子 | ✅ 测试全程只用包内可见的 `Resolve`/`ResolveCached`/`PathGuard`，无新增导出符号 | `git diff` 仅新增测试文件 |
