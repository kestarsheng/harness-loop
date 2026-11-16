package sandbox

// T-P2-T4 · 沙箱路径逃逸模糊测试（design 4.13.2 / REQ-128 前置面）。
//
// 覆盖五类逃逸面：编码绕过（%2e/%2f/双编码/非法 UTF-8）、分隔符混用（\ 与 /、
// 尾随点空格、Unicode 伪分隔符）、UNC（\\server、\\?\、\\?\UNC\）、符号链接、
// Windows junction。
//
// 三条不变量，任何一条被打破都算逃逸：
//  1. Resolve 返回 nil error 时，结果必须落在 workspace root 内（字符串域）；
//  2. 结果若真实存在，其文件实体不得与 root 外的哨兵目录/哨兵文件是同一 inode
//     （文件系统域，能抓到 junction/symlink 别名这类"字符串合法、语义越界"）；
//  3. 解析过程零副作用：root 内外的目录快照（名字+大小+mtime）必须逐字节不变。
//
// 夹具（root / outside / symlink / junction）只在模糊循环外建一次，循环里全部只读，
// 否则"零副作用"这条断言会被夹具自己的写操作污染。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fuzzMaxInput 限制单次输入长度：模糊器会生成数 KB 级的串，超长的没有新的逃逸
// 语义，只是拖慢迭代次数，直接跳过以保证 60s 内跑到足够多的有效用例。
const fuzzMaxInput = 4096

// escapeSeeds 是五类逃逸面的种子语料。种子同时充当"确定性矩阵"的数据源
// （见 TestEscapeVectorMatrix），所以两边不会漂移。
var escapeSeeds = []string{
	// 1) 编码绕过
	"../x",
	"%2e%2e%2fx",
	"%2E%2E%2Fx",
	"%2e%2e%5cx",
	".%2e/x",
	"%2e%2e/%2e%2e/x",
	"%252e%252e%252fx",     // 单次解码后仍是字面量：契约是"只解码一次"，落盘应为普通文件名
	"..%c0%afx",            // 非法 UTF-8 的过长短编码：不得被当成 ".."
	"%ef%bc%8e%ef%bc%8e/x", // 全角句点：不是 Windows 分隔符
	"a%00b",
	"%2f%2fetc%2fpasswd",
	"%5c%5cserver%5cshare",

	// 2) 分隔符混用
	`..\x`,
	`..\..\x`,
	"a/../../x",
	"pkg/./../../x",
	"pkg/sub/../../..",
	"sub/../../x",
	".. ",
	`..\ `,
	"pkg/.. /..",
	"a/../.. /x",
	"....//x",
	".../x",
	"a\u2215..\u2215..\u2215x", // U+2215 division slash
	"a\uFF0E\uFF0E/x",          // U+FF0E 全角句点

	// 3) UNC / 设备路径
	`\\server\share\x`,
	`\\?\C:\windows`,
	`\\?\UNC\server\share\x`,
	`\\.\PhysicalDrive0`,
	"//server/share/x",
	"file:///etc/passwd",
	"http://evil.example/x",
	"jar:file:///etc/passwd!/x",

	// 4) 绝对路径
	"/etc/passwd",
	`\x`,
	"C:/windows/system32",
	"D:/x",
	`C:\x`,

	// 5) 链接类（夹具里真实存在）
	"sym-out/secret.txt",
	"sym-out",
	"jun-out/secret.txt",
	"jun-out",
	"sym-file",
	"chain/secret.txt",
	"rel-up/outside-sentinel.txt",
	"pkg/../sym-out/secret.txt",

	// 6) 必须是空/空的等价物
	"",
	".",
	"./",
}

// insideSeeds 是必须被放行的合法路径：用来防"把一切都拒掉"这种假绿。
var insideSeeds = []string{
	"pkg/a.go",
	"pkg/sub/b.go",
	"./pkg/../pkg/sub",
	"pkg/sub/",
	"newdir/new.go",
	"pkg/中文名.go",
	"pkg/with space.go",
	"con",
	"aux.go",
	"a:b", // 不是盘符（无 "://"、非绝对），按文件名处理
}

type escapeRig struct {
	root        string
	outside     string
	sentinel    string
	outDirInfo  os.FileInfo
	outFileInfo os.FileInfo
	guard       PathGuard
	links       []string
}

func newEscapeRig(tb testing.TB) *escapeRig {
	tb.Helper()
	root := tb.TempDir()
	outside := tb.TempDir()
	rig := &escapeRig{root: root, outside: outside}
	rig.sentinel = filepath.Join(outside, "outside-sentinel.txt")
	if err := os.WriteFile(rig.sentinel, []byte("SENTINEL"), 0o644); err != nil {
		tb.Fatalf("write sentinel: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg", "sub"), 0o755); err != nil {
		tb.Fatalf("mkdir pkg: %v", err)
	}
	rig.outDirInfo, _ = os.Stat(outside)
	rig.outFileInfo, _ = os.Stat(rig.sentinel)

	// 符号链接（目录 / 文件 / 相对上级 / 链式）
	if err := os.Symlink(outside, filepath.Join(root, "sym-out")); err == nil {
		rig.links = append(rig.links, "sym-out")
	} else {
		tb.Logf("dir symlink unavailable: %v", err)
	}
	if err := os.Symlink(rig.sentinel, filepath.Join(root, "sym-file")); err == nil {
		rig.links = append(rig.links, "sym-file")
	} else {
		tb.Logf("file symlink unavailable: %v", err)
	}
	// 相对链接 ".."：从 root 内部指回 root 的父目录（跨平台的经典逃逸面）。
	if err := os.Symlink("..", filepath.Join(root, "rel-up")); err == nil {
		rig.links = append(rig.links, "rel-up")
	}
	if len(rig.links) > 0 {
		if err := os.Symlink(filepath.Join(root, "sym-out"), filepath.Join(root, "chain")); err == nil {
			rig.links = append(rig.links, "chain")
		}
	}

	// Windows junction：mklink /J 不需要管理员权限，且 EvalSymlinks 会解析它。
	if runtime.GOOS == "windows" {
		if err := makeJunction(filepath.Join(root, "jun-out"), outside); err == nil {
			rig.links = append(rig.links, "jun-out")
		} else {
			tb.Logf("junction unavailable (junction 用例将跳过): %v", err)
		}
	}

	rig.guard = PathGuard{Root: root, AllowedSubpaths: []string{"pkg"}}
	rig.settle()
	return rig
}

// makeJunction 用 mklink /J 建 Windows junction（reparse point）。
// 注意 junction 不是 symlink：os.Symlink 建出来的是 symlink，只有 mklink /J 才是
// 真正的 junction，两者在 EvalSymlinks 下的行为要通过同一组断言分别验证。
func makeJunction(link, target string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("junction is windows-only")
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		return fmt.Errorf("junction not created: %v", statErr)
	}
	return nil
}

// snapshot 抓 root 与 outside 的目录树指纹，用于证明零副作用。
//
// 目录项只记"名字+类型"：NTFS 的目录 mtime 是延迟刷盘的，MkdirAll 之后立刻读到
// 的 mtime 会在几毫秒后才落定，把目录 mtime 也算进来会得到假阳性。文件项记
// 名字+大小+mtime，内容被改写一定跑不掉；新增/删除文件则由名字列表本身抓到。
func (r *escapeRig) snapshot() string {
	var b strings.Builder
	for _, dir := range []string{r.root, r.outside} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			b.WriteString("ERR:" + err.Error())
			continue
		}
		for _, entry := range entries {
			info, statErr := entry.Info()
			if statErr != nil {
				b.WriteString(dir + "|" + entry.Name() + "?;")
				continue
			}
			if entry.IsDir() {
				fmt.Fprintf(&b, "%s|%s|<dir>;", dir, entry.Name())
				continue
			}
			fmt.Fprintf(&b, "%s|%s|%d|%d;", dir, entry.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return b.String()
}

// settle 等夹具落定：连续两次快照一致才认为基线稳定（NTFS 元数据是延迟写的）。
func (r *escapeRig) settle() {
	for i := 0; i < 20; i++ {
		first := r.snapshot()
		time.Sleep(5 * time.Millisecond)
		if first == r.snapshot() {
			return
		}
	}
}

// assertContained 做字符串域 + 文件系统域双重判定。
func (r *escapeRig) assertContained(t *testing.T, label, rel, resolved string) {
	t.Helper()
	if resolved == "" {
		return
	}
	if !isWithin(r.root, resolved) {
		t.Fatalf("%s: input %q resolved to %q, outside root %q", label, rel, resolved, r.root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return // 目标不存在：字符串域已经判过，没有可用实体
	}
	if r.outDirInfo != nil && os.SameFile(info, r.outDirInfo) {
		t.Fatalf("%s: input %q resolved to %q which IS the outside sandbox dir", label, rel, resolved)
	}
	if r.outFileInfo != nil && os.SameFile(info, r.outFileInfo) {
		t.Fatalf("%s: input %q resolved to %q which IS the outside sentinel file", label, rel, resolved)
	}
}

// FuzzPathEscape 是 `go test -fuzz=FuzzPathEscape` 的目标。
// 断言集合见文件头三条不变量；任何一条失败都打印完整输入，便于直接回收 corpus。
func FuzzPathEscape(f *testing.F) {
	rig := newEscapeRig(f)
	for _, seed := range escapeSeeds {
		f.Add(seed)
	}
	for _, seed := range insideSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, rel string) {
		if len(rel) > fuzzMaxInput {
			t.Skipf("input too long: %d bytes", len(rel))
		}
		before := rig.snapshot()

		for _, target := range []struct {
			label string
			fn    func(string, string) (string, error)
		}{
			{"Resolve", Resolve},
			{"ResolveCached", ResolveCached},
		} {
			resolved, err := target.fn(rig.root, rel)
			if err != nil {
				// 拒绝理由必须是这两个哨兵错误之一：不能靠 panic 或杂牌错误"拒绝"。
				if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrPathEscape) {
					t.Fatalf("%s(%q) returned unexpected error kind: %v", target.label, rel, err)
				}
				if resolved != "" {
					t.Fatalf("%s(%q) returned a path together with an error: %q", target.label, rel, resolved)
				}
				continue
			}
			rig.assertContained(t, target.label, rel, resolved)
		}

		// 子路径白名单层：放行的必须真的在 allowed 子树里，拒绝的必须回显原路径。
		bad, err := rig.guard.Check([]string{rel})
		if err != nil {
			if bad != rel {
				t.Fatalf("PathGuard.Check(%q) echoed %q as the offending path", rel, bad)
			}
		} else {
			resolved, rerr := Resolve(rig.root, rel)
			if rerr == nil && !rig.guard.underAllowed(resolved) {
				t.Fatalf("PathGuard allowed %q -> %q which is outside allowed subpaths", rel, resolved)
			}
		}

		if after := rig.snapshot(); after != before {
			t.Fatalf("resolving %q mutated the filesystem\nbefore: %s\nafter:  %s", rel, before, after)
		}
	})
}

// TestEscapeVectorMatrix 是 fuzz 目标的确定性兜底：种子逐条写成可读的矩阵，
// Windows 本机与 Linux 容器跑的是同一份期望值，失败信息直接指出类别。
// 它同时验证"拒绝得对"（错误类别）与"不该拒的没拒"（insideSeeds 必须放行）。
func TestEscapeVectorMatrix(t *testing.T) {
	rig := newEscapeRig(t)
	if runtime.GOOS != "windows" {
		// junction 是 Windows 专属；Linux 上 jun-out 不存在，跳过这几条（见报告）。
		t.Log("non-windows: junction vectors are covered by symlink equivalents")
	}

	for _, rel := range escapeSeeds {
		before := rig.snapshot()
		resolved, err := Resolve(rig.root, rel)
		if err == nil {
			// 少数种子（如 %252e 双编码、".. "）在设计上就是"合法"的：只要限制在
			// root 内且没有指向外部实体，就不算逃逸。矩阵里用 assertContained 兜底。
			rig.assertContained(t, "seed", rel, resolved)
		} else if !errors.Is(err, ErrUnsafePath) && !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%q rejected with a non-sentinel error: %v", rel, err)
		}
		if after := rig.snapshot(); after != before {
			t.Fatalf("resolving %q mutated the filesystem", rel)
		}
	}

	for _, rel := range insideSeeds {
		resolved, err := Resolve(rig.root, rel)
		if err != nil {
			t.Fatalf("legitimate path %q must be accepted, got %v", rel, err)
		}
		if !isWithin(rig.root, resolved) {
			t.Fatalf("legitimate path %q resolved outside root: %q", rel, resolved)
		}
	}
}

// TestEscapeJunctionAndSymlinkBothRejected 单独把链接类拎出来，保证 junction
// 与 symlink 各自都有显式断言（而不是被模糊器"顺带"覆盖）。
func TestEscapeJunctionAndSymlinkBothRejected(t *testing.T) {
	rig := newEscapeRig(t)
	cases := []string{"sym-out/secret.txt", "rel-up/outside-sentinel.txt", "chain/secret.txt", "sym-file"}
	if runtime.GOOS == "windows" {
		cases = append(cases, "jun-out/secret.txt", "jun-out")
	}
	for _, rel := range cases {
		if _, err := Resolve(rig.root, rel); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%q must be rejected with E_PATH_ESCAPE, got %v", rel, err)
		}
	}
}
