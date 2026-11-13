package memoryfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	SchemaVersion = 1
	MaxBodyChars  = 4000

	ScopeGlobal  = "global"
	ScopeProject = "project"
)

type Category string

const (
	CategoryUserPreference     Category = "user_preference"
	CategoryLongTermConstraint Category = "long_term_constraint"
	CategoryProjectFact        Category = "project_fact"
	CategoryTaskState          Category = "task_state"
)

func (c Category) Valid() bool {
	switch c {
	case CategoryUserPreference, CategoryLongTermConstraint, CategoryProjectFact, CategoryTaskState:
		return true
	}
	return false
}

var (
	ErrInvalidFrontmatter = errors.New("memoryfile: invalid frontmatter")
	ErrInvalidTopicKey    = errors.New("memoryfile: invalid topic key")
	ErrDuplicateTopic     = errors.New("memoryfile: duplicate topic key")
	ErrBodyTooLarge       = errors.New("memoryfile: topic body exceeds limit")
	ErrInvalidMeta        = errors.New("memoryfile: invalid topic meta")
	ErrOrderMismatch      = errors.New("memoryfile: order does not match topics")
)

var topicKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func ValidTopicKey(key string) bool { return topicKeyPattern.MatchString(key) }

type Meta struct {
	Category    Category
	Pinned      bool
	UpdatedAt   time.Time
	ContentHash string
}

type Topic struct {
	Key   string
	Title string
	Body  string
	Meta  Meta
}

type Document struct {
	SchemaVersion int
	Scope         string
	TenantID      string
	ProjectID     string
	UpdatedAt     time.Time
	OrderVersion  int
	Order         []string
	Topics        []Topic
}

func ComputeContentHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (d *Document) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidFrontmatter, d.SchemaVersion)
	}
	if d.Scope != ScopeGlobal && d.Scope != ScopeProject {
		return fmt.Errorf("%w: scope %q must be global|project", ErrInvalidFrontmatter, d.Scope)
	}
	seen := make(map[string]bool, len(d.Topics))
	for _, t := range d.Topics {
		if !ValidTopicKey(t.Key) {
			return fmt.Errorf("%w: %q", ErrInvalidTopicKey, t.Key)
		}
		if seen[t.Key] {
			return fmt.Errorf("%w: %q", ErrDuplicateTopic, t.Key)
		}
		seen[t.Key] = true
		if !t.Meta.Category.Valid() {
			return fmt.Errorf("%w: topic %q category %q", ErrInvalidMeta, t.Key, t.Meta.Category)
		}
		if t.Meta.UpdatedAt.IsZero() {
			return fmt.Errorf("%w: topic %q updated_at is required", ErrInvalidMeta, t.Key)
		}
		if strings.TrimSpace(t.Title) == "" {
			return fmt.Errorf("%w: topic %q title is required", ErrInvalidMeta, t.Key)
		}
		if len([]rune(t.Body)) > MaxBodyChars {
			return fmt.Errorf("%w: topic %q has %d chars", ErrBodyTooLarge, t.Key, len([]rune(t.Body)))
		}
	}
	if len(d.Order) != len(d.Topics) {
		return fmt.Errorf("%w: order has %d keys, topics %d", ErrOrderMismatch, len(d.Order), len(d.Topics))
	}
	for i, key := range d.Order {
		if !seen[key] {
			return fmt.Errorf("%w: order references unknown topic %q", ErrOrderMismatch, key)
		}
		if a, b := i, keyIndex(d.Topics, key); b != a {
			return fmt.Errorf("%w: order position %d points to topic index %d", ErrOrderMismatch, a, b)
		}
	}
	return nil
}

func keyIndex(topics []Topic, key string) int {
	for i, t := range topics {
		if t.Key == key {
			return i
		}
	}
	return -1
}

func (d *Document) TopicByKey(key string) (Topic, bool) {
	for _, t := range d.Topics {
		if t.Key == key {
			return t, true
		}
	}
	return Topic{}, false
}

type Layout struct {
	HarnessHome   string
	WorkspaceRoot string
}

func NewLayout(harnessHome, workspaceRoot string) (Layout, error) {
	if strings.TrimSpace(harnessHome) == "" {
		return Layout{}, fmt.Errorf("memoryfile: harness home is required")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return Layout{}, fmt.Errorf("memoryfile: workspace root is required")
	}
	return Layout{
		HarnessHome:   filepath.Clean(harnessHome),
		WorkspaceRoot: filepath.Clean(workspaceRoot),
	}, nil
}

func (l Layout) GlobalDir() string {
	return filepath.Join(l.HarnessHome, "memory")
}

func (l Layout) GlobalFile() string {
	return filepath.Join(l.GlobalDir(), "global.md")
}

func (l Layout) GlobalLock() string {
	return filepath.Join(l.GlobalDir(), ".lock")
}

func (l Layout) GlobalBackupDir() string {
	return filepath.Join(l.GlobalDir(), "backup")
}

func (l Layout) ProjectDir() string {
	return filepath.Join(l.WorkspaceRoot, ".harness", "memory")
}

func (l Layout) ProjectFile() string {
	return filepath.Join(l.ProjectDir(), "project.md")
}

func (l Layout) ProjectLock() string {
	return filepath.Join(l.ProjectDir(), ".lock")
}

func (l Layout) ProjectBackupDir() string {
	return filepath.Join(l.ProjectDir(), "backup")
}

func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.GlobalDir(), l.GlobalBackupDir(), l.ProjectDir(), l.ProjectBackupDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("memoryfile: create %s: %w", dir, err)
		}
	}
	return nil
}
