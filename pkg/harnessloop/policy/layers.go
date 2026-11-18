package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// LayerSetSource 说明某一层策略集是从哪儿来的。
//
// 审计里的 policy_source 就是这份来源标签：出了问题要能回答「这条决策到底是
// 哪一层、哪份配置判的」，而不是所有行都写着 layer=system。
type LayerSetSource string

const (
	// LayerSourceNone 表示该层没有策略集，解析时直接跳过。
	LayerSourceNone LayerSetSource = "none"
	// LayerSourceConfig 表示来自 policy_layers.yaml（部署配置）。
	LayerSourceConfig LayerSetSource = "config"
	// LayerSourceSnapshot 表示来自 hlp_task.task_runs.policy_snapshot（存储）。
	LayerSourceSnapshot LayerSetSource = "snapshot"
	// LayerSourceInline 表示调用方直接把策略集塞进了 Request（契约路径）。
	LayerSourceInline LayerSetSource = "inline"
)

// LayerSets 是一次四层解析用到的三个可配置层（system 层由 Engine 承担）。
type LayerSets struct {
	Tenant *PolicySet
	Role   *PolicySet
	Task   *PolicySet

	TenantSource LayerSetSource
	RoleSource   LayerSetSource
	TaskSource   LayerSetSource
}

// Source 返回某层的来源标签。system 层永远来自 engine。
func (s LayerSets) Source(l Layer) LayerSetSource {
	switch l {
	case LayerTenant:
		return s.TenantSource
	case LayerRole:
		return s.RoleSource
	case LayerTask:
		return s.TaskSource
	default:
		return LayerSourceNone
	}
}

// Describe 给日志/事件用：一眼看出哪几层真的命中了配置。
func (s LayerSets) Describe() string {
	parts := []string{
		"tenant=" + string(orNone(s.TenantSource, s.Tenant)),
		"role=" + string(orNone(s.RoleSource, s.Role)),
		"task=" + string(orNone(s.TaskSource, s.Task)),
	}
	return strings.Join(parts, ",")
}

func orNone(src LayerSetSource, set *PolicySet) LayerSetSource {
	if set == nil {
		return LayerSourceNone
	}
	if src == "" {
		return LayerSourceConfig
	}
	return src
}

// LayerStore 是四层策略集的装配点：tenant/role/task 从配置文件读，任务层还可以
// 用任务快照（task_runs.policy_snapshot）覆盖。
//
// 之所以把 store 放在 policy 包而不是 service 包：解析顺序、缓存键、审计来源
// 三者必须看同一份装配结果，分开放一定会漂移。
type LayerStore struct {
	mu          sync.RWMutex
	path        string
	tenants     map[string]*PolicySet
	roles       map[string]*PolicySet
	tasks       map[string]*PolicySet
	taskSources map[string]LayerSetSource
}

func NewLayerStore() *LayerStore {
	return &LayerStore{
		tenants:     make(map[string]*PolicySet),
		roles:       make(map[string]*PolicySet),
		tasks:       make(map[string]*PolicySet),
		taskSources: make(map[string]LayerSetSource),
	}
}

// layerSetFile 是 policy_layers.yaml 的磁盘结构。
//
// 三张 map 各自独立：一个租户配了也算数，角色层空着就是「角色层不参与」，
// 不是「角色层全拒」——空 map 与空策略集在语义上必须分得开。
type layerSetFile struct {
	Version int                    `yaml:"version"`
	Tenants map[string]*PolicySet  `yaml:"tenants"`
	Roles   map[string]*PolicySet  `yaml:"roles"`
	Tasks   map[string]*PolicySet  `yaml:"tasks"`
}

// LoadLayerStore 读取配置文件。path 为空或文件不存在时返回空 store + error，
// 调用方决定是否降级（空 store 等于「只有 system 层」，行为与改动前一致）。
func LoadLayerStore(path string) (*LayerStore, error) {
	store := NewLayerStore()
	store.path = path
	if strings.TrimSpace(path) == "" {
		return store, fmt.Errorf("policy: layer set file path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return store, err
	}
	var file layerSetFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return store, fmt.Errorf("policy: parse layer sets %s: %w", path, err)
	}
	for id, set := range file.Tenants {
		if err := store.put("tenant", id, set); err != nil {
			return store, err
		}
	}
	for id, set := range file.Roles {
		if err := store.put("role", id, set); err != nil {
			return store, err
		}
	}
	for id, set := range file.Tasks {
		if err := store.put("task", id, set); err != nil {
			return store, err
		}
	}
	return store, nil
}

func (s *LayerStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Len 返回三层各自的条目数，装配日志用它证明「配置真的被读进去了」。
func (s *LayerStore) Len() (tenants, roles, tasks int) {
	if s == nil {
		return 0, 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tenants), len(s.roles), len(s.tasks)
}

func (s *LayerStore) PutTenant(id string, set *PolicySet) error { return s.put("tenant", id, set) }
func (s *LayerStore) PutRole(id string, set *PolicySet) error   { return s.put("role", id, set) }
func (s *LayerStore) PutTask(id string, set *PolicySet) error   { return s.put("task", id, set) }

// PutTaskSnapshot 注入来自任务快照的任务层策略集。task 层的来源必须在解析结果里
// 可辨（snapshot 还是 config），否则 policy_source 会把「存储里的任务策略」写成
// 配置来源，排查时分不清改哪儿才生效。
func (s *LayerStore) PutTaskSnapshot(id string, set *PolicySet) error {
	if err := s.put("task", id, set); err != nil {
		return err
	}
	if s == nil || set == nil {
		return nil
	}
	s.mu.Lock()
	s.taskSources[strings.TrimSpace(id)] = LayerSourceSnapshot
	s.mu.Unlock()
	return nil
}

func (s *LayerStore) put(kind, id string, set *PolicySet) error {
	if s == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("policy: empty %s id in layer set file", kind)
	}
	if set == nil {
		return nil
	}
	if err := validatePolicySet(set); err != nil {
		return fmt.Errorf("policy: %s %q: %w", kind, id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "tenant":
		s.tenants[id] = set
	case "role":
		s.roles[id] = set
	case "task":
		s.tasks[id] = set
	}
	return nil
}

func validatePolicySet(set *PolicySet) error {
	for i := range set.Rules {
		switch set.Rules[i].Effect {
		case EffectAllow, EffectDeny, EffectRequireApproval:
		case "":
			return fmt.Errorf("rule %q has no effect", set.Rules[i].ID)
		default:
			return fmt.Errorf("rule %q has unknown effect %q", set.Rules[i].ID, set.Rules[i].Effect)
		}
	}
	return nil
}

func (s *LayerStore) tenant(id string) *PolicySet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tenants[id]
}

func (s *LayerStore) role(id string) *PolicySet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.roles[id]
}

func (s *LayerStore) task(id string) *PolicySet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id]
}

// Resolve 装配一次四层解析需要的三个可配置层。
//
// roleName 为空时回落到 agentID：stage 路径只带得出 entry_agent，而配置是按
// 角色名写的，没有这条回落角色层在真实任务里永远命中不了。
func (s *LayerStore) Resolve(tenantID, roleName, agentID, taskID string) LayerSets {
	if s == nil {
		return LayerSets{TenantSource: LayerSourceNone, RoleSource: LayerSourceNone, TaskSource: LayerSourceNone}
	}
	sets := LayerSets{
		TenantSource: LayerSourceNone,
		RoleSource:   LayerSourceNone,
		TaskSource:   LayerSourceNone,
	}
	if set := s.tenant(strings.TrimSpace(tenantID)); set != nil {
		sets.Tenant = set
		sets.TenantSource = LayerSourceConfig
	}
	roleKey := strings.TrimSpace(roleName)
	if roleKey == "" {
		roleKey = strings.TrimSpace(agentID)
	}
	if set := s.role(roleKey); set != nil {
		sets.Role = set
		sets.RoleSource = LayerSourceConfig
	}
	if set := s.task(strings.TrimSpace(taskID)); set != nil {
		sets.Task = set
		sets.TaskSource = s.taskSource(strings.TrimSpace(taskID))
	}
	return sets
}

func (s *LayerStore) taskSource(id string) LayerSetSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if src, ok := s.taskSources[id]; ok && src != "" {
		return src
	}
	return LayerSourceConfig
}

// TaskPolicyFromSnapshot 从 task_runs.policy_snapshot（jsonb）里取任务级工具
// 策略集。
//
// 快照里没有 tool_policy 键时返回 nil：那是「这个任务不使用任务层」，不能当成
// 「任务层是空策略集」——后者会被解释成配了但没规则，容易和「全拒」混淆。
func TaskPolicyFromSnapshot(raw []byte) *PolicySet {
	if len(raw) == 0 {
		return nil
	}
	var envelope struct {
		ToolPolicy *PolicySet `json:"tool_policy"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	if envelope.ToolPolicy == nil || envelope.ToolPolicy.Empty() {
		return nil
	}
	return envelope.ToolPolicy
}

// SnapshotWithTaskPolicy 把任务级策略集写回 policy_snapshot 的 tool_policy 键，
// 保留快照里其它键（验收标准等）不动。测试与种子脚本用它造任务行。
func SnapshotWithTaskPolicy(raw []byte, set *PolicySet) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("policy: policy_snapshot is not an object: %w", err)
		}
	}
	if set == nil {
		delete(doc, "tool_policy")
	} else {
		encoded, err := json.Marshal(set)
		if err != nil {
			return nil, err
		}
		doc["tool_policy"] = encoded
	}
	return json.Marshal(doc)
}

// Keys 返回某一层的全部 id（升序），只给日志与测试用。
func (s *LayerStore) Keys(kind string) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var src map[string]*PolicySet
	switch kind {
	case "tenant":
		src = s.tenants
	case "role":
		src = s.roles
	case "task":
		src = s.tasks
	default:
		return nil
	}
	out := make([]string, 0, len(src))
	for id := range src {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
