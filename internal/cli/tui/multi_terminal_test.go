package tui

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"agent-platform/internal/cli/tui/views"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

// 本文件覆盖 T-P2-T5「两个终端 → 同一 URL → 同一看板两个 agent」的链路：
//
//	TestMultiTerminalSameBoardURL      两个终端解析出的看板 URL 完全一致
//	TestMultiTerminalTasksAndLabels    两终端并发建任务后 ListTasks 返回两条、标签不同
//	TestMultiTerminalStreamAndReconnect 两个终端同时收实时事件，其中一个断线后按 lastSeq 续传且不重不漏
//
// 边界（如实声明，报告里同样写明）：
//   - 任务与标签走 bufconn 上的进程内 gRPC 服务桩，验证的是 pb/服务契约，不是真实 harnessloop-service；
//   - CLI 侧目前尚未把 session_id/terminal_label 放进 CreateTask 请求（T-P2-11/12 落在 gateway+proto），
//     因此这里显式构造请求验证契约，CLI 侧缺口记入报告遗留项；
//   - 事件链路走 tui.Source 抽象（真实实现是 grpcSource），覆盖的是 Model 的实时接收与重连状态机。

const terminalTenant = "default"

// ---------------------------------------------------------------------------
// 1. 同一 URL
// ---------------------------------------------------------------------------

func TestMultiTerminalSameBoardURL(t *testing.T) {
	// 两个终端各自从配置解析 web base，得到的看板地址必须逐字符相同。
	first := URLFor("http://localhost:5174")
	second := URLFor("http://localhost:5174/")
	if first != second {
		t.Fatalf("two terminals resolved different board URL: %q vs %q", first, second)
	}
	if !strings.HasSuffix(first, MonitorPath) {
		t.Fatalf("board URL %q does not end with %q", first, MonitorPath)
	}
	if strings.ContainsAny(first, "?#") {
		t.Fatalf("board URL must not carry query or fragment: %q", first)
	}

	// T-P2-42 去掉了看板 URL 行：两个终端的顶栏现在都是 logo，URL 只保留在
	// "o" 打开的浏览器入口里，因此断言顶栏不泄露 URL 且两个终端渲染一致。
	renderURL := func(taskID string) string {
		m := NewModel(TaskState{TaskID: taskID, Status: "running"}, nil,
			Options{NoColor: true, WebURL: first})
		next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		return next.(*Model).View()
	}
	for _, taskID := range []string{"task-terminal-a", "task-terminal-b"} {
		view := renderURL(taskID)
		if strings.Contains(view, first) {
			t.Fatalf("terminal %s must not render the board URL bar:\n%s", taskID, view)
		}
		if !hasWordmark(view) {
			t.Fatalf("terminal %s must open with the wordmark:\n%s", taskID, view)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 两终端并发建任务 + ListTasks 标签
// ---------------------------------------------------------------------------

func TestMultiTerminalTasksAndLabels(t *testing.T) {
	client, cleanup := newBoardClient(t)
	defer cleanup()

	type terminal struct {
		session string
		label   string
		goal    string
	}
	terminals := []terminal{
		{session: "sess-a", label: "hostA:/repo", goal: "两个终端各建一个任务 A"},
		{session: "sess-b", label: "hostB:/repo", goal: "两个终端各建一个任务 B"},
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []*harnesspb.TaskView
		errs    []error
	)
	for _, term := range terminals {
		wg.Add(1)
		go func(term terminal) {
			defer wg.Done()
			resp, err := client.CreateTask(context.Background(), &harnesspb.CreateTaskRequest{
				TenantId:      terminalTenant,
				WorkspaceRoot: "/repo",
				Goal:          term.goal,
				SessionId:     term.session,
				TerminalLabel: term.label,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			created = append(created, resp.GetTask())
		}(term)
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent create failed: %v", errs)
	}
	if len(created) != 2 {
		t.Fatalf("created %d tasks, want 2", len(created))
	}

	// 同一看板：两条任务同租户、任务号不同、终端标签不同。
	if created[0].GetTenantId() != created[1].GetTenantId() {
		t.Fatalf("tenants differ: %q vs %q", created[0].GetTenantId(), created[1].GetTenantId())
	}
	if created[0].GetTaskId() == created[1].GetTaskId() {
		t.Fatalf("duplicated task id %q", created[0].GetTaskId())
	}
	if created[0].GetTerminalLabel() == created[1].GetTerminalLabel() {
		t.Fatalf("terminal labels must differ, both are %q", created[0].GetTerminalLabel())
	}

	list, err := client.ListTasks(context.Background(), &harnesspb.ListTasksRequest{TenantId: terminalTenant})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(list.GetTasks()) != 2 {
		t.Fatalf("ListTasks returned %d tasks, want 2", len(list.GetTasks()))
	}
	labels := map[string]bool{}
	for _, task := range list.GetTasks() {
		if task.GetTenantId() != terminalTenant {
			t.Fatalf("task %s leaked tenant %q", task.GetTaskId(), task.GetTenantId())
		}
		if task.GetTerminalLabel() == "" || task.GetSessionId() == "" {
			t.Fatalf("task %s lost terminal identity: session=%q label=%q",
				task.GetTaskId(), task.GetSessionId(), task.GetTerminalLabel())
		}
		labels[task.GetTerminalLabel()] = true
	}
	if len(labels) != 2 {
		t.Fatalf("ListTasks returned %d distinct labels, want 2: %v", len(labels), labels)
	}

	// 另一个租户看不到这两条（看板按 tenant 聚合，不按 project 过滤）。
	other, err := client.ListTasks(context.Background(), &harnesspb.ListTasksRequest{TenantId: "tenant-other"})
	if err != nil {
		t.Fatalf("list other tenant: %v", err)
	}
	if len(other.GetTasks()) != 0 {
		t.Fatalf("tenant isolation broken: %d tasks leaked", len(other.GetTasks()))
	}
}

// ---------------------------------------------------------------------------
// 3. 实时事件 + 断线重连
// ---------------------------------------------------------------------------

func TestMultiTerminalStreamAndReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newTestHub()
	board := URLFor(DefaultWebBaseURL)

	terminalA := newTerminalModel(t, ctx, hub, "task-a", board)
	terminalB := newTerminalModel(t, ctx, hub, "task-b", board)
	for _, terminal := range []*Model{terminalA, terminalB} {
		if !terminal.connected {
			t.Fatalf("terminal %s did not connect to the event stream", terminal.state.TaskID)
		}
	}
	if terminalA.webURL != terminalB.webURL {
		t.Fatalf("terminals watch different boards: %q vs %q", terminalA.webURL, terminalB.webURL)
	}

	// 两终端同时收到同一条实时事件。
	hub.publish(testEvent(1, "stage.transition", "b", "c"))
	receiveOne(t, terminalA)
	receiveOne(t, terminalB)
	if terminalA.lastSeq != 1 || terminalB.lastSeq != 1 {
		t.Fatalf("lastSeq after first event: A=%d B=%d, want 1", terminalA.lastSeq, terminalB.lastSeq)
	}

	// B 断线：事件 2 只在线上期间落在 A 那里。
	terminalB = dropStream(t, terminalB)
	if !terminalB.reconnecting || terminalB.connected {
		t.Fatalf("terminal B did not enter reconnect state: reconnecting=%v connected=%v",
			terminalB.reconnecting, terminalB.connected)
	}
	hub.publish(testEvent(2, "stage.transition", "c", "d"))
	receiveOne(t, terminalA)

	// B 重连：按 lastSeq 补齐缺的那条，再切回实时流。
	terminalB = reconnect(t, terminalB)
	if !terminalB.connected || terminalB.reconnecting {
		t.Fatalf("terminal B did not resume the stream: connected=%v reconnecting=%v",
			terminalB.connected, terminalB.reconnecting)
	}
	if got := seqsOf(terminalB.events); !equalSeqs(got, []int64{1, 2}) {
		t.Fatalf("terminal B events after reconnect = %v, want [1 2] (no gap, no duplicate)", got)
	}
	if got := seqsOf(terminalA.events); !equalSeqs(got, []int64{1, 2}) {
		t.Fatalf("terminal A events = %v, want [1 2]", got)
	}

	// 重连后继续实时接收，且重复投递同一 seq 不会产生第二行。
	hub.publish(testEvent(3, "stage.transition", "d", "e"))
	receiveOne(t, terminalA)
	receiveOne(t, terminalB)
	terminalA = deliver(t, terminalA, eventMsg{Event: testEvent(3, "stage.transition", "d", "e")})
	if got := seqsOf(terminalA.events); !equalSeqs(got, []int64{1, 2, 3}) {
		t.Fatalf("duplicate delivery was not deduped: %v", got)
	}

	// 两终端最终收敛到同一事件序列与同一看板。
	if !equalSeqs(seqsOf(terminalA.events), seqsOf(terminalB.events)) {
		t.Fatalf("terminals diverged: A=%v B=%v", seqsOf(terminalA.events), seqsOf(terminalB.events))
	}

	// 渲染快照：本次没有在真实终端目视，只记录固定尺寸下的渲染结果，报告里如实标注。
	t.Logf("terminal A view (task-a, %s):\n%s", board, terminalA.View())
	t.Logf("terminal B view (task-b, %s):\n%s", board, terminalB.View())
}

// ---------------------------------------------------------------------------
// 终端模型驱动
// ---------------------------------------------------------------------------

func newTerminalModel(t *testing.T, ctx context.Context, src Source, taskID, board string) *Model {
	t.Helper()
	m := NewModel(TaskState{TaskID: taskID, Status: "running", CurrentStage: "b", Attempts: map[string]int{"b": 1}}, src,
		Options{NoColor: true, WebURL: board})
	m = m.WithContext(ctx)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(*Model)

	// catch-up（lastSeq=0）→ 订阅实时流，等价于 CLI 启动后的首次连流。
	m = deliver(t, m, m.catchUpCmd()())
	m = deliver(t, m, m.connectCmd()())
	return m
}

func deliver(t *testing.T, m *Model, msg tea.Msg) *Model {
	t.Helper()
	if msg == nil {
		t.Fatalf("terminal %s produced a nil message", m.state.TaskID)
	}
	next, _ := m.Update(msg)
	return next.(*Model)
}

func receiveOne(t *testing.T, m *Model) *Model {
	t.Helper()
	cmd := m.nextEventCmd()
	if cmd == nil {
		t.Fatalf("terminal %s has no active stream", m.state.TaskID)
	}
	return deliver(t, m, cmd())
}

func dropStream(t *testing.T, m *Model) *Model {
	t.Helper()
	next, _ := m.Update(watchErrMsg{Err: net.ErrClosed})
	return next.(*Model)
}

// reconnect 直接注入 reconnectMsg：真实路径是 reconnectMin(1s) 的 tick，
// 测试不等真实退避时间，只验证「退避到点 → 补齐 → 重新订阅」这条链。
func reconnect(t *testing.T, m *Model) *Model {
	t.Helper()
	m = deliver(t, m, reconnectMsg{})
	m = deliver(t, m, m.catchUpCmd()())
	m = deliver(t, m, m.connectCmd()())
	return m
}

func seqsOf(lines []views.EventLine) []int64 {
	out := make([]int64, 0, len(lines))
	for _, line := range lines {
		out = append(out, line.Seq)
	}
	return out
}

func equalSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func testEvent(seq int64, typ, from, to string) Event {
	return Event{
		Seq:      seq,
		Type:     typ,
		Stage:    to,
		Severity: "info",
		Time:     time.Date(2026, 11, 16, 10, 0, int(seq), 0, time.UTC),
		Payload:  []byte(`{"from":"` + from + `","to":"` + to + `"}`),
	}
}

// ---------------------------------------------------------------------------
// 测试替身：共享事件中心（相当于按 tenant 订阅的服务端事件流）
// ---------------------------------------------------------------------------

type testHub struct {
	mu       sync.Mutex
	events   []Event
	watchers []chan WatchEvent
}

func newTestHub() *testHub { return &testHub{} }

func (h *testHub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
	for _, ch := range h.watchers {
		select {
		case ch <- WatchEvent{Event: ev}:
		default:
		}
	}
}

func (h *testHub) Task(ctx context.Context) (TaskState, error) {
	return TaskState{TaskID: "task-shared", Status: "running", CurrentStage: "c"}, nil
}

func (h *testHub) Abort(ctx context.Context) error { return nil }

func (h *testHub) ListEvents(ctx context.Context, sinceSeq int64) ([]Event, int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Event
	next := sinceSeq
	for _, ev := range h.events {
		if ev.Seq > sinceSeq {
			out = append(out, ev)
			if ev.Seq > next {
				next = ev.Seq
			}
		}
	}
	return out, next, nil
}

func (h *testHub) Watch(ctx context.Context, sinceSeq int64) (<-chan WatchEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan WatchEvent, 8)
	h.watchers = append(h.watchers, ch)
	return ch, nil
}

func (h *testHub) Subtasks(ctx context.Context) ([]Subtask, error)               { return nil, nil }
func (h *testHub) ModelSelections(ctx context.Context) ([]ModelSelection, error) { return nil, nil }
func (h *testHub) Handoffs(ctx context.Context) ([]Handoff, error)               { return nil, nil }
func (h *testHub) Checkpoints(ctx context.Context) ([]Checkpoint, error)         { return nil, nil }

// ---------------------------------------------------------------------------
// 测试替身：进程内 gRPC 任务服务（bufconn），只实现 CreateTask / ListTasks
// ---------------------------------------------------------------------------

type boardServer struct {
	harnesspb.UnimplementedHarnessLoopServiceServer

	mu    sync.Mutex
	tasks []*harnesspb.TaskView
	seq   int
}

func (s *boardServer) CreateTask(ctx context.Context, req *harnesspb.CreateTaskRequest) (*harnesspb.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	task := &harnesspb.TaskView{
		TaskId:        "task-" + string(rune('0'+s.seq)),
		TenantId:      req.GetTenantId(),
		ProjectId:     req.GetProjectId(),
		WorkspaceRoot: req.GetWorkspaceRoot(),
		Goal:          req.GetGoal(),
		Status:        "running",
		SessionId:     req.GetSessionId(),
		TerminalLabel: req.GetTerminalLabel(),
	}
	s.tasks = append(s.tasks, task)
	return &harnesspb.CreateTaskResponse{Task: task}, nil
}

func (s *boardServer) ListTasks(ctx context.Context, req *harnesspb.ListTasksRequest) (*harnesspb.ListTasksResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*harnesspb.TaskView, 0, len(s.tasks))
	for _, task := range s.tasks {
		if task.GetTenantId() != req.GetTenantId() {
			continue
		}
		out = append(out, task)
	}
	return &harnesspb.ListTasksResponse{Tasks: out}, nil
}

func newBoardClient(t *testing.T) (harnesspb.HarnessLoopServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	harnesspb.RegisterHarnessLoopServiceServer(server, &boardServer{})
	go func() { _ = server.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		server.Stop()
		t.Fatalf("dial bufconn: %v", err)
	}
	return harnesspb.NewHarnessLoopServiceClient(conn), func() {
		_ = conn.Close()
		server.Stop()
	}
}
