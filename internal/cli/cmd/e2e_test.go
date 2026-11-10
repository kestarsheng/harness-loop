//go:build integration

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	harnesspb "agent-platform/pkg/pb/harnessloop"
)

type stubHarnessServer struct {
	harnesspb.UnimplementedHarnessLoopServiceServer
	getCalls int32
}

func (s *stubHarnessServer) CreateTask(ctx context.Context, req *harnesspb.CreateTaskRequest) (*harnesspb.CreateTaskResponse, error) {
	return &harnesspb.CreateTaskResponse{Task: &harnesspb.TaskView{
		TaskId: "task-cli-1", TenantId: req.GetTenantId(), Status: "running",
		CurrentStage: "a", FsmEnabled: true,
	}}, nil
}

func (s *stubHarnessServer) GetTask(ctx context.Context, req *harnesspb.GetTaskRequest) (*harnesspb.TaskView, error) {
	if req.GetTaskId() == "missing" {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	n := atomic.AddInt32(&s.getCalls, 1)
	view := &harnesspb.TaskView{
		TaskId: req.GetTaskId(), Status: "running", CurrentStage: "c", FsmEnabled: true,
		AttemptNoByStage: map[string]int32{"a": 1, "b": 1, "c": 1},
	}
	if n >= 2 {
		view.Status = "completed"
		view.TerminalReason = "completed"
		view.CurrentStage = "e"
		view.AttemptNoByStage["e"] = 0
	}
	return view, nil
}

func (s *stubHarnessServer) AbortTask(ctx context.Context, req *harnesspb.TaskCommand) (*harnesspb.TaskView, error) {
	if req.GetTaskId() == "missing" {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return &harnesspb.TaskView{TaskId: req.GetTaskId(), Status: "aborted", TerminalReason: "aborted", CurrentStage: "c"}, nil
}

func (s *stubHarnessServer) ListEvents(ctx context.Context, req *harnesspb.ListEventsRequest) (*harnesspb.ListEventsResponse, error) {
	return &harnesspb.ListEventsResponse{
		Events: []*harnesspb.HarnessEvent{{
			EventId: "ev-1", EventType: "stage.entered", SchemaVersion: "1.0.0",
			TaskId: req.GetTaskId(), Seq: 1, TimestampMs: time.Now().UnixMilli(), Severity: "info",
			TenantId: "t1", ProjectId: "p1", Payload: []byte(`{"text":"sk-abcdefgh12345678"}`),
		}},
		NextSinceSeq: 1,
	}, nil
}

func startStubServer(t *testing.T) (string, *stubHarnessServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	stub := &stubHarnessServer{}
	harnesspb.RegisterHarnessLoopServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), stub
}

func runCLI(t *testing.T, addr string, args ...string) (int, string) {
	t.Helper()
	t.Cleanup(resetOpts)

	var buf bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	fullArgs := append([]string{"--server", addr, "--tenant", "t1", "--json"}, args...)
	root.SetArgs(fullArgs)

	err := root.Execute()
	code := 0
	if err != nil {
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.Code
		} else {
			code = 4
		}
	}
	return code, buf.String()
}

func TestE2ECLIRunNoWait(t *testing.T) {
	addr, _ := startStubServer(t)
	code, out := runCLI(t, addr, "run", "--goal", "demo", "--workspace", t.TempDir(), "--no-wait")
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, out)
	}
	if env["task_id"] != "task-cli-1" || env["status"] != "running" {
		t.Fatalf("envelope = %v", env)
	}
}

func TestE2ECLIRunFollowsToCompleted(t *testing.T) {
	addr, stub := startStubServer(t)
	code, out := runCLI(t, addr, "run", "--goal", "demo", "--workspace", t.TempDir(), "--timeout", "10s")
	if code != 0 {
		t.Fatalf("exit = %d, out=%s", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output is not json: %v", err)
	}
	if env["status"] != "completed" || env["terminal_reason"] != "completed" {
		t.Fatalf("envelope = %v", env)
	}
	if atomic.LoadInt32(&stub.getCalls) < 2 {
		t.Fatal("expected polling until terminal state")
	}
}

func TestE2ECLIAbortExitCode(t *testing.T) {
	addr, _ := startStubServer(t)
	code, out := runCLI(t, addr, "abort", "--task", "task-cli-1", "--reason", "e2e")
	if code != 3 {
		t.Fatalf("abort exit = %d, out=%s", code, out)
	}
}

func TestE2ECLIStatusNotFound(t *testing.T) {
	addr, _ := startStubServer(t)
	code, out := runCLI(t, addr, "status", "--task", "missing")
	if code != 2 {
		t.Fatalf("not-found exit = %d, out=%s", code, out)
	}
}

func TestE2ECLILogsRedactsSecrets(t *testing.T) {
	addr, _ := startStubServer(t)
	code, out := runCLI(t, addr, "logs", "--task", "task-cli-1")
	if code != 0 {
		t.Fatalf("logs exit = %d, out=%s", code, out)
	}
	if strings.Contains(out, "sk-abcdefgh12345678") {
		t.Fatalf("secret leaked in logs output: %s", out)
	}
	if !strings.Contains(out, "stage.entered") {
		t.Fatalf("expected event line, got %s", out)
	}
}

func TestE2ECLIConnectionFailure(t *testing.T) {
	code, _ := runCLI(t, "127.0.0.1:1", "status", "--task", "x")
	if code != 5 {
		t.Fatalf("connection failure exit = %d, want 5", code)
	}
}
