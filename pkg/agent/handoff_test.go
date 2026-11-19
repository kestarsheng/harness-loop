package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingLLM replays scripted responses and remembers the tool names offered
// on each call, so a test can assert what an agent was allowed to call.
type recordingLLM struct {
	replies   []*LLMResponse
	seenTools [][]string
	idx       int
}

func (s *recordingLLM) Chat(_ context.Context, req *LLMRequest) (*LLMResponse, error) {
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if n, ok := fn["name"].(string); ok {
			names = append(names, n)
		}
	}
	s.seenTools = append(s.seenTools, names)

	if s.idx >= len(s.replies) {
		return &LLMResponse{Content: "done"}, nil
	}
	resp := s.replies[s.idx]
	s.idx++
	return resp, nil
}

func handoffCall(id, target string) ToolCall {
	return ToolCall{ID: id, Name: HandoffPrefix + target, Arguments: []byte(`{}`)}
}

func newHandoffRegistry(t *testing.T, agents ...*Agent) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, a := range agents {
		if err := reg.Register(a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	return reg
}

func handoffToolNames(tools []map[string]any) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if fn, ok := t["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// 1) An agent that lists itself in Handoffs must not get a self-handoff tool.
func TestBuildHandoffToolsSkipsSelf(t *testing.T) {
	self := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	other := &Agent{ID: "main-agent", Name: "main", Instructions: "route"}
	self.Handoffs = []string{"coder-agent", "main-agent"}
	reg := newHandoffRegistry(t, self, other)

	got := BuildHandoffTools(self, reg)
	if len(got) != 1 {
		t.Fatalf("expected 1 handoff tool, got %d: %+v", len(got), got)
	}
	if got[0].TargetAgentID != "main-agent" {
		t.Errorf("unexpected handoff target %q", got[0].TargetAgentID)
	}

	all := handoffToolNames(BuildAllTools(self, reg, map[string]any{}))
	if containsName(all, HandoffPrefix+"coder-agent") {
		t.Errorf("self-handoff tool must not be offered: %v", all)
	}
	if !containsName(all, HandoffPrefix+"main-agent") {
		t.Errorf("legit handoff tool missing: %v", all)
	}
}

// 2) from == to is rejected with a message carrying both sides.
func TestValidateHandoffRejectsSelf(t *testing.T) {
	coder := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	coder.Handoffs = []string{"coder-agent"}
	main := &Agent{ID: "main-agent", Name: "main", Instructions: "route"}
	reg := newHandoffRegistry(t, coder, main)

	err := ValidateHandoff(coder, "coder-agent", reg)
	if err == nil {
		t.Fatal("expected self handoff to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "agent coder-agent cannot handoff to coder-agent") {
		t.Errorf("unexpected message: %q", msg)
	}
	if !errors.Is(err, ErrInvalidHandoff) {
		t.Errorf("error should unwrap to ErrInvalidHandoff: %v", err)
	}
}

// 3) A→B succeeds once, and B's tool set is rebuilt from B's own Handoffs:
// no self tool, and no leftover tool belonging to A.
func TestHandoffSwitchesAgentAndRebuildsTools(t *testing.T) {
	main := &Agent{ID: "main-agent", Name: "main", Instructions: "route"}
	// coder-agent's config deliberately contains a self entry, mirroring the
	// production data that triggered the incident.
	coder := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	main.Handoffs = []string{"coder-agent"}
	coder.Handoffs = []string{"coder-agent"}

	reg := newHandoffRegistry(t, main, coder)
	llm := &recordingLLM{replies: []*LLMResponse{
		{Content: "这个交给 coder", ToolCalls: []ToolCall{handoffCall("c1", "coder-agent")}},
		{Content: "脚本如下：print('hi')"},
	}}
	e := NewEngine(reg, llm, &stubTools{}, nil, DefaultEngineConfig())

	res, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-handoff", Message: "帮我写一个简洁的脚本", EntryAgent: "main-agent",
	})
	if err != nil {
		t.Fatalf("normal A->B handoff must succeed, got error: %v", err)
	}
	if strings.TrimSpace(res.Response) == "" {
		t.Fatalf("response must carry coder-agent's answer, got empty")
	}
	if !strings.Contains(res.Response, "print('hi')") {
		t.Errorf("unexpected response: %q", res.Response)
	}
	if len(llm.seenTools) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(llm.seenTools))
	}
	// First call runs as main-agent: it may transfer to coder.
	if !containsName(llm.seenTools[0], HandoffPrefix+"coder-agent") {
		t.Errorf("entry agent should keep its declared handoff tool: %v", llm.seenTools[0])
	}
	// Second call runs as coder-agent: no tool may transfer to coder itself.
	if containsName(llm.seenTools[1], HandoffPrefix+"coder-agent") {
		t.Errorf("coder-agent must not be offered a self-handoff tool: %v", llm.seenTools[1])
	}
	if len(llm.seenTools[1]) != 0 {
		t.Errorf("coder-agent declares no valid handoffs, expected no handoff tools: %v", llm.seenTools[1])
	}

	// History records exactly one handoff, from main to coder.
	var hops int
	for _, r := range res.AgentHistory {
		if r.Action == "handoff" {
			hops++
			if r.AgentID != "main-agent" || r.HandoffTo != "coder-agent" {
				t.Errorf("unexpected handoff record: %+v", r)
			}
		}
	}
	if hops != 1 {
		t.Errorf("expected exactly 1 handoff, got %d", hops)
	}
}

// 4) A→B→A is rejected as a cycle and the run stops with a clear error.
func TestHandoffCycleRejected(t *testing.T) {
	main := &Agent{ID: "main-agent", Name: "main", Instructions: "route"}
	coder := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	main.Handoffs = []string{"coder-agent"}
	coder.Handoffs = []string{"main-agent"}

	reg := newHandoffRegistry(t, main, coder)
	llm := &recordingLLM{replies: []*LLMResponse{
		{Content: "to coder", ToolCalls: []ToolCall{handoffCall("c1", "coder-agent")}},
		{Content: "back to main", ToolCalls: []ToolCall{handoffCall("c2", "main-agent")}},
		{Content: "should never be reached"},
	}}
	e := NewEngine(reg, llm, &stubTools{}, nil, DefaultEngineConfig())

	res, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-cycle", Message: "go", EntryAgent: "main-agent",
	})
	if err == nil {
		t.Fatal("A->B->A must be rejected")
	}
	if !strings.Contains(err.Error(), "agent coder-agent cannot handoff to main-agent") {
		t.Errorf("message must name both sides: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("message should explain the cycle: %q", err.Error())
	}
	// No retry: the loop stopped at the offending step.
	if len(llm.seenTools) != 2 {
		t.Errorf("handoff rejection must not be retried, LLM calls = %d", len(llm.seenTools))
	}
	if res != nil && strings.TrimSpace(res.Response) != "" {
		t.Errorf("rejected run must not return a response: %q", res.Response)
	}
}

// 5) B→B is rejected as self handoff even if the model emits the tool name.
func TestHandoffSelfCallRejected(t *testing.T) {
	coder := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	coder.Handoffs = []string{"coder-agent"}
	reg := newHandoffRegistry(t, coder)

	llm := &recordingLLM{replies: []*LLMResponse{
		{Content: "handing to myself", ToolCalls: []ToolCall{handoffCall("c1", "coder-agent")}},
	}}
	e := NewEngine(reg, llm, &stubTools{}, nil, DefaultEngineConfig())

	_, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-self", Message: "go", EntryAgent: "coder-agent",
	})
	if err == nil {
		t.Fatal("B->B must be rejected")
	}
	if !strings.Contains(err.Error(), "agent coder-agent cannot handoff to coder-agent") {
		t.Errorf("unexpected message: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "self handoff") {
		t.Errorf("message should name the self-handoff reason: %q", err.Error())
	}
}

// 6) Hop budget bounds a long non-cyclic chain.
func TestHandoffHopLimitRejected(t *testing.T) {
	a := &Agent{ID: "a", Name: "a", Instructions: "a"}
	b := &Agent{ID: "b", Name: "b", Instructions: "b"}
	c := &Agent{ID: "c", Name: "c", Instructions: "c"}
	d := &Agent{ID: "d", Name: "d", Instructions: "d"}
	a.Handoffs = []string{"b"}
	b.Handoffs = []string{"c"}
	c.Handoffs = []string{"d"}

	reg := newHandoffRegistry(t, a, b, c, d)
	llm := &recordingLLM{replies: []*LLMResponse{
		{Content: "a->b", ToolCalls: []ToolCall{handoffCall("1", "b")}},
		{Content: "b->c", ToolCalls: []ToolCall{handoffCall("2", "c")}},
		{Content: "c->d", ToolCalls: []ToolCall{handoffCall("3", "d")}},
	}}
	cfg := DefaultEngineConfig()
	cfg.MaxHandoffHops = 2
	e := NewEngine(reg, llm, &stubTools{}, nil, cfg)

	_, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-hops", Message: "go", EntryAgent: "a",
	})
	if err == nil {
		t.Fatal("chain longer than MaxHandoffHops must be rejected")
	}
	if !strings.Contains(err.Error(), "hop limit") {
		t.Errorf("unexpected message: %q", err.Error())
	}
}

// 7) The guard itself: visited set + hop budget + resume seeding.
func TestHandoffGuardVisitedSet(t *testing.T) {
	g := NewHandoffGuard("a", 2)
	if err := g.Check("a"); err == nil {
		t.Error("self must be rejected")
	}
	if err := g.Check("b"); err != nil {
		t.Fatalf("a->b should be allowed: %v", err)
	}
	g.Record("b")
	if err := g.Check("a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("a->b->a must be a cycle, got %v", err)
	}
	if err := g.Check("c"); err != nil {
		t.Fatalf("a->b->c should be allowed: %v", err)
	}
	g.Record("c")
	if g.Hops() != 2 {
		t.Errorf("expected 2 hops, got %d", g.Hops())
	}
	if err := g.Check("d"); err == nil || !strings.Contains(err.Error(), "hop limit") {
		t.Errorf("expected hop limit at 3rd hop, got %v", err)
	}

	// Resume seeding: history recorded a->b, the run resumes on b, so neither a
	// (already visited) nor b (self) may be handed to again.
	execCtx := NewExecutionContext("s")
	execCtx.AgentHistory = []AgentExecutionRecord{{AgentID: "a", Action: "handoff", HandoffTo: "b"}}
	rg := NewHandoffGuardFromContext(execCtx, "b", 3)
	if err := rg.Check("b"); err == nil || !strings.Contains(err.Error(), "self handoff") {
		t.Errorf("resumed run must reject self handoff, got %v", err)
	}
	if err := rg.Check("a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("resumed run must not walk back to a, got %v", err)
	}
}

// 8) The handoff tool call must be paired with a tool result, otherwise the
// next agent's request carries an orphan tool_call and gateways reject it.
func TestHandoffToolCallIsPairedWithResult(t *testing.T) {
	main := &Agent{ID: "main-agent", Name: "main", Instructions: "route"}
	coder := &Agent{ID: "coder-agent", Name: "coder", Instructions: "code"}
	main.Handoffs = []string{"coder-agent"}

	reg := newHandoffRegistry(t, main, coder)
	llm := &recordingLLM{replies: []*LLMResponse{
		{Content: "to coder", ToolCalls: []ToolCall{handoffCall("call-h1", "coder-agent")}},
		{Content: "done"},
	}}
	store := &captureStore{}
	e := NewEngine(reg, llm, &stubTools{}, store, DefaultEngineConfig())

	if _, err := e.Run(context.Background(), &ExecutionRequest{
		SessionID: "s-pair", Message: "go", EntryAgent: "main-agent",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var paired bool
	for _, msg := range store.last {
		if msg.Role == "tool" && msg.ToolID == "call-h1" {
			paired = true
			if !strings.Contains(msg.Content, "handoff_accepted") {
				t.Errorf("unexpected handoff tool result: %q", msg.Content)
			}
		}
	}
	if !paired {
		t.Fatalf("handoff call has no paired tool result: %#v", store.last)
	}
}
