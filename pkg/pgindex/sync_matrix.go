package pgindex

import (
	"fmt"
	"sort"
)

type SyncScenario string

const (
	SyncMemory    SyncScenario = "memory"
	SyncCodeIndex SyncScenario = "codeindex"
	SyncKnowledge SyncScenario = "knowledge"
	SyncEvent     SyncScenario = "event"
	SyncSubtask   SyncScenario = "subtask"
	SyncModel     SyncScenario = "model"
	SyncPolicy    SyncScenario = "policy"
)

// SyncRule 是 design 5.4 双写/单向同步矩阵的一行。
type SyncRule struct {
	Scenario        SyncScenario
	Flag            string
	NewWrite        string
	NewRead         string
	LegacyWrite     string
	LegacyRead      string
	Rollback        string
	DegradedWhenOff bool
}

// SyncRoute 是某个场景在给定开关状态下的实际读写路径。
type SyncRoute struct {
	Scenario    SyncScenario
	Flag        string
	Enabled     bool
	WriteTarget string
	ReadPath    string
	Degraded    bool
}

var syncRules = []SyncRule{
	{
		Scenario:    SyncMemory,
		Flag:        "memory.dual_write",
		NewWrite:    "markdown+legacy",
		NewRead:     "markdown",
		LegacyWrite: "markdown",
		LegacyRead:  "markdown",
		Rollback:    "memory.source=legacy",
	},
	{
		Scenario:        SyncCodeIndex,
		Flag:            "codeindex.enabled",
		NewWrite:        "postgres",
		NewRead:         "postgres",
		LegacyWrite:     "none",
		LegacyRead:      "postgres_disabled",
		Rollback:        "close flag, retrieval returns E_INDEX_DISABLED",
		DegradedWhenOff: true,
	},
	{
		Scenario:    SyncKnowledge,
		Flag:        "knowledge.rerank.enabled",
		NewWrite:    "mongodb+qdrant",
		NewRead:     "mongodb+qdrant+rerank",
		LegacyWrite: "mongodb+qdrant",
		LegacyRead:  "mongodb+qdrant",
		Rollback:    "close flag",
	},
	{
		Scenario:    SyncEvent,
		Flag:        "harness.loop.events.persist",
		NewWrite:    "postgres+redis",
		NewRead:     "postgres+sse",
		LegacyWrite: "trace-memory",
		LegacyRead:  "trace-memory",
		Rollback:    "close flag",
	},
	{
		Scenario:    SyncSubtask,
		Flag:        "harness.loop.subtask.enabled",
		NewWrite:    "dag-path",
		NewRead:     "dag-path",
		LegacyWrite: "single-executor",
		LegacyRead:  "single-executor",
		Rollback:    "close flag",
	},
	{
		Scenario:    SyncModel,
		Flag:        "harness.loop.model.strategy",
		NewWrite:    "registry+selection",
		NewRead:     "registry-snapshot",
		LegacyWrite: "static-model",
		LegacyRead:  "static-model",
		Rollback:    "model strategy frozen",
	},
	{
		Scenario:    SyncPolicy,
		Flag:        "harness.loop.policy.enforce",
		NewWrite:    "policy_snapshot",
		NewRead:     "decision-cache",
		LegacyWrite: "policy-engine",
		LegacyRead:  "policy-engine",
		Rollback:    "policy.mode=shadow",
	},
}

// SyncMatrix 返回矩阵副本，按场景名排序。
func SyncMatrix() []SyncRule {
	out := make([]SyncRule, len(syncRules))
	copy(out, syncRules)
	sort.Slice(out, func(i, j int) bool { return out[i].Scenario < out[j].Scenario })
	return out
}

// ResolveSyncRoute 依据开关状态返回读写路径；关闭时回退到旧路径。
func ResolveSyncRoute(scenario SyncScenario, enabled bool) (SyncRoute, error) {
	rule, ok := lookupSyncRule(scenario)
	if !ok {
		return SyncRoute{}, fmt.Errorf("pgindex: unknown sync scenario %q", scenario)
	}
	route := SyncRoute{Scenario: scenario, Flag: rule.Flag, Enabled: enabled}
	if enabled {
		route.WriteTarget = rule.NewWrite
		route.ReadPath = rule.NewRead
		return route, nil
	}
	route.WriteTarget = rule.LegacyWrite
	route.ReadPath = rule.LegacyRead
	route.Degraded = rule.DegradedWhenOff
	return route, nil
}

func lookupSyncRule(scenario SyncScenario) (SyncRule, bool) {
	for _, rule := range syncRules {
		if rule.Scenario == scenario {
			return rule, true
		}
	}
	return SyncRule{}, false
}
