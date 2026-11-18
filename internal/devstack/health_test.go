package devstack

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openLocalPort(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() { _ = ln.Close() }
}

func closedLocalPort(t *testing.T) int {
	t.Helper()
	port, closeFn := openLocalPort(t)
	closeFn()
	return port
}

func testProber() *SystemProber {
	return &SystemProber{
		DialTimeout: 200 * time.Millisecond,
		HTTPTimeout: time.Second,
		ExecTimeout: time.Second,
	}
}

// execHealth returns an item whose health command is a binary that does not exist.
func execHealth(port int) Item {
	return Item{
		Name:   "postgres",
		Kind:   KindContainer,
		Port:   port,
		Health: Health{Type: HealthExec, Command: "tp21-no-such-binary-xyz -h 127.0.0.1 -p " + strconv.Itoa(port)},
	}
}

func TestProbeExecMissingBinaryFallsBackToReachablePort(t *testing.T) {
	port, closeFn := openLocalPort(t)
	defer closeFn()

	healthy, detail := testProber().Healthy(context.Background(), execHealth(port))
	if !healthy {
		t.Fatalf("missing binary with reachable port must be healthy, detail = %q", detail)
	}
	if !strings.Contains(detail, "fallback") {
		t.Fatalf("detail must mention port fallback, got %q", detail)
	}
	if !strings.Contains(detail, strconv.Itoa(port)) {
		t.Fatalf("detail must mention port %d, got %q", port, detail)
	}
}

func TestProbeExecMissingBinaryAndClosedPortFails(t *testing.T) {
	port := closedLocalPort(t)

	healthy, detail := testProber().Healthy(context.Background(), execHealth(port))
	if healthy {
		t.Fatalf("missing binary with closed port must not be healthy, detail = %q", detail)
	}
	if !strings.Contains(detail, "unavailable") {
		t.Fatalf("detail must report the missing command, got %q", detail)
	}
}

// An existing command that exits non-zero must stay unhealthy even when the
// port is reachable; otherwise an unready database would be reported healthy.
func TestProbeExecPresentCommandFailureDoesNotFallBack(t *testing.T) {
	port, closeFn := openLocalPort(t)
	defer closeFn()

	command := "sh -c 'exit 3'"
	if runtime.GOOS == "windows" {
		command = "cmd /c exit 3"
	}
	item := Item{
		Name:   "postgres",
		Kind:   KindContainer,
		Port:   port,
		Health: Health{Type: HealthExec, Command: command},
	}
	healthy, detail := testProber().Healthy(context.Background(), item)
	if healthy {
		t.Fatalf("failing command must not be healthy, detail = %q", detail)
	}
	if strings.Contains(strings.ToLower(detail), "fallback") {
		t.Fatalf("execution failure must not degrade to a port probe, detail = %q", detail)
	}
}

func TestCommandBinary(t *testing.T) {
	tests := map[string]string{
		`pg_isready -h 127.0.0.1 -p 55432`: "pg_isready",
		`"pg_isready" -h 127.0.0.1`:        "pg_isready",
		``:                                 "",
		`   `:                              "",
	}
	for command, want := range tests {
		if got := commandBinary(command); got != want {
			t.Fatalf("commandBinary(%q) = %q, want %q", command, got, want)
		}
	}
}
