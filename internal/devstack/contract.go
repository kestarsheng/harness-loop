// Package devstack turns scripts/dev-stack.json into a runnable local stack.
//
// The JSON contract is the single source of truth for the 14 dev items
// (4 data containers + 9 services + vite): order, ports, commands, config
// templates and health probes all come from it. scripts/dev-up.sh and
// scripts/dev-down.sh consume the same file, so this package must not hardcode
// a second list of items.
package devstack

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// SupportedVersion is the contract schema version this package understands.
const SupportedVersion = 1

// DefaultContractPath is the canonical contract location, relative to the repo root.
const DefaultContractPath = "scripts/dev-stack.json"

// Kind values allowed by the contract.
const (
	KindContainer = "container"
	KindProcess   = "process"
	KindFrontend  = "frontend"
)

// Health check types allowed by the contract.
const (
	HealthPort = "port"
	HealthHTTP = "http"
	HealthExec = "exec"
)

// Contract mirrors scripts/dev-stack.json.
type Contract struct {
	Version     int      `json:"version"`
	Description string   `json:"description"`
	Tenant      string   `json:"tenant"`
	Defaults    Defaults `json:"defaults"`
	Items       []Item   `json:"items"`
}

// Defaults holds repo-relative runtime directories.
type Defaults struct {
	DevDir  string `json:"dev_dir"`
	BinDir  string `json:"bin_dir"`
	ConfDir string `json:"conf_dir"`
	LogDir  string `json:"log_dir"`
	RunDir  string `json:"run_dir"`
}

// Item is one startable unit of the local stack.
type Item struct {
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	PidName        string            `json:"pid_name"`
	Port           int               `json:"port"`
	HTTPPort       int               `json:"http_port"`
	Command        string            `json:"command"`
	ConfigTemplate string            `json:"config_template"`
	ConfigOutput   string            `json:"config_output"`
	Env            map[string]string `json:"env"`
	Health         Health            `json:"health"`
	Required       bool              `json:"required"`
}

// Health describes how an item is considered ready.
type Health struct {
	Type         string `json:"type"`
	Command      string `json:"command"`
	Address      string `json:"address"`
	URL          string `json:"url"`
	ExpectStatus int    `json:"expect_status"`
}

// PIDFileStem is the base name used for the item's PID file.
func (it Item) PIDFileStem() string {
	if it.PidName != "" {
		return it.PidName
	}
	return it.Name
}

// IsProcess reports whether the item is a process we launch ourselves
// (as opposed to a container managed by docker).
func (it Item) IsProcess() bool {
	return it.Kind == KindProcess || it.Kind == KindFrontend
}

// LoadContract reads and validates the JSON contract at path.
func LoadContract(path string) (*Contract, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("devstack: read contract %s: %w", path, err)
	}
	var c Contract
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("devstack: parse contract %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("devstack: invalid contract %s: %w", path, err)
	}
	return &c, nil
}

// Validate enforces the invariants the runtime relies on.
func (c *Contract) Validate() error {
	if c == nil {
		return fmt.Errorf("contract is nil")
	}
	if c.Version != SupportedVersion {
		return fmt.Errorf("unsupported version %d (want %d)", c.Version, SupportedVersion)
	}
	if len(c.Items) == 0 {
		return fmt.Errorf("items is empty")
	}
	names := make(map[string]bool, len(c.Items))
	ports := make(map[int]string, len(c.Items))
	stems := make(map[string]string, len(c.Items))
	for i, it := range c.Items {
		label := fmt.Sprintf("items[%d] %q", i, it.Name)
		if strings.TrimSpace(it.Name) == "" {
			return fmt.Errorf("%s: name is required", label)
		}
		if names[it.Name] {
			return fmt.Errorf("%s: duplicate name", label)
		}
		names[it.Name] = true
		switch it.Kind {
		case KindContainer, KindProcess, KindFrontend:
		default:
			return fmt.Errorf("%s: unknown kind %q", label, it.Kind)
		}
		if strings.TrimSpace(it.Command) == "" {
			return fmt.Errorf("%s: command is required", label)
		}
		if err := validPort(it.Port, true, label+" port"); err != nil {
			return err
		}
		if err := validPort(it.HTTPPort, false, label+" http_port"); err != nil {
			return err
		}
		if other, ok := ports[it.Port]; ok {
			return fmt.Errorf("%s: port %d already used by %q", label, it.Port, other)
		}
		ports[it.Port] = it.Name
		stem := it.PIDFileStem()
		if other, ok := stems[stem]; ok {
			return fmt.Errorf("%s: pid file %q already used by %q", label, stem, other)
		}
		stems[stem] = it.Name
		if (it.ConfigTemplate == "") != (it.ConfigOutput == "") {
			return fmt.Errorf("%s: config_template and config_output must both be set or both empty", label)
		}
		if err := it.Health.validate(label); err != nil {
			return err
		}
	}
	return nil
}

func (h Health) validate(label string) error {
	switch h.Type {
	case HealthPort:
		if h.Address == "" {
			return fmt.Errorf("%s: health.address is required for port probes", label)
		}
	case HealthHTTP:
		if h.URL == "" {
			return fmt.Errorf("%s: health.url is required for http probes", label)
		}
		u, err := url.Parse(h.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s: invalid health.url %q", label, h.URL)
		}
	case HealthExec:
		if strings.TrimSpace(h.Command) == "" {
			return fmt.Errorf("%s: health.command is required for exec probes", label)
		}
	default:
		return fmt.Errorf("%s: unknown health type %q", label, h.Type)
	}
	return nil
}

func validPort(port int, required bool, label string) error {
	if port == 0 && !required {
		return nil
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s: %d is out of range", label, port)
	}
	return nil
}

// Item looks up an item by name.
func (c *Contract) Item(name string) (Item, bool) {
	for _, it := range c.Items {
		if it.Name == name {
			return it, true
		}
	}
	return Item{}, false
}

// HarnessLoop returns the item whose health is the single-stack probe
// (harnessloop-service on 127.0.0.1:50013).
func (c *Contract) HarnessLoop() (Item, bool) {
	return c.Item("harnessloop")
}

// Names returns item names in contract order.
func (c *Contract) Names() []string {
	out := make([]string, 0, len(c.Items))
	for _, it := range c.Items {
		out = append(out, it.Name)
	}
	return out
}

// Dirs is the resolved runtime directory set.
type Dirs struct {
	Root string
	Dev  string
	Bin  string
	Conf string
	Logs string
	Run  string
}

// Dirs resolves contract defaults against root. Missing defaults fall back to
// the documented .harness/dev layout.
func (c *Contract) Dirs(root string) Dirs {
	d := Dirs{
		Root: root,
		Dev:  filepath.Join(root, ".harness", "dev"),
	}
	if c != nil {
		if c.Defaults.DevDir != "" {
			d.Dev = absJoin(root, c.Defaults.DevDir)
		}
	}
	d.Bin = filepath.Join(d.Dev, "bin")
	d.Conf = filepath.Join(d.Dev, "conf")
	d.Logs = filepath.Join(d.Dev, "logs")
	d.Run = filepath.Join(d.Dev, "run")
	if c != nil {
		if c.Defaults.BinDir != "" {
			d.Bin = absJoin(root, c.Defaults.BinDir)
		}
		if c.Defaults.ConfDir != "" {
			d.Conf = absJoin(root, c.Defaults.ConfDir)
		}
		if c.Defaults.LogDir != "" {
			d.Logs = absJoin(root, c.Defaults.LogDir)
		}
		if c.Defaults.RunDir != "" {
			d.Run = absJoin(root, c.Defaults.RunDir)
		}
	}
	return d
}

func absJoin(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
