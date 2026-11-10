package policy

import "sync"

const (
	TagRead    = "read"
	TagWrite   = "write"
	TagExec    = "exec"
	TagNetwork = "network"
)

var builtinTools = map[string][]string{
	"read_file":   {TagRead},
	"list_dir":    {TagRead},
	"grep":        {TagRead},
	"write_file":  {TagWrite},
	"edit_file":   {TagWrite},
	"apply_patch": {TagWrite},
	"mkdir":       {TagWrite},
	"delete":      {TagWrite},
	"move":        {TagWrite},
	"run_command": {TagExec},
	"go_test":     {TagExec},
	"go_build":    {TagExec},
	"http_fetch":  {TagNetwork},
	"web_search":  {TagNetwork},
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string][]string
}

func NewRegistry() *Registry {
	r := &Registry{tools: make(map[string][]string, len(builtinTools))}
	for name, tags := range builtinTools {
		r.tools[name] = tags
	}
	return r
}

func (r *Registry) Register(tool string, tags []string) error {
	if tool == "" {
		return errInvalidTool
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool] = append([]string(nil), tags...)
	return nil
}

func (r *Registry) TagsFor(tool string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tags, ok := r.tools[tool]
	if !ok {
		return nil, false
	}
	return append([]string(nil), tags...), true
}

func (r *Registry) Known(tool string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[tool]
	return ok
}
