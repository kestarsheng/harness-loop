package approval

// 并发读取/决策回归：Await 挂起 + 外部 approve/reject 并发执行，同时并发
// 调用 GetRequest/GetPendingRequests/GetHistory。修复前这些入口返回共享指针，
// -race 会报 applyDecision 写 Status 与调用方锁外读 Status 的数据竞争。

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestApprovalManagerConcurrentDecisions(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 60; round++ {
		m := NewApprovalFlowManager()
		created, err := m.CreateRequest(ctx, &ApprovalRequest{
			Type:           ApprovalTypeToolCall,
			Priority:       PriorityHigh,
			AgentID:        "agent-race",
			SessionID:      fmt.Sprintf("session-%d", round),
			UserID:         "reviewer",
			Description:    "concurrency probe",
			TimeoutSeconds: 30,
		})
		if err != nil {
			t.Fatalf("round %d create: %v", round, err)
		}

		var readers sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < 3; i++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if got, err := m.GetRequest(created.ID); err == nil {
						_ = got.Status
						_ = got.ApprovedBy
					}
					for _, pending := range m.GetPendingRequests("reviewer") {
						_ = pending.Status
					}
					for _, hist := range m.GetHistory("agent-race") {
						_ = hist.Status
					}
					runtime.Gosched()
				}
			}()
		}

		done := make(chan *ApprovalDecision, 1)
		go func() {
			d, _ := m.WaitForApproval(ctx, created.ID)
			done <- d
		}()

		// 等待 WaitForApproval 进入挂起轮询。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if got, err := m.GetRequest(created.ID); err == nil && got.Status == StatusPending {
				break
			}
			time.Sleep(time.Millisecond)
		}

		decision := StatusApproved
		if round%2 == 1 {
			decision = StatusRejected
		}
		if err := m.SubmitDecision(ctx, &ApprovalDecision{
			RequestID: created.ID, Decision: decision, UserID: "reviewer", Reason: "probe",
		}); err != nil {
			t.Fatalf("round %d submit: %v", round, err)
		}
		select {
		case d := <-done:
			if d == nil || d.Decision != decision {
				t.Fatalf("round %d decision = %+v, want %s", round, d, decision)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d wait timeout", round)
		}
		close(stop)
		readers.Wait()
	}
}

// 返回副本不得让调用方改到管理器内部状态。
func TestApprovalManagerGetRequestReturnsCopy(t *testing.T) {
	ctx := context.Background()
	m := NewApprovalFlowManager()
	created, err := m.CreateRequest(ctx, &ApprovalRequest{
		Type: ApprovalTypeToolCall, AgentID: "agent-copy", UserID: "reviewer",
		Details:  map[string]interface{}{"k": "v"},
		Metadata: map[string]interface{}{"m": "n"},
		Comments: []ApprovalComment{{ID: "c1", Content: "first"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := m.GetRequest(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Status = StatusApproved
	got.Details["k"] = "mutated"
	got.Metadata["m"] = "mutated"
	got.Comments[0].Content = "mutated"

	again, err := m.GetRequest(created.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Status != StatusPending {
		t.Fatalf("caller mutation changed stored status to %s", again.Status)
	}
	if again.Details["k"] != "v" || again.Metadata["m"] != "n" || again.Comments[0].Content != "first" {
		t.Fatalf("caller mutation leaked into stored request: %+v", again)
	}
	if created == again {
		t.Fatal("GetRequest must return a copy, not the stored pointer")
	}
}