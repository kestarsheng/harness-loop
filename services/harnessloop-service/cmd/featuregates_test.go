package main

import (
	"os"
	"path/filepath"
	"testing"

	"agent-platform/services/harnessloop-service/internal/config"
)

func writeFlags(t *testing.T, body string) *config.FlagSet {
	t.Helper()
	path := filepath.Join(t.TempDir(), "features.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := config.LoadFlagSet(path)
	if err != nil {
		t.Fatalf("load flags: %v", err)
	}
	return set
}

// 仓库里那份 features.yaml 的默认值决定了「零配置上线是不是旧行为」，所以直接
// 拿真文件跑一遍：三条新链路必须默认关闭，fsm 保持默认开启（既有行为）。
func TestShippedFeatureFlagsKeepNewPathsOff(t *testing.T) {
	set, err := config.LoadFlagSet(filepath.Join("..", "..", "..", "pkg", "config", "harness_loop", "features.yaml"))
	if err != nil {
		t.Fatalf("load repo features.yaml: %v", err)
	}

	gates := resolveFeatureGates(set)
	if gates.Orchestrator || gates.ModelStrategy || gates.PolicyLayers {
		t.Fatalf("shipped flags must keep every new path off, got %s", gates.summary())
	}
	if gates.anyOn() {
		t.Fatal("anyOn must agree with the individual bits")
	}
	if !set.EvaluateFSM(config.FlagContext{TenantID: gateTenant}) {
		t.Fatal("fsm flag must stay on by default: it is the current production behaviour")
	}
}

// dev-up --demo 指的就是这份文件：它必须真的把三条链路全打开，否则演示就是假的。
func TestDemoFlagsTurnEveryPathOn(t *testing.T) {
	set, err := config.LoadFlagSet(filepath.Join("..", "..", "..", "scripts", "dev-features.demo.yaml"))
	if err != nil {
		t.Fatalf("load demo flags: %v", err)
	}
	gates := resolveFeatureGates(set)
	if !gates.Orchestrator || !gates.ModelStrategy || !gates.PolicyLayers {
		t.Fatalf("dev-up --demo must light up all three paths, got %s", gates.summary())
	}
}

// 三关全开 → 三个 bit 全亮（新链路）。
func TestResolveFeatureGatesAllOn(t *testing.T) {
	set := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.orchestrator.enabled
    default: true
  - name: harness.loop.model_strategy.enabled
    default: true
  - name: harness.loop.policy_layers.enabled
    default: true
`)
	gates := resolveFeatureGates(set)
	if !gates.Orchestrator || !gates.ModelStrategy || !gates.PolicyLayers {
		t.Fatalf("all three flags are on, got %s", gates.summary())
	}
}

// 越权关掉其中一条不影响另外两条：三个开关彼此独立。
func TestFeatureGatesAreIndependent(t *testing.T) {
	set := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.orchestrator.enabled
    default: true
  - name: harness.loop.model_strategy.enabled
    default: false
  - name: harness.loop.policy_layers.enabled
    default: true
`)
	gates := resolveFeatureGates(set)
	if !gates.Orchestrator || gates.ModelStrategy || !gates.PolicyLayers {
		t.Fatalf("gates must be independent, got %s", gates.summary())
	}
}

// 缺配置不等于打开：文件里没写这条开关时保持旧行为。
func TestMissingFlagKeepsLegacyPath(t *testing.T) {
	set := writeFlags(t, "version: 1\nflags:\n  - name: harness.loop.fsm.enabled\n    default: true\n")
	if gates := resolveFeatureGates(set); gates.anyOn() {
		t.Fatalf("undeclared flags must stay off, got %s", gates.summary())
	}

	var nilSet *config.FlagSet
	if gates := resolveFeatureGates(nilSet); gates.anyOn() {
		t.Fatalf("nil flag set must stay off, got %s", gates.summary())
	}
}

// 租户规则：命中本服务所属租户才在启动时打开。
func TestTenantRuleDrivesStartupGate(t *testing.T) {
	set := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.orchestrator.enabled
    default: false
    rules:
      - tenants: [tenant-a]
        enabled: true
`)
	if gates := resolveFeatureGates(set); gates.Orchestrator {
		t.Fatal("a tenant-a rule must not open the path for the platform tenant")
	}

	on, from := set.EvaluateFirst(config.FlagContext{TenantID: "tenant-a"}, config.FlagNames(config.FlagOrchestratorEnabled))
	if !on || from != config.FlagOrchestratorEnabled {
		t.Fatalf("tenant-a rule must open the path for tenant-a, got on=%v from=%q", on, from)
	}
}

// tenant 规则里显式 enabled: false 能盖掉 default: true（灰度回滚路径）。
func TestTenantRuleCanTurnFlagOff(t *testing.T) {
	set := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.policy_layers.enabled
    default: true
    rules:
      - tenants: [platform]
        enabled: false
`)
	if gates := resolveFeatureGates(set); gates.PolicyLayers {
		t.Fatal("an explicit tenant rule must be able to switch the path off")
	}
}

// 旧名字继续被读到，且规范名优先。
func TestLegacyFlagNamesStillResolve(t *testing.T) {
	legacy := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.model.strategy.enabled
    default: true
  - name: harness.loop.policy.resolver.enabled
    default: true
`)
	gates := resolveFeatureGates(legacy)
	if !gates.ModelStrategy || !gates.PolicyLayers {
		t.Fatalf("legacy names must keep working, got %s", gates.summary())
	}

	both := writeFlags(t, `
version: 1
flags:
  - name: harness.loop.model_strategy.enabled
    default: false
  - name: harness.loop.model.strategy.enabled
    default: true
`)
	if gates := resolveFeatureGates(both); gates.ModelStrategy {
		t.Fatal("canonical name must win over the legacy alias")
	}
}
