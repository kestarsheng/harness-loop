package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

// TestExitCodeForCategory pins the contract a supervisor depends on: different
// startup failures must be distinguishable without reading the log text.
func TestExitCodeForCategory(t *testing.T) {
	cases := map[string]int{
		catPanic:         exitPanic,
		catConfigLoad:    exitConfigLoad,
		catConfigInvalid: exitConfigBad,
		catDatabase:      exitDatabase,
		catMigration:     exitMigration,
		catListen:        exitListen,
		catPolicy:        exitPolicy,
		catSelfCheck:     exitSelfCheck,
	}
	seen := map[int]string{}
	for category, want := range cases {
		if got := exitCodeFor(category); got != want {
			t.Fatalf("exitCodeFor(%s) = %d, want %d", category, got, want)
		}
		if other, dup := seen[want]; dup {
			t.Fatalf("%s and %s share exit code %d", category, other, want)
		}
		seen[want] = category
	}
	if got := exitCodeFor("nonsense"); got != 1 {
		t.Fatalf("unknown category = %d, want 1", got)
	}
}

// TestExitReporterWritesRecord is the "退出原因可查" half: the reason has to
// survive the terminal. Before this, a repo whose stderr was discarded left
// nothing at all behind.
func TestExitReporterWritesRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "last-exit.json")
	reporter := newExitReporter(path)
	reporter.logger = log.New(io.Discard, "", 0)

	code := reporter.Fail(catListen, errors.New("bind: address already in use"))
	if code != exitListen {
		t.Fatalf("code = %d, want %d", code, exitListen)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec exitRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if rec.Code != exitListen || rec.Category != catListen {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Detail == "" {
		t.Fatal("record must keep the reason text")
	}
	if rec.PID == 0 || rec.Time == "" {
		t.Fatalf("record is missing pid/time: %+v", rec)
	}
}

// A read-only or missing state dir must never turn one failure into two.
func TestExitReporterSurvivesUnwritableStateDir(t *testing.T) {
	reporter := newExitReporter(filepath.Join(t.TempDir(), "last-exit.json", "impossible", "x.json"))
	reporter.logger = log.New(io.Discard, "", 0)
	if code := reporter.Fail(catDatabase, errors.New("connection refused")); code != exitDatabase {
		t.Fatalf("code = %d, want %d", code, exitDatabase)
	}
}

func TestExitStatePathHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HARNESSLOOP_STATE_DIR", dir)
	if got, want := exitStatePath(), filepath.Join(dir, "last-exit.json"); got != want {
		t.Fatalf("state path = %q, want %q", got, want)
	}
}
