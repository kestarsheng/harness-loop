package devstack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contractPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "scripts", "dev-stack.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("contract not found at %s: %v", path, err)
	}
	return path
}

func loadContract(t *testing.T) *Contract {
	t.Helper()
	c, err := LoadContract(contractPath(t))
	if err != nil {
		t.Fatalf("LoadContract: %v", err)
	}
	return c
}

func TestLoadContract14Items(t *testing.T) {
	c := loadContract(t)

	if c.Version != SupportedVersion {
		t.Fatalf("version = %d", c.Version)
	}
	if c.Tenant != "tenant-devfull" {
		t.Fatalf("tenant = %q", c.Tenant)
	}
	if got := len(c.Items); got != 14 {
		t.Fatalf("items = %d, want 14", got)
	}

	want := []struct {
		name   string
		kind   string
		group  string
		port   int
		health string
	}{
		{"postgres", KindContainer, GroupInfra, 55432, HealthExec},
		{"mongo", KindContainer, GroupInfra, 27017, HealthPort},
		{"qdrant", KindContainer, GroupInfra, 6333, HealthHTTP},
		{"redis", KindContainer, GroupInfra, 56379, HealthPort},
		{"memory", KindProcess, GroupAgent, 50003, HealthPort},
		{"knowledge", KindProcess, GroupAgent, 50002, HealthPort},
		{"chat", KindProcess, GroupAgent, 50001, HealthPort},
		{"a2a", KindProcess, GroupAgent, 50004, HealthPort},
		{"mcp", KindProcess, GroupAgent, 50005, HealthPort},
		{"harness-service", KindProcess, GroupAgent, 50007, HealthPort},
		{"agent", KindProcess, GroupAgent, 50006, HealthPort},
		{"harnessloop", KindProcess, GroupAgent, 50013, HealthPort},
		{"gateway", KindProcess, GroupAgent, 9000, HealthHTTP},
		{"vite", KindFrontend, GroupBoard, 5174, HealthHTTP},
	}
	for i, exp := range want {
		got := c.Items[i]
		if got.Name != exp.name || got.Kind != exp.kind || got.Port != exp.port || got.Health.Type != exp.health {
			t.Fatalf("items[%d] = %s/%s/%d/%s, want %s/%s/%d/%s",
				i, got.Name, got.Kind, got.Port, got.Health.Type,
				exp.name, exp.kind, exp.port, exp.health)
		}
		if got.Group != exp.group {
			t.Fatalf("items[%d] %s group = %q, want %q", i, got.Name, got.Group, exp.group)
		}
		if !got.Required {
			t.Fatalf("%s must be required", got.Name)
		}
		if got.Command == "" {
			t.Fatalf("%s command is empty", got.Name)
		}
	}
}

// TestEveryItemDeclaresALegalGroup pins the group partition `loop` relies on:
// 14 items, every one in a legal group, 4 infra + 9 agent + 1 board.
func TestEveryItemDeclaresALegalGroup(t *testing.T) {
	c := loadContract(t)
	if len(c.Items) != 14 {
		t.Fatalf("items = %d, want 14", len(c.Items))
	}
	counts := map[string]int{}
	for _, it := range c.Items {
		if !ValidGroup(it.Group) {
			t.Fatalf("%s has illegal group %q", it.Name, it.Group)
		}
		counts[it.Group]++
	}
	if counts[GroupInfra] != 4 {
		t.Fatalf("infra items = %d, want 4", counts[GroupInfra])
	}
	if counts[GroupAgent] != 9 {
		t.Fatalf("agent items = %d, want 9", counts[GroupAgent])
	}
	if counts[GroupBoard] != 1 {
		t.Fatalf("board items = %d, want 1", counts[GroupBoard])
	}

	infra := c.ItemsInGroups(GroupInfra)
	for _, it := range infra {
		if it.Kind != KindContainer {
			t.Fatalf("infra item %s must be a data container, got kind %s", it.Name, it.Kind)
		}
	}
	agent := c.ItemsInGroups(GroupAgent)
	if strings.Join(namesOf(agent), ",") != "memory,knowledge,chat,a2a,mcp,harness-service,agent,harnessloop,gateway" {
		t.Fatalf("agent group = %v", namesOf(agent))
	}
	board := c.ItemsInGroups(GroupBoard)
	if len(board) != 1 || board[0].Name != "vite" || !board[0].Board {
		t.Fatalf("board group = %+v, want the board:true vite item", board)
	}
	// infra+agent is exactly what `loop` starts: everything but the board.
	if got := len(c.Items) - len(board); got != 13 {
		t.Fatalf("non-board items = %d, want 13", got)
	}
}

func namesOf(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestHarnessLoopProbeTargets50013(t *testing.T) {
	c := loadContract(t)
	item, ok := c.HarnessLoop()
	if !ok {
		t.Fatal("harnessloop item missing")
	}
	if item.Port != 50013 {
		t.Fatalf("harnessloop port = %d", item.Port)
	}
	if item.Health.Type != HealthPort || item.Health.Address != "127.0.0.1:50013" {
		t.Fatalf("harnessloop health = %+v", item.Health)
	}
}

// TestContractDeclaresExactlyOneProbe pins T-P2-43 on the shipped contract: the
// stack probe is declared, not inferred, and only harnessloop carries it.
func TestContractDeclaresExactlyOneProbe(t *testing.T) {
	c := loadContract(t)
	if len(c.Items) != 14 {
		t.Fatalf("items = %d, want 14", len(c.Items))
	}
	var flagged []string
	for _, it := range c.Items {
		if it.Probe {
			flagged = append(flagged, it.Name)
		}
	}
	if strings.Join(flagged, ",") != "harnessloop" {
		t.Fatalf("probe:true items = %v, want exactly [harnessloop]", flagged)
	}
	probe, ok := c.ProbeItem()
	if !ok {
		t.Fatal("ProbeItem must find the declared probe")
	}
	if probe.Name != "harnessloop" {
		t.Fatalf("probe item = %q", probe.Name)
	}
	if probe.Kind != KindProcess {
		t.Fatalf("probe kind = %q, want process", probe.Kind)
	}
	if probe.Group != GroupAgent {
		t.Fatalf("probe group = %q, want %q", probe.Group, GroupAgent)
	}
	if probe.Health.Type == "" {
		t.Fatal("probe must declare a health check")
	}
	if probe.Port != 50013 || probe.Health.Address != "127.0.0.1:50013" {
		t.Fatalf("probe must stay on 50013: port=%d health=%+v", probe.Port, probe.Health)
	}
	// The legacy name lookup must keep resolving to the same item.
	legacy, ok := c.HarnessLoop()
	if !ok || legacy.Name != probe.Name {
		t.Fatalf("HarnessLoop() = %q,%v; want the declared probe", legacy.Name, ok)
	}
	// Ports and commands stay untouched by the probe flag.
	if it, _ := c.Item("harnessloop"); it.Command != ".harness/dev/bin/harnessloop-service -config .harness/dev/conf/harnessloop.yaml" {
		t.Fatalf("harnessloop command changed: %q", it.Command)
	}
}

// TestProbeItemPrefersDeclarationOverName guards against reintroducing the old
// naming convention: an item literally called "harnessloop" must not win over
// the item flagged probe:true.
func TestProbeItemPrefersDeclarationOverName(t *testing.T) {
	c := &Contract{Version: SupportedVersion, Items: []Item{
		{Name: "harnessloop", Kind: KindProcess, Group: GroupAgent, Port: 50013, Command: "true",
			Health: Health{Type: HealthPort, Address: "127.0.0.1:50013"}},
		{Name: "obs-collector", Kind: KindProcess, Group: GroupAgent, Port: 50020, Command: "true",
			Health: Health{Type: HealthPort, Address: "127.0.0.1:50020"}, Probe: true},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	probe, ok := c.ProbeItem()
	if !ok || probe.Name != "obs-collector" {
		t.Fatalf("ProbeItem() = %q,%v; want the declared obs-collector", probe.Name, ok)
	}
	if legacy, _ := c.HarnessLoop(); legacy.Name != "harnessloop" {
		t.Fatalf("HarnessLoop() name lookup = %q", legacy.Name)
	}
}

func TestProcessItemsDeclareConfigOutput(t *testing.T) {
	c := loadContract(t)
	for _, it := range c.Items {
		if it.Kind != KindProcess {
			if it.ConfigTemplate != "" {
				t.Fatalf("%s must not declare config_template", it.Name)
			}
			continue
		}
		if it.ConfigTemplate == "" || it.ConfigOutput == "" {
			t.Fatalf("process %s must declare config_template/config_output", it.Name)
		}
		if it.Health.Type == HealthPort && it.Health.Address == "" {
			t.Fatalf("process %s must declare health.address", it.Name)
		}
	}
}

func TestPIDFileStem(t *testing.T) {
	c := loadContract(t)
	item, _ := c.Item("harness-service")
	if item.PIDFileStem() != "harness-svc" {
		t.Fatalf("harness-service pid stem = %q", item.PIDFileStem())
	}
	other, _ := c.Item("gateway")
	if other.PIDFileStem() != "gateway" {
		t.Fatalf("gateway pid stem = %q", other.PIDFileStem())
	}
}

func TestDirsResolveUnderRoot(t *testing.T) {
	c := loadContract(t)
	root := filepath.Join(t.TempDir(), "repo")
	d := c.Dirs(root)
	if d.Dev != filepath.Join(root, ".harness", "dev") {
		t.Fatalf("dev dir = %s", d.Dev)
	}
	for _, dir := range []string{d.Bin, d.Conf, d.Logs, d.Run} {
		if filepath.Dir(dir) != d.Dev {
			t.Fatalf("%s is not under %s", dir, d.Dev)
		}
	}
}

func TestContractValidationRejects(t *testing.T) {
	base := loadContract(t)
	clone := func() *Contract {
		raw, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		var c Contract
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}

	tests := []struct {
		name   string
		mutate func(*Contract)
	}{
		{"duplicate name", func(c *Contract) { c.Items[1].Name = c.Items[0].Name }},
		{"duplicate port", func(c *Contract) { c.Items[1].Port = c.Items[0].Port }},
		{"duplicate pid stem", func(c *Contract) { c.Items[1].PidName = c.Items[0].PIDFileStem() }},
		{"unknown kind", func(c *Contract) { c.Items[0].Kind = "vm" }},
		{"missing command", func(c *Contract) { c.Items[0].Command = "" }},
		{"unknown health", func(c *Contract) { c.Items[0].Health.Type = "tcp" }},
		{"http without url", func(c *Contract) { c.Items[2].Health.URL = "" }},
		{"bad http url", func(c *Contract) { c.Items[2].Health.URL = "not a url" }},
		{"exec without command", func(c *Contract) { c.Items[0].Health.Command = "" }},
		{"config output without template", func(c *Contract) { c.Items[4].ConfigTemplate = "" }},
		{"bad version", func(c *Contract) { c.Version = 2 }},
		{"empty items", func(c *Contract) { c.Items = nil }},
		{"missing group", func(c *Contract) { c.Items[0].Group = "" }},
		{"unknown group", func(c *Contract) { c.Items[0].Group = "vm" }},
		{"board item outside board group", func(c *Contract) { c.Items[13].Group = GroupAgent }},
		{"two probe items", func(c *Contract) { c.Items[12].Probe = true }},
		{"probe is a frontend", func(c *Contract) { c.Items[11].Kind = KindFrontend }},
		{"probe is a container", func(c *Contract) {
			c.Items[11].Kind = KindContainer
			c.Items[11].ConfigTemplate, c.Items[11].ConfigOutput = "", ""
		}},
		{"probe outside agent group", func(c *Contract) { c.Items[11].Group = GroupInfra }},
		{"probe without health", func(c *Contract) { c.Items[11].Health = Health{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := clone()
			tc.mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
		})
	}
}

func TestLoadContractErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadContract(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing contract must fail")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContract(bad); err == nil {
		t.Fatal("malformed JSON must fail")
	}
}
