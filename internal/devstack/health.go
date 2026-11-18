package devstack

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Prober reports whether one contract item is healthy.
//
// Healthy returns (true, detail) when the probe passed; on failure detail
// carries the last observed reason (connection refused, HTTP status, exit code).
type Prober interface {
	Healthy(ctx context.Context, item Item) (bool, string)
}

// SystemProber performs real TCP / HTTP / exec probes.
type SystemProber struct {
	DialTimeout time.Duration
	HTTPTimeout time.Duration
	ExecTimeout time.Duration
	Client      *http.Client
}

// NewSystemProber returns a prober with dev-friendly timeouts.
func NewSystemProber() *SystemProber {
	return &SystemProber{
		DialTimeout: 800 * time.Millisecond,
		HTTPTimeout: 3 * time.Second,
		ExecTimeout: 5 * time.Second,
	}
}

// Healthy implements Prober.
func (p *SystemProber) Healthy(ctx context.Context, item Item) (bool, string) {
	switch item.Health.Type {
	case HealthPort:
		return p.probePort(ctx, item)
	case HealthHTTP:
		return p.probeHTTP(ctx, item)
	case HealthExec:
		return p.probeExec(ctx, item)
	default:
		return false, fmt.Sprintf("unknown health type %q", item.Health.Type)
	}
}

func (p *SystemProber) probePort(ctx context.Context, item Item) (bool, string) {
	addr := item.Health.Address
	if addr == "" {
		addr = itemAddress(item)
	}
	if err := dialTCP(ctx, addr, p.dialTimeout()); err != nil {
		return false, fmt.Sprintf("%s: %v", addr, err)
	}
	return true, addr + " reachable"
}

func (p *SystemProber) probeHTTP(ctx context.Context, item Item) (bool, string) {
	target := item.Health.URL
	if target == "" {
		target = "http://" + itemAddress(item)
	}
	want := item.Health.ExpectStatus
	if want == 0 {
		want = http.StatusOK
	}
	cctx, cancel := context.WithTimeout(ctx, p.httpTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", target, err)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: p.httpTimeout()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return false, fmt.Sprintf("%s: HTTP %d (want %d)", target, resp.StatusCode, want)
	}
	return true, fmt.Sprintf("%s: HTTP %d", target, want)
}

// probeExec runs the declared command. pg_isready is not always installed
// outside docker, so an unavailable binary falls back to a TCP probe on the
// item's port (the same degradation scripts/dev-lib.sh uses).
//
// Availability is decided with exec.LookPath, not by matching error text:
// Windows localizes "not recognized", and cmd /c returns ExitError rather than
// exec.ErrNotFound. Execution failures of a present binary never fall back, so
// an unready database is still reported unhealthy.
func (p *SystemProber) probeExec(ctx context.Context, item Item) (bool, string) {
	binary := commandBinary(item.Health.Command)
	if binary == "" {
		return false, "health command is empty"
	}
	if _, err := exec.LookPath(binary); err != nil {
		addr := itemAddress(item)
		if addr != "" && dialTCP(ctx, addr, p.dialTimeout()) == nil {
			return true, fmt.Sprintf("%s reachable (health command unavailable, port fallback)", addr)
		}
		return false, fmt.Sprintf("health command %q unavailable and %s unreachable", item.Health.Command, addr)
	}

	cctx, cancel := context.WithTimeout(ctx, p.execTimeout())
	defer cancel()
	cmd := shellCommandCtx(cctx, item.Health.Command)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, strings.TrimSpace(item.Health.Command) + ": ok"
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	return false, detail
}

// commandBinary returns the executable named by the first command token.
func commandBinary(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], `"'`)
}

func (p *SystemProber) dialTimeout() time.Duration {
	if p.DialTimeout > 0 {
		return p.DialTimeout
	}
	return 800 * time.Millisecond
}

func (p *SystemProber) httpTimeout() time.Duration {
	if p.HTTPTimeout > 0 {
		return p.HTTPTimeout
	}
	return 3 * time.Second
}

func (p *SystemProber) execTimeout() time.Duration {
	if p.ExecTimeout > 0 {
		return p.ExecTimeout
	}
	return 5 * time.Second
}

func itemAddress(item Item) string {
	if item.Health.Address != "" {
		return item.Health.Address
	}
	if item.Port > 0 {
		return "127.0.0.1:" + strconv.Itoa(item.Port)
	}
	return ""
}

func dialTCP(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func shellCommand(command string) *exec.Cmd {
	return shellCommandCtx(context.Background(), command)
}

func shellCommandCtx(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/c", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}
