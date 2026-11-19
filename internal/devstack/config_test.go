package devstack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderConfigLocalizesMemoryTemplate(t *testing.T) {
	c := loadContract(t)
	mem, _ := c.Item("memory")
	out := localizeConfig(`server:
  grpc_port: 50003
database:
  sqlite:
    path: /app/data/memory.db
qdrant:
  url: http://qdrant:6333
redis:
  url: redis://redis:6379
`, c, mem, nil)

	for _, want := range []string{
		"grpc_port: 50003",
		"path: .harness/dev/memory.db",
		"http://127.0.0.1:6333",
		"redis://127.0.0.1:56379",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered memory config missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "qdrant:6333") || strings.Contains(out, "redis:6379") {
		t.Fatalf("compose hostnames leaked:\n%s", out)
	}
}

func TestRenderConfigLocalizesHarnessLoopTemplate(t *testing.T) {
	c := loadContract(t)
	loop, _ := c.Item("harnessloop")
	out := localizeConfig(`server:
  address: ":50008"
postgres:
  dsn: "postgres://agent:agent_dev_password@localhost:5432/agent_platform?sslmode=disable"
policy:
  matrix_file: "config/harness_loop/stage_matrix.yaml"
agent:
  target: "127.0.0.1:50006"
memory:
  addr: "127.0.0.1:50003"
`, c, loop, nil)

	wantDSN := "postgres://agent:agent_dev_password@127.0.0.1:55432/agent_platform?sslmode=disable"
	for _, want := range []string{
		`address: "127.0.0.1:50013"`,
		wantDSN,
		`"pkg/config/harness_loop/stage_matrix.yaml"`,
		`target: "127.0.0.1:50006"`,
		`addr: "127.0.0.1:50003"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered harnessloop config missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfigRewritesServiceAliases(t *testing.T) {
	c := loadContract(t)
	gw, _ := c.Item("gateway")
	out := localizeConfig(`server:
  http_port: 9000
services:
  chat: chat-service:50001
  knowledge: knowledge-service:50002
  memory: memory-service:50003
  harness: harness-service:50007
  agent: agent-service:50006
  harness_loop: harnessloop-service:50008
llm:
  base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
`, c, gw, nil)

	for _, want := range []string{
		"chat: 127.0.0.1:50001",
		"knowledge: 127.0.0.1:50002",
		"memory: 127.0.0.1:50003",
		"harness: 127.0.0.1:50007",
		"agent: 127.0.0.1:50006",
		"harness_loop: 127.0.0.1:50013",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered gateway config missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "https://dashscope.aliyuncs.com/compatible-mode/v1") {
		t.Fatalf("external URL must be preserved:\n%s", out)
	}
}

func TestRenderConfigKeepsUnrelatedHTTPPort(t *testing.T) {
	c := loadContract(t)
	chat, _ := c.Item("chat")
	out := localizeConfig("server:\n  grpc_port: 50001\n  http_port: 8081\n", c, chat, nil)
	if !strings.Contains(out, "grpc_port: 50001") || !strings.Contains(out, "http_port: 8081") {
		t.Fatalf("unexpected rewrite for chat:\n%s", out)
	}

	a2a, _ := c.Item("a2a")
	out = localizeConfig("server:\n  grpc_port: 50004\n  http_port: 50014\n", c, a2a, nil)
	if !strings.Contains(out, "grpc_port: 50004") || !strings.Contains(out, "http_port: 50014") {
		t.Fatalf("unexpected rewrite for a2a:\n%s", out)
	}
}

func TestRenderConfigWritesOutput(t *testing.T) {
	root := t.TempDir()
	tpl := filepath.Join(root, "services", "memory-service", "config.example.yaml")
	if err := os.MkdirAll(filepath.Dir(tpl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tpl, []byte("redis:\n  url: redis://redis:6379\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadContract(t)
	item, _ := c.Item("memory")
	item.ConfigTemplate = "services/memory-service/config.example.yaml"
	item.ConfigOutput = ".harness/dev/conf/memory.yaml"
	if err := RenderConfig(root, c, item); err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".harness", "dev", "conf", "memory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "redis://127.0.0.1:56379") {
		t.Fatalf("config not localized:\n%s", raw)
	}

	item.ConfigTemplate = "services/missing.yaml"
	if err := RenderConfig(root, c, item); err == nil {
		t.Fatal("missing template must fail")
	}
}

func TestRenderConfigWithoutTemplateIsNoop(t *testing.T) {
	if err := RenderConfig(t.TempDir(), nil, Item{Name: "postgres"}); err != nil {
		t.Fatalf("container config must be a no-op: %v", err)
	}
}

func TestRenderConfigInjectsLLMFromDevEnv(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".harness", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := "export HARNESS_LLM_API_KEY='test-key'\n" +
		"export HARNESS_LLM_BASE_URL='https://dashscope.example/v1'\n" +
		"export HARNESS_LLM_MODEL='test-model'\n"
	if err := os.WriteFile(filepath.Join(root, ".harness", "dev", "env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl := "server:\n  grpc_port: 50006\nllm:\n  provider: dashscope\n" +
		"  api_key: \"<your-api-key>\"\n  base_url: \"https://dashscope.aliyuncs.com/compatible-mode/v1\"\n" +
		"  model: \"qwen3.7-max\"\n  embedding_model: \"text-embedding-v3\"\n" +
		"tools:\n  - name: demo\n    api_key: \"mcp-server-key\"\n"
	if err := os.WriteFile(filepath.Join(root, "agent.example.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Contract{Version: SupportedVersion}
	item := Item{
		Name: "agent", Kind: KindProcess, Port: 50006,
		ConfigTemplate: "agent.example.yaml", ConfigOutput: ".harness/dev/conf/agent.yaml",
	}
	if err := RenderConfig(root, c, item); err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".harness/dev/conf/agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		`api_key: "test-key"`,
		`base_url: "https://dashscope.example/v1"`,
		`model: "test-model"`,
		`provider: dashscope`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `api_key: "mcp-server-key"`) {
		t.Fatalf("MCP server key under tools: must not be overwritten:\n%s", out)
	}
}

func TestRenderConfigKeepsPlaceholdersWithoutDevEnv(t *testing.T) {
	root := t.TempDir()
	tpl := "llm:\n  api_key: \"<your-api-key>\"\n"
	if err := os.WriteFile(filepath.Join(root, "agent.example.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	item := Item{Name: "agent", Port: 50006, ConfigTemplate: "agent.example.yaml", ConfigOutput: "agent.yaml"}
	if err := RenderConfig(root, &Contract{Version: SupportedVersion}, item); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "agent.yaml"))
	if !strings.Contains(string(raw), `<your-api-key>`) {
		t.Fatalf("without .harness/dev/env the placeholder must stay:\n%s", raw)
	}
}

func TestRenderConfigPinsEntryAgent(t *testing.T) {
	root := t.TempDir()
	tpl := "agent:\n  target: \"127.0.0.1:50006\"\n  entry_agent: \"default\"\n"
	if err := os.WriteFile(filepath.Join(root, "harnessloop.example.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	item := Item{Name: "harnessloop", Port: 50013, ConfigTemplate: "harnessloop.example.yaml", ConfigOutput: "harnessloop.yaml"}
	if err := RenderConfig(root, &Contract{Version: SupportedVersion}, item); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "harnessloop.yaml"))
	if !strings.Contains(string(raw), `entry_agent: "main-agent"`) {
		t.Fatalf("entry_agent must be pinned to main-agent:\n%s", raw)
	}
}
