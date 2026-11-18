package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeLayerFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy_layers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write layer file: %v", err)
	}
	return path
}

func TestLayerStoreLoadsThreeLayers(t *testing.T) {
	path := writeLayerFile(t, `
version: 1
tenants:
  t-1:
    deny_tools: [write_file]
roles:
  reviewer:
    rules:
      - id: role.approval.exec
        tool: run_command
        effect: require_approval
        requires_approval: true
tasks:
  task-1:
    allow_tools: [read_file]
`)
	store, err := LoadLayerStore(path)
	if err != nil {
		t.Fatalf("load layer store: %v", err)
	}
	tenants, roles, tasks := store.Len()
	if tenants != 1 || roles != 1 || tasks != 1 {
		t.Fatalf("len = %d/%d/%d, want 1/1/1", tenants, roles, tasks)
	}

	sets := store.Resolve("t-1", "reviewer", "", "task-1")
	if sets.Tenant == nil || sets.TenantSource != LayerSourceConfig {
		t.Fatalf("tenant layer not assembled: %+v", sets)
	}
	if sets.Role == nil || sets.RoleSource != LayerSourceConfig {
		t.Fatalf("role layer not assembled: %+v", sets)
	}
	if sets.Task == nil || sets.TaskSource != LayerSourceConfig {
		t.Fatalf("task layer not assembled: %+v", sets)
	}
	if got := sets.Describe(); got != "tenant=config,role=config,task=config" {
		t.Fatalf("describe = %q", got)
	}

	unknown := store.Resolve("t-none", "nobody", "", "task-none")
	if unknown.Tenant != nil || unknown.Role != nil || unknown.Task != nil {
		t.Fatalf("unknown ids must not assemble layers: %+v", unknown)
	}
	if got := unknown.Describe(); got != "tenant=none,role=none,task=none" {
		t.Fatalf("describe = %q", got)
	}
}

// roleName 为空时要用 agentID 回落：stage 路径只带得出 entry_agent。
func TestLayerStoreRoleFallsBackToAgentID(t *testing.T) {
	store := NewLayerStore()
	if err := store.PutRole("main-agent", &PolicySet{DenyTools: []string{"list_dir"}}); err != nil {
		t.Fatalf("put role: %v", err)
	}
	sets := store.Resolve("t-1", "", "main-agent", "task-1")
	if sets.Role == nil || sets.RoleSource != LayerSourceConfig {
		t.Fatalf("role must fall back to agent id: %+v", sets)
	}
}

func TestLayerStoreRejectsUnknownEffect(t *testing.T) {
	path := writeLayerFile(t, `
version: 1
tenants:
  t-1:
    rules:
      - id: bad
        tool: run_command
        effect: maybe
`)
	if _, err := LoadLayerStore(path); err == nil {
		t.Fatal("unknown effect must be rejected at load time, not at decision time")
	}
}

func TestLayerStoreMissingFileDegrades(t *testing.T) {
	store, err := LoadLayerStore(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("missing file must report an error")
	}
	if store == nil {
		t.Fatal("missing file must still return an empty store")
	}
	if tenants, roles, tasks := store.Len(); tenants+roles+tasks != 0 {
		t.Fatalf("empty store expected, got %d/%d/%d", tenants, roles, tasks)
	}
}

func TestTaskPolicyFromSnapshot(t *testing.T) {
	if set := TaskPolicyFromSnapshot([]byte(`{"mode":"enforce"}`)); set != nil {
		t.Fatalf("snapshot without tool_policy must yield nil, got %+v", set)
	}
	if set := TaskPolicyFromSnapshot(nil); set != nil {
		t.Fatalf("empty snapshot must yield nil, got %+v", set)
	}

	want := &PolicySet{Rules: []Rule{{
		ID: "task.approval.exec", Tool: "run_command", Stages: []string{"c"},
		Effect: EffectRequireApproval, RequiresApproval: true,
	}}}
	raw, err := SnapshotWithTaskPolicy([]byte(`{"mode":"enforce"}`), want)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	// 其它键必须保留，验收标准还在里面。
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if _, ok := doc["mode"]; !ok {
		t.Fatalf("snapshot lost unrelated keys: %s", raw)
	}

	got := TaskPolicyFromSnapshot(raw)
	if got == nil || len(got.Rules) != 1 || got.Rules[0].ID != "task.approval.exec" {
		t.Fatalf("snapshot task policy = %+v", got)
	}
	if got.Rules[0].Effect != EffectRequireApproval || !got.Rules[0].RequiresApproval {
		t.Fatalf("task policy lost effect: %+v", got.Rules[0])
	}
}

// 四层命中必须各带各的 policy_source，否则审计里四层和一层没有区别。
func TestResolverPolicySourcePerLayer(t *testing.T) {
	store := NewLayerStore()
	if err := store.PutTenant("t-1", &PolicySet{DenyTools: []string{"write_file"}}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := store.PutRole("probe-agent", &PolicySet{DenyTools: []string{"list_dir"}}); err != nil {
		t.Fatalf("role: %v", err)
	}
	if err := store.PutTask("task-1", &PolicySet{DenyTools: []string{"read_file"}}); err != nil {
		t.Fatalf("task: %v", err)
	}
	r := NewResolver(nil)
	r.SetLayerStore(store)
	root := t.TempDir()

	cases := []struct {
		name   string
		req    Request
		layer  Layer
		source string
	}{
		{
			name:   "tenant layer",
			req:    Request{Tool: "write_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, TenantID: "t-1", TaskID: "other-task"},
			layer:  LayerTenant,
			source: "tenant:config",
		},
		{
			name:   "role layer",
			req:    Request{Tool: "list_dir", Stage: "c", Args: ToolArgs{Paths: []string{"."}}, TenantID: "t-none", AgentID: "probe-agent"},
			layer:  LayerRole,
			source: "role:config",
		},
		{
			name:   "task layer",
			req:    Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, TenantID: "t-none", TaskID: "task-1"},
			layer:  LayerTask,
			source: "task:config",
		},
		{
			name:   "system layer",
			req:    Request{Tool: "shell_exec", Stage: "c", TenantID: "t-none"},
			layer:  LayerSystem,
			source: "system:engine",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.WorkspaceRoot = root
			d := r.Decide(context.Background(), req)
			if d.Allowed {
				t.Fatalf("call must be denied: %+v", d)
			}
			if d.Layer != tc.layer {
				t.Fatalf("layer = %s, want %s (%+v)", d.Layer, tc.layer, d)
			}
			if d.PolicySource != tc.source {
				t.Fatalf("policy_source = %q, want %q", d.PolicySource, tc.source)
			}
		})
	}
}

// 缓存键必须包含装配后的策略集：同一 tenant 的两个任务，任务层不同，
// 决策不能互相串味。
func TestResolverCacheKeyedByAssembledLayers(t *testing.T) {
	store := NewLayerStore()
	if err := store.PutTask("task-deny", &PolicySet{DenyTools: []string{"read_file"}}); err != nil {
		t.Fatalf("put task: %v", err)
	}
	r := NewResolver(nil)
	r.SetLayerStore(store)
	root := t.TempDir()

	base := Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: root, TenantID: "t-1"}

	deny := base
	deny.TaskID = "task-deny"
	if d := r.Decide(context.Background(), deny); d.Allowed || d.PolicySource != "task:config" {
		t.Fatalf("task-deny must be denied by task layer: %+v", d)
	}

	allow := base
	allow.TaskID = "task-allow"
	if d := r.Decide(context.Background(), allow); !d.Allowed {
		t.Fatalf("task-allow must be allowed: %+v", d)
	}
}

// 任务层策略变了以后，旧判定必须立刻失效（不是等 TTL 过期）。
func TestResolverTaskPolicyReloadInvalidatesCache(t *testing.T) {
	store := NewLayerStore()
	r := NewResolver(nil)
	r.SetLayerStore(store)
	root := t.TempDir()
	req := Request{Tool: "read_file", Stage: "c", Args: ToolArgs{Paths: []string{"a.go"}}, WorkspaceRoot: root, TenantID: "t-1", TaskID: "task-1"}

	if d := r.Decide(context.Background(), req); !d.Allowed {
		t.Fatalf("baseline must allow: %+v", d)
	}
	if err := store.PutTask("task-1", &PolicySet{DenyTools: []string{"read_file"}}); err != nil {
		t.Fatalf("put task: %v", err)
	}
	r.BumpPolicyVersion()
	if d := r.Decide(context.Background(), req); d.Allowed {
		t.Fatalf("task policy change must take effect immediately: %+v", d)
	}
}
