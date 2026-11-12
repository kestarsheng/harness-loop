package config

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const FlagFSMEnabled = "harness.loop.fsm.enabled"

// T-P1-31 feature switches. All three default to false: when a flag is absent
// or disabled the runtime keeps the pre-existing single-agent, static-model and
// policy.Engine paths.
const (
	FlagOrchestratorEnabled   = "harness.loop.orchestrator.enabled"
	FlagModelStrategyEnabled  = "harness.loop.model.strategy.enabled"
	FlagPolicyResolverEnabled = "harness.loop.policy.resolver.enabled"
)

type FlagContext struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	TaskType  string `json:"task_type"`
	UserID    string `json:"user_id"`
}

type FlagRule struct {
	Tenants    []string `yaml:"tenants"`
	Projects   []string `yaml:"projects"`
	TaskTypes  []string `yaml:"task_types"`
	Users      []string `yaml:"users"`
	Percentage int      `yaml:"percentage"`
	Enabled    *bool    `yaml:"enabled"`
}

type Flag struct {
	Name    string     `yaml:"name"`
	Default bool       `yaml:"default"`
	Rules   []FlagRule `yaml:"rules"`
}

type flagFile struct {
	Version int    `yaml:"version"`
	Flags   []Flag `yaml:"flags"`
}

type FlagSet struct {
	Version int
	flags   map[string]Flag
	order   []string
}

func LoadFlagSet(path string) (*FlagSet, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseFlagSet(raw)
}

func parseFlagSet(raw []byte) (*FlagSet, error) {
	var f flagFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	set := &FlagSet{Version: f.Version, flags: make(map[string]Flag, len(f.Flags))}
	if set.Version <= 0 {
		set.Version = 1
	}
	for _, flag := range f.Flags {
		if strings.TrimSpace(flag.Name) == "" {
			return nil, fmt.Errorf("featureflag: flag name is required")
		}
		if _, exists := set.flags[flag.Name]; exists {
			return nil, fmt.Errorf("featureflag: duplicate flag %q", flag.Name)
		}
		for i, rule := range flag.Rules {
			if rule.Percentage < 0 || rule.Percentage > 100 {
				return nil, fmt.Errorf("featureflag: %s rule %d percentage must be 0..100", flag.Name, i)
			}
		}
		set.flags[flag.Name] = flag
		set.order = append(set.order, flag.Name)
	}
	return set, nil
}

func (s *FlagSet) Flags() []string {
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

func (s *FlagSet) Get(name string) (Flag, bool) {
	if s == nil {
		return Flag{}, false
	}
	f, ok := s.flags[name]
	return f, ok
}

func (s *FlagSet) Evaluate(name string, ctx FlagContext) bool {
	if s == nil {
		return false
	}
	flag, ok := s.flags[name]
	if !ok {
		return false
	}
	for i, rule := range flag.Rules {
		if !matchesList(rule.Tenants, ctx.TenantID) {
			continue
		}
		if !matchesList(rule.Projects, ctx.ProjectID) {
			continue
		}
		if !matchesList(rule.TaskTypes, ctx.TaskType) {
			continue
		}
		if !matchesList(rule.Users, ctx.UserID) {
			continue
		}
		if rule.Percentage > 0 {
			if bucket(name, i, ctx) >= rule.Percentage {
				continue
			}
		}
		if rule.Enabled != nil {
			return *rule.Enabled
		}
		return true
	}
	return flag.Default
}

func (s *FlagSet) EvaluateFSM(ctx FlagContext) bool {
	return s.Evaluate(FlagFSMEnabled, ctx)
}

func matchesList(list []string, value string) bool {
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

func bucket(name string, ruleIndex int, ctx FlagContext) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(fmt.Sprintf("%d", ruleIndex)))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(ctx.TenantID))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(ctx.UserID))
	return int(h.Sum32() % 100)
}
