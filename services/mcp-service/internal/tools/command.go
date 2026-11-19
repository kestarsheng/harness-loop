package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-platform/pkg/harnessloop/policy"
	"agent-platform/pkg/harnessloop/policy/sandbox"
	"agent-platform/services/mcp-service/internal/audit"
)

// CommandTool 是 code_execute 的真实执行器（此前是禁用状态的 stub）。
//
// 它不是「任意 shell」：只在策略层已经放行（白名单命令，或人工批准后的
// 一次性令牌）之后才会被调用，而且永远：
//   - 以调用方给的 workspace_root 为 cwd（没有 root 就拒绝，不退回进程 cwd）；
//   - 不经过 shell，argv 直接 exec；
//   - 拒绝解释器、绝对路径、上跳与元字符；
//   - rm/del 在进程内完成（路径仍走 sandbox.Resolve），既不依赖平台有 rm，
//     也不会把 `-rf` 之类交给外部程序去解释。
type CommandTool struct{}

const (
	commandDefaultTimeout = 60 * time.Second
	commandMaxTimeout     = 300 * time.Second
	commandMaxOutput      = 64 * 1024
)

func NewCommandTool() *CommandTool { return &CommandTool{} }

func (t *CommandTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	return t.ExecuteWithConfig(ctx, args, nil)
}

func (t *CommandTool) ExecuteWithConfig(ctx context.Context, args map[string]interface{}, config map[string]interface{}) (string, error) {
	root := WorkspaceRootFromConfig(config)
	if strings.TrimSpace(root) == "" {
		audit.Deny("code_execute", policy.CodePathEscape, "", "", "workspace root is not configured for this call")
		return "", fmt.Errorf("%w: workspace root is not configured for this call", sandbox.ErrUnsafePath)
	}

	argv := commandArgv(args)
	if len(argv) == 0 {
		return "", errors.New("command is required")
	}
	if err := policy.CheckApprovedCommand(argv); err != nil {
		audit.Deny("code_execute", policy.CodeExecDenied, "", root, err.Error())
		return "", err
	}

	timeout := commandDefaultTimeout
	if raw, ok := args["timeout_seconds"].(float64); ok && raw > 0 {
		timeout = time.Duration(raw) * time.Second
		if timeout > commandMaxTimeout {
			timeout = commandMaxTimeout
		}
	}

	switch strings.ToLower(argv[0]) {
	case "rm", "del":
		return removeWithinWorkspace(root, argv)
	}
	return runCommand(ctx, root, argv, timeout)
}

// commandArgv 把 command/cmd/argv 还原成 argv。字符串按 MCP 的同一套规则
// （strings.Fields）拆分，与策略层算 digest 的口径一致。
func commandArgv(args map[string]interface{}) []string {
	for _, key := range []string{"command", "cmd", "argv"} {
		value, ok := args[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if fields := strings.Fields(typed); len(fields) > 0 {
				return fields
			}
		case []interface{}:
			out := make([]string, 0, len(typed))
			for _, item := range typed {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case []string:
			return typed
		}
	}
	return nil
}

// removeWithinWorkspace 在进程内实现 rm/del。-r/-rf 才允许删目录；目标必须
// 落在 workspace 内。
func removeWithinWorkspace(root string, argv []string) (string, error) {
	recursive := false
	var targets []string
	for _, arg := range argv[1:] {
		switch {
		case arg == "-r" || arg == "-R" || arg == "--recursive":
			recursive = true
		case strings.HasPrefix(arg, "-"):
			// -f/--force 等删除类旗标对「文件不存在」的语义不改变返回值，
			// 这里统一按幂等处理。
		default:
			targets = append(targets, arg)
		}
	}
	if len(targets) == 0 {
		return "", errors.New("rm requires at least one path")
	}

	removed := make([]string, 0, len(targets))
	for _, target := range targets {
		abs, err := resolveWorkspacePath(root, target)
		if err != nil {
			audit.Deny("code_execute", policy.CodePathEscape, target, root, err.Error())
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				removed = append(removed, target+" (not present)")
				continue
			}
			return "", fmt.Errorf("stat %s: %w", target, err)
		}
		if info.IsDir() {
			if !recursive {
				return "", fmt.Errorf("rm: %s is a directory (use -r)", target)
			}
			if err := os.RemoveAll(abs); err != nil {
				return "", fmt.Errorf("rm -r %s: %w", target, err)
			}
		} else if err := os.Remove(abs); err != nil {
			return "", fmt.Errorf("rm %s: %w", target, err)
		}
		removed = append(removed, target)
	}
	return "removed: " + strings.Join(removed, ", "), nil
}

func runCommand(ctx context.Context, root string, argv []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = commandEnv()
	out, err := cmd.CombinedOutput()
	output := string(out)
	if len(output) > commandMaxOutput {
		output = output[:commandMaxOutput] + "\n[output truncated]"
	}
	payload := map[string]any{
		"command": argv,
		"cwd":     root,
		"output":  output,
	}
	if cmd.ProcessState != nil {
		payload["exit_code"] = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	raw, _ := json.Marshal(payload)
	if err != nil {
		audit.Deny("code_execute", policy.CodeExecDenied, filepath.Join(root, argv[0]), root, err.Error())
		return string(raw), fmt.Errorf("command failed: %w", err)
	}
	return string(raw), nil
}

// commandEnv 只继承运行命令所必需的 PATH 与临时目录，避免把审批服务里的
// 密钥（HARNESS_APPROVAL_TOKEN_SECRET 等）泄漏给子进程。
func commandEnv() []string {
	env := make([]string, 0, 4)
	for _, key := range []string{"PATH", "Path", "TMP", "TEMP", "TMPDIR", "HOME", "USERPROFILE", "SYSTEMROOT", "WINDIR", "COMSPEC"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
