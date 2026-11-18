// Package audit leaves a durable trace for tool calls that the MCP layer
// refuses on its own (path escape, missing workspace root). Policy-level
// denials are audited by pkg/harnessloop/policy; this package covers the
// enforcement that lives inside the tool executor and therefore never reaches
// the policy engine.
package audit

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one enforcement decision that must stay readable after the fact.
type Record struct {
	TS            string `json:"ts"`
	Tool          string `json:"tool"`
	Decision      string `json:"decision"`
	Code          string `json:"code"`
	Path          string `json:"path,omitempty"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Decision values.
const (
	DecisionDeny = "deny"
)

var (
	mu      sync.Mutex
	logPath string
	records []Record
)

// SetFile points the audit trail at a JSONL file. An empty path keeps the
// trail in memory only; the structured log line is always emitted.
func SetFile(path string) {
	mu.Lock()
	defer mu.Unlock()
	logPath = path
}

// Deny records a refused tool call and returns the record.
func Deny(tool, code, path, root, reason string) Record {
	rec := Record{
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Tool:          tool,
		Decision:      DecisionDeny,
		Code:          code,
		Path:          path,
		WorkspaceRoot: root,
		Reason:        reason,
	}
	record(rec)
	return rec
}

func record(rec Record) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	log.Printf("[audit] %s", line)

	mu.Lock()
	defer mu.Unlock()
	records = append(records, rec)
	if logPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		log.Printf("[audit] cannot create audit dir: %v", err)
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[audit] cannot open audit file: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("[audit] cannot write audit file: %v", err)
	}
}

// Records returns the in-memory trail (used by tests and the smoke script).
func Records() []Record {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Record, len(records))
	copy(out, records)
	return out
}

// Reset drops the in-memory trail. Tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	records = nil
}
