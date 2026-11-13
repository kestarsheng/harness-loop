package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"

	"agent-platform/pkg/config"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/services/gateway/internal/middleware"
)

type fakeHarnessLoopServer struct {
	harnesspb.UnimplementedHarnessLoopServiceServer

	mu         sync.Mutex
	events     map[string][]*harnesspb.HarnessEvent
	watchExtra map[string][]*harnesspb.HarnessEvent
	watchEnded chan string
}

func (s *fakeHarnessLoopServer) ListEvents(_ context.Context, req *harnesspb.ListEventsRequest) (*harnesspb.ListEventsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &harnesspb.ListEventsResponse{NextSinceSeq: req.GetSinceSeq()}
	for _, e := range s.events[req.GetTenantId()] {
		if e.Seq <= req.GetSinceSeq() {
			continue
		}
		resp.Events = append(resp.Events, e)
		resp.NextSinceSeq = e.Seq
		if req.GetLimit() > 0 && len(resp.Events) >= int(req.GetLimit()) {
			break
		}
	}
	return resp, nil
}

func (s *fakeHarnessLoopServer) WatchEvents(req *harnesspb.WatchEventsRequest, stream harnesspb.HarnessLoopService_WatchEventsServer) error {
	if s.watchEnded != nil {
		defer func() { s.watchEnded <- req.GetTenantId() }()
	}
	s.mu.Lock()
	var pending []*harnesspb.HarnessEvent
	for _, e := range append(append([]*harnesspb.HarnessEvent{}, s.events[req.GetTenantId()]...), s.watchExtra[req.GetTenantId()]...) {
		if e.Seq > req.GetSinceSeq() {
			pending = append(pending, e)
		}
	}
	s.mu.Unlock()

	for _, e := range pending {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

func mkHarnessEvent(tenantID string, seq int64, stage, eventType string) *harnesspb.HarnessEvent {
	return &harnesspb.HarnessEvent{
		EventId:       fmt.Sprintf("ev-%s-%d", tenantID, seq),
		EventType:     eventType,
		SchemaVersion: "1.0.0",
		TaskId:        "task-" + tenantID,
		Seq:           seq,
		TimestampMs:   1700000000000 + seq,
		Severity:      "info",
		TenantId:      tenantID,
		ProjectId:     "proj-a",
		Stage:         &stage,
		Payload:       []byte(`{"ok":true}`),
	}
}

func startMonitorTestRouter(t *testing.T, events map[string][]*harnesspb.HarnessEvent) *httptest.Server {
	t.Helper()
	ts, _ := startMonitorTestRouterFull(t, events, nil)
	return ts
}

func startMonitorTestRouterFull(t *testing.T, events, watchExtra map[string][]*harnesspb.HarnessEvent) (*httptest.Server, *fakeHarnessLoopServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	fake := &fakeHarnessLoopServer{events: events, watchExtra: watchExtra, watchEnded: make(chan string, 8)}
	harnesspb.RegisterHarnessLoopServiceServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	cfg := &config.Config{}
	cfg.Services.HarnessLoop = lis.Addr().String()
	h := NewHarnessLoopHandler(cfg)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api/v2")
	api.Use(middleware.Tenant())
	api.GET("/harness-loop/monitor/stream", h.MonitorStream)

	ts := httptest.NewServer(engine)
	t.Cleanup(func() {
		ts.Close()
		h.Close()
	})
	return ts, fake
}

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

func readSSEUntil(t *testing.T, body io.Reader, wantEvent string) []sseFrame {
	t.Helper()
	scanner := bufio.NewScanner(body)
	var (
		frames []sseFrame
		cur    sseFrame
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if cur.Event != "" || cur.Data != "" {
				frames = append(frames, cur)
				if cur.Event == wantEvent {
					return frames
				}
			}
			cur = sseFrame{}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read sse: %v", err)
	}
	t.Fatalf("stream ended before event %q, frames=%+v", wantEvent, frames)
	return nil
}

func monitorGet(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

func TestMonitorStreamReplaysLastSeqWithoutGapsOrDuplicates(t *testing.T) {
	events := map[string][]*harnesspb.HarnessEvent{
		"tenant-a": {
			mkHarnessEvent("tenant-a", 5, "a", "stage.entered"),
			mkHarnessEvent("tenant-a", 6, "b", "stage.entered"),
			mkHarnessEvent("tenant-a", 7, "c", "acceptance.passed"),
		},
		"tenant-b": {
			mkHarnessEvent("tenant-b", 1, "a", "task.started"),
			mkHarnessEvent("tenant-b", 2, "a", "tool.result"),
		},
	}
	ts := startMonitorTestRouter(t, events)

	resp := monitorGet(t, ts.URL+"/api/v2/harness-loop/monitor/stream?tenant=tenant-a&last_seq=4")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	frames := readSSEUntil(t, resp.Body, "synced")
	var seqs []int64
	for _, f := range frames {
		if f.Event != "harness" {
			continue
		}
		var data struct {
			Seq         int64           `json:"seq"`
			TaskID      string          `json:"task_id"`
			EventType   string          `json:"event_type"`
			Stage       string          `json:"stage"`
			TimestampMs int64           `json:"timestamp_ms"`
			Payload     json.RawMessage `json:"payload"`
			TenantID    string          `json:"tenant_id"`
			EventID     string          `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
			t.Fatalf("decode sse data %q: %v", f.Data, err)
		}
		if data.TaskID == "" || data.EventType == "" || data.Stage == "" || data.TimestampMs == 0 || len(data.Payload) == 0 {
			t.Fatalf("data missing required fields: %+v", data)
		}
		if data.TenantID != "tenant-a" || f.ID == "" || f.ID != data.EventID {
			t.Fatalf("tenant/id mismatch: frame=%+v data=%+v", f, data)
		}
		seqs = append(seqs, data.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 5 || seqs[1] != 6 || seqs[2] != 7 {
		t.Fatalf("replayed seqs = %v, want [5 6 7]", seqs)
	}

	// A second subscriber scoped to tenant-b must only ever see tenant-b data.
	respB := monitorGet(t, ts.URL+"/api/v2/harness-loop/monitor/stream?tenant=tenant-b&last_seq=0")
	defer respB.Body.Close()
	framesB := readSSEUntil(t, respB.Body, "synced")
	var seqsB []int64
	for _, f := range framesB {
		if f.Event != "harness" {
			continue
		}
		var data struct {
			Seq      int64  `json:"seq"`
			TenantID string `json:"tenant_id"`
		}
		if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
			t.Fatalf("decode tenant-b data: %v", err)
		}
		if data.TenantID != "tenant-b" {
			t.Fatalf("tenant-b stream leaked tenant %q", data.TenantID)
		}
		seqsB = append(seqsB, data.Seq)
	}
	if len(seqsB) != 2 || seqsB[0] != 1 || seqsB[1] != 2 {
		t.Fatalf("tenant-b seqs = %v, want [1 2]", seqsB)
	}
}

func TestMonitorStreamRequiresTenantFromQuery(t *testing.T) {
	ts := startMonitorTestRouter(t, map[string][]*harnesspb.HarnessEvent{})

	cases := []string{
		"/api/v2/harness-loop/monitor/stream",
		"/api/v2/harness-loop/monitor/stream?tenant=tenant-a&last_seq=abc",
	}
	for _, path := range cases {
		resp := monitorGet(t, ts.URL+path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body=%s", path, resp.StatusCode, body)
		}
		var envelope map[string]any
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("%s invalid json: %v", path, err)
		}
		if envelope["code"] != "E_TENANT_REQUIRED" && envelope["code"] != "E_INVALID_ARGUMENT" {
			t.Fatalf("%s code = %v", path, envelope["code"])
		}
	}
}

func TestMonitorStreamSwitchesToLiveAndClosesUpstreamOnDisconnect(t *testing.T) {
	events := map[string][]*harnesspb.HarnessEvent{
		"tenant-a": {
			mkHarnessEvent("tenant-a", 5, "a", "stage.entered"),
			mkHarnessEvent("tenant-a", 6, "b", "stage.entered"),
			mkHarnessEvent("tenant-a", 7, "c", "acceptance.passed"),
		},
	}
	watchExtra := map[string][]*harnesspb.HarnessEvent{
		"tenant-a": {mkHarnessEvent("tenant-a", 8, "d", "acceptance.started")},
	}
	ts, fake := startMonitorTestRouterFull(t, events, watchExtra)

	resp := monitorGet(t, ts.URL+"/api/v2/harness-loop/monitor/stream?tenant=tenant-a&last_seq=7")
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var (
		cur       sseFrame
		synced    bool
		replays   int
		liveSeq   int64
		liveFound bool
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			switch cur.Event {
			case "synced":
				synced = true
			case "harness":
				var data struct {
					Seq int64 `json:"seq"`
				}
				if err := json.Unmarshal([]byte(cur.Data), &data); err != nil {
					t.Fatalf("decode live data: %v", err)
				}
				if data.Seq <= 7 {
					replays++
				} else {
					liveSeq = data.Seq
					liveFound = true
				}
			}
			cur = sseFrame{}
			if liveFound {
				break
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	if !synced {
		t.Fatal("missing synced event")
	}
	if replays != 0 {
		t.Fatalf("catch-up replayed %d events that were already acknowledged by last_seq=7", replays)
	}
	if !liveFound || liveSeq != 8 {
		t.Fatalf("live event = %d (found=%v), want seq 8", liveSeq, liveFound)
	}

	resp.Body.Close()
	select {
	case tenant := <-fake.watchEnded:
		if tenant != "tenant-a" {
			t.Fatalf("watch closed for tenant %q", tenant)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream WatchEvents stream was not closed after the SSE client disconnected")
	}
}
