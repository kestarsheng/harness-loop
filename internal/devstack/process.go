package devstack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LaunchSpec is one process launch (or foreground command run).
type LaunchSpec struct {
	// Name is used for diagnostics only.
	Name string
	// Dir is the working directory (repo root for all contract items).
	Dir string
	// Command is a shell command line; the contract stores arguments inline.
	Command string
	// Env is the complete child environment (os.Environ + item env).
	Env []string
	// LogPath is truncated before launch and receives stdout+stderr.
	LogPath string
}

// Runner abstracts process control so tests can run without spawning anything.
type Runner interface {
	// Run executes the command to completion (container start/stop commands).
	Run(ctx context.Context, spec LaunchSpec) error
	// Start launches a long-lived process. It returns the OS PID when known;
	// Windows returns 0 because the shell wrapper PID is not the service PID,
	// so callers must resolve the real PID by listening port.
	Start(ctx context.Context, spec LaunchSpec) (int, error)
	// Kill terminates pid and its children.
	Kill(ctx context.Context, pid int, grace time.Duration) error
	// LookupListenerPID resolves the process listening on port.
	LookupListenerPID(port int) (int, error)
}

// SystemRunner implements Runner with real OS processes.
type SystemRunner struct{}

// NewSystemRunner returns the real runner.
func NewSystemRunner() Runner { return SystemRunner{} }

// Run implements Runner.
func (SystemRunner) Run(ctx context.Context, spec LaunchSpec) error {
	f, err := openLog(spec.LogPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := shellCommandCtx(ctx, spec.Command)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd.Run()
}

// Start implements Runner.
func (SystemRunner) Start(ctx context.Context, spec LaunchSpec) (int, error) {
	f, err := openLog(spec.LogPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cmd := shellCommand(spec.Command)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = f
	cmd.Stderr = f
	startDetached(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, nil
	}
	if runtime.GOOS == "windows" {
		// cmd.exe 的 PID 不是服务进程 PID；调用方按端口反查真实 PID。
		return 0, nil
	}
	return pid, nil
}

// Kill implements Runner.
func (SystemRunner) Kill(ctx context.Context, pid int, grace time.Duration) error {
	if grace <= 0 {
		grace = 10 * time.Second
	}
	return killProcessTree(pid, grace)
}

// LookupListenerPID implements Runner.
func (SystemRunner) LookupListenerPID(port int) (int, error) { return lookupListenerPID(port) }

func openLog(path string) (*os.File, error) {
	if path == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("devstack: create log dir: %w", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

func lookupListenerPID(port int) (int, error) {
	if runtime.GOOS == "windows" {
		return lookupPIDWindows(port)
	}
	return lookupPIDUnix(port)
}

func lookupPIDWindows(port int) (int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0, fmt.Errorf("netstat: %w", err)
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 5 {
			continue
		}
		if !strings.EqualFold(fields[0], "TCP") || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			continue
		}
		return pid, nil
	}
	return 0, fmt.Errorf("no listener on 127.0.0.1:%d", port)
}

func lookupPIDUnix(port int) (int, error) {
	if path, err := exec.LookPath("lsof"); err == nil {
		out, err := exec.Command(path, "-ti", fmt.Sprintf("tcp:%d", port), "-sTCP:LISTEN").Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if pid, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && pid > 0 {
					return pid, nil
				}
			}
		}
	}
	if path, err := exec.LookPath("ss"); err == nil {
		out, err := exec.Command(path, "-ltnp").Output()
		if err == nil {
			suffix := ":" + strconv.Itoa(port)
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, suffix) {
					continue
				}
				idx := strings.Index(line, "pid=")
				if idx < 0 {
					continue
				}
				rest := line[idx+len("pid="):]
				end := strings.IndexAny(rest, ",)")
				if end >= 0 {
					rest = rest[:end]
				}
				if pid, convErr := strconv.Atoi(rest); convErr == nil && pid > 0 {
					return pid, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("no listener on 127.0.0.1:%d", port)
}
