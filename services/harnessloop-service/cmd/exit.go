package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Exit codes are part of the contract with whatever starts this process
// (dev-up.sh, devstack.Ensure, a container supervisor). Before T-P2-35 every
// startup failure ended in log.Fatalf → exit 1 with a human-only stderr line,
// so a wrapper could not tell "port taken" from "bad DSN" from "panic", and an
// operator who captured only stdout saw a *silent* exit(1).
const (
	exitOK         = 0
	exitPanic      = 2  // the Go runtime's own code for an uncaught panic
	exitConfigLoad = 10 // config file missing/unreadable
	exitConfigBad  = 11 // config failed validation
	exitDatabase   = 12 // postgres connect/ping
	exitMigration  = 13 // migrations
	exitListen     = 14 // gRPC bind failed (port conflict is the common case)
	exitPolicy     = 15 // policy/model/retry assets could not be loaded
	exitSelfCheck  = 16 // startup self-check found a fatal wiring error
)

// Failure categories are the stable, greppable half of the exit reason: the log
// line and the state file both carry them.
const (
	catPanic         = "panic"
	catConfigLoad    = "config_load"
	catConfigInvalid = "config_validate"
	catDatabase      = "database"
	catMigration     = "migration"
	catListen        = "listen"
	catPolicy        = "policy"
	catSelfCheck     = "selfcheck"
)

var exitCodeByCategory = map[string]int{
	catPanic:         exitPanic,
	catConfigLoad:    exitConfigLoad,
	catConfigInvalid: exitConfigBad,
	catDatabase:      exitDatabase,
	catMigration:     exitMigration,
	catListen:        exitListen,
	catPolicy:        exitPolicy,
	catSelfCheck:     exitSelfCheck,
}

// exitCodeFor maps a category to its exit code. An unknown category is a
// programming error rather than a fourth kind of failure, so it falls back to 1
// and still reports its own name in the log line.
func exitCodeFor(category string) int {
	if code, ok := exitCodeByCategory[category]; ok {
		return code
	}
	return 1
}

// exitRecord is what lands in last-exit.json. Keeping it on disk (not only in
// stderr) is what makes "退出原因可查" true after the terminal is gone: the
// service may have been started by a supervisor that discards both streams.
type exitRecord struct {
	Code     int    `json:"code"`
	Category string `json:"category"`
	Detail   string `json:"detail,omitempty"`
	PID      int    `json:"pid"`
	Time     string `json:"time"`
}

// exitReporter turns a startup failure into three durable things: a log line on
// both streams (log output is wired to stdout+stderr by main), a category, and
// a stable exit code.
type exitReporter struct {
	logger    *log.Logger
	statePath string
	now       func() time.Time
}

func newExitReporter(statePath string) *exitReporter {
	return &exitReporter{logger: log.Default(), statePath: statePath, now: time.Now}
}

// Fail reports the failure and returns the process exit code.
func (r *exitReporter) Fail(category string, err error) int {
	code := exitCodeFor(category)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if r.logger != nil {
		r.logger.Printf("harnessloop: FATAL category=%s exit=%d reason=%s", category, code, detail)
	}
	r.writeRecord(exitRecord{
		Code:     code,
		Category: category,
		Detail:   detail,
		PID:      os.Getpid(),
		Time:     r.now().Format(time.RFC3339Nano),
	})
	return code
}

// writeRecord is best effort: a read-only state dir must never turn one startup
// failure into two.
func (r *exitReporter) writeRecord(rec exitRecord) {
	if r == nil || r.statePath == "" {
		return
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(r.statePath, append(raw, '\n'), 0o644)
}

// exitStatePath resolves where the last exit reason is written.
// HARNESSLOOP_STATE_DIR is honoured so an isolated stack can keep its own copy.
func exitStatePath() string {
	dir := os.Getenv("HARNESSLOOP_STATE_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "harnessloop")
	}
	return filepath.Join(dir, "last-exit.json")
}
