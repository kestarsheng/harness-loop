package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluateDefaultsAndRules(t *testing.T) {
	set, err := parseFlagSet([]byte(`
version: 1
flags:
  - name: harness.loop.fsm.enabled
    default: false
    rules:
      - tenants: [tenant-a]
      - projects: [proj-canary]
        percentage: 100
      - users: [alice]
        enabled: true
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(set.Flags()) != 1 {
		t.Fatalf("flags = %v", set.Flags())
	}

	cases := []struct {
		name string
		ctx  FlagContext
		want bool
	}{
		{"tenant rule", FlagContext{TenantID: "tenant-a"}, true},
		{"tenant mismatch", FlagContext{TenantID: "tenant-b"}, false},
		{"project rule", FlagContext{TenantID: "tenant-b", ProjectID: "proj-canary"}, true},
		{"user rule", FlagContext{UserID: "alice"}, true},
		{"nothing matches", FlagContext{TenantID: "x", ProjectID: "y", UserID: "z"}, false},
	}
	for _, c := range cases {
		if got := set.EvaluateFSM(c.ctx); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestEvaluatePercentageIsStableAndBounded(t *testing.T) {
	set, err := parseFlagSet([]byte(`
version: 1
flags:
  - name: harness.loop.fsm.enabled
    default: false
    rules:
      - percentage: 50
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	on := 0
	for i := 0; i < 500; i++ {
		ctx := FlagContext{TenantID: "tenant-" + string(rune('a'+i%26)), UserID: string(rune('0' + i%10))}
		first := set.EvaluateFSM(ctx)
		second := set.EvaluateFSM(ctx)
		if first != second {
			t.Fatalf("evaluation is not deterministic for %+v", ctx)
		}
		if first {
			on++
		}
	}
	if on == 0 || on == 500 {
		t.Fatalf("percentage bucketing looks broken: on=%d", on)
	}
}

func TestEvaluateUnknownFlag(t *testing.T) {
	set, err := parseFlagSet([]byte("version: 1\nflags:\n  - name: other\n    default: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if set.EvaluateFSM(FlagContext{TenantID: "t"}) {
		t.Fatal("unknown flag must evaluate to false")
	}
	var nilSet *FlagSet
	if nilSet.EvaluateFSM(FlagContext{TenantID: "t"}) {
		t.Fatal("nil flag set must evaluate to false")
	}
	if _, ok := nilSet.Get("x"); ok {
		t.Fatal("nil flag set must not resolve flags")
	}
}

func TestParseRejectsInvalidFlags(t *testing.T) {
	cases := []string{
		"flags:\n  - name: ''\n    default: true\n",
		"flags:\n  - name: a\n    default: true\n  - name: a\n    default: false\n",
		"flags:\n  - name: a\n    rules:\n      - percentage: 150\n",
	}
	for _, body := range cases {
		if _, err := parseFlagSet([]byte(body)); err == nil {
			t.Fatalf("invalid flag set accepted: %s", body)
		}
	}
}

func TestLoadFlagSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "features.yaml")
	if err := os.WriteFile(path, []byte("version: 2\nflags:\n  - name: harness.loop.fsm.enabled\n    default: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := LoadFlagSet(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if set.Version != 2 || !set.EvaluateFSM(FlagContext{}) {
		t.Fatalf("unexpected flag set: %+v", set.Flags())
	}
	if _, err := LoadFlagSet(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
	set, err = LoadFlagSet("")
	if err != nil || set != nil {
		t.Fatalf("empty path must return nil set: %v %v", set, err)
	}
}

func TestDefaultFlagIsOff(t *testing.T) {
	if Default().Harness.Enabled {
		t.Fatal("harness loop must be disabled by default (DFX-C-02)")
	}
}
