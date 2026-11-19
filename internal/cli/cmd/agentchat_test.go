package cmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agent-platform/pkg/harnessclient"
)

func TestAgentChatRequestShape(t *testing.T) {
	req := buildAgentChatRequest("tenant-a", "session-9", "D:/ws", "hi")
	if req.GetEntryAgent() != "main-agent" {
		t.Fatalf("entry agent = %q", req.GetEntryAgent())
	}
	if req.GetMessage() != "hi" {
		t.Fatalf("message = %q (must be forwarded verbatim)", req.GetMessage())
	}
	if req.GetTenantId() != "tenant-a" || req.GetSessionId() != "session-9" {
		t.Fatalf("tenant/session = %q/%q", req.GetTenantId(), req.GetSessionId())
	}
	cv := req.GetContextVars()
	if _, ok := cv["__stage_mode__"]; ok {
		t.Fatal("direct chat must not set __stage_mode__")
	}
	if cv["workspace"] != "D:/ws" {
		t.Fatalf("workspace context var = %q", cv["workspace"])
	}
	if len(cv) != 1 {
		t.Fatalf("unexpected context vars: %v", cv)
	}
}

func TestAgentChatRequestOmitsEmptyWorkspace(t *testing.T) {
	req := buildAgentChatRequest("t", "s", "", "hi")
	if _, ok := req.GetContextVars()["workspace"]; ok {
		t.Fatal("empty workspace must not be sent")
	}
}

func TestResolveAgentServerPrecedence(t *testing.T) {
	saved := opts.AgentServer
	defer func() { opts.AgentServer = saved }()

	opts.AgentServer = ""
	os.Unsetenv("HARNESS_AGENT_SERVER")
	if got := resolveAgentServer(fileConfig{}); got != DefaultAgentServerAddr {
		t.Fatalf("default = %q, want %q", got, DefaultAgentServerAddr)
	}
	if got := resolveAgentServer(fileConfig{AgentServer: "127.0.0.1:7001"}); got != "127.0.0.1:7001" {
		t.Fatalf("config = %q", got)
	}
	t.Setenv("HARNESS_AGENT_SERVER", "127.0.0.1:7002")
	if got := resolveAgentServer(fileConfig{AgentServer: "127.0.0.1:7001"}); got != "127.0.0.1:7002" {
		t.Fatalf("env = %q", got)
	}
	opts.AgentServer = "127.0.0.1:7003"
	if got := resolveAgentServer(fileConfig{AgentServer: "127.0.0.1:7001"}); got != "127.0.0.1:7003" {
		t.Fatalf("flag = %q", got)
	}
}

func TestAgentServerFlagRegistered(t *testing.T) {
	root := NewRootCmd()
	if root.PersistentFlags().Lookup("agent-server") == nil {
		t.Fatal("--agent-server flag missing")
	}
}

// TestLiveAgentChatStream drives newAgentChatFunc (the exact code path the TUI
// uses) against a running agent-service. It is skipped unless
// HARNESS_LIVE_AGENT=1 so the default suite stays hermetic.
func TestLiveAgentChatStream(t *testing.T) {
	if os.Getenv("HARNESS_LIVE_AGENT") != "1" {
		t.Skip("set HARNESS_LIVE_AGENT=1 to probe a live agent-service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := harnessclient.DialLazy(ctx, harnessclient.DefaultConfig(DefaultAgentServerAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	chat := newAgentChatFunc(client, DefaultAgentServerAddr, nil, "default", "cli-chat-live", "D:/Desktop/生涯/老公毕业论文/hlp-p2-chat")
	ch, err := chat(ctx, "hi")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	types := map[string]int{}
	var text string
	for chunk := range ch {
		types[chunk.Type]++
		if chunk.Type == "token" || chunk.Type == "response" || chunk.Type == "final" {
			text += chunk.Content
		}
		t.Logf("chunk type=%s agent=%s content=%q", chunk.Type, chunk.Agent, chunk.Content)
	}
	t.Logf("chunk types: %v", types)
	if types["error"] > 0 {
		t.Fatalf("live chat returned an error chunk")
	}
	if types["token"] == 0 && types["final"] == 0 {
		t.Fatalf("live chat produced no reply text, chunk types: %v", types)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatalf("live chat reply was empty")
	}
}
