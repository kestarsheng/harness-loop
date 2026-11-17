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
//
// 命名对齐 P2 任务书：模型/权限两项用下划线分段（model_strategy、policy_layers），
// 与 design.md 里的 model.strategy / policy.resolver 拼法不同；旧拼法保留在
// FlagAliases 里，老 flag 文件不会因为改名而静默变成 false。
const (
	FlagOrchestratorEnabled  = "harness.loop.orchestrator.enabled"
	FlagModelStrategyEnabled = "harness.loop.model_strategy.enabled"
	FlagPolicyLayersEnabled  = "harness.loop.policy_layers.enabled"
)

// FlagAliases maps a canonical flag name to the legacy names still accepted when
// reading. Order matters: the canonical name wins when both are present.
var FlagAliases = map[string][]string{
	FlagModelStrategyEnabled: {"harness.loop.model.strategy.enabled"},
	FlagPolicyLayersEnabled:  {"harness.loop.policy.resolver.enabled"},
}

// FlagNames returns the canonical name followed by its legacy aliases. Callers
// pass the result to EvaluateFirst so a renamed flag keeps working.
func FlagNames(name string) []string {
	out := make([]string, 0, 1+len(FlagAliases[name]))
	out = append(out, name)
	out = append(out, FlagAliases[name]...)
	return out
}

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

// EvaluateFirst returns the verdict of the first name that exists in the flag
// file, together with the name it came from ("" when none is declared).
//
// 用途：进程启动时把一条开关解析成一个布尔位（bridge 的 enabled）。三个新链路
// 开关的 enabled 位是「本服务所属租户」的解析结果——默认 platform；租户规则因此
// 在启动时生效。请求级租户差异不在本层解决（bridge 是进程级单例）。
func (s *FlagSet) EvaluateFirst(ctx FlagContext, names []string) (bool, string) {
	if s == nil {
		return false, ""
	}
	for _, name := range names {
		if _, ok := s.Get(name); ok {
			return s.Evaluate(name, ctx), name
		}
	}
	return false, ""
}

// Declared reports whether the flag file declares name (canonical or alias).
func (s *FlagSet) Declared(names []string) bool {
	if s == nil {
		return false
	}
	for _, name := range names {
		if _, ok := s.Get(name); ok {
			return true
		}
	}
	return false
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
