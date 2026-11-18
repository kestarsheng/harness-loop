package config

import (
	"strings"
	"testing"
)

// TestSetLocalOnlyKeepsPort is the T-P2-35 fix for `-local`: it used to hardcode
// 50008, so a local run either stole another service's port or failed to bind
// and exited within seconds.
func TestSetLocalOnlyKeepsPort(t *testing.T) {
	cases := map[string]string{
		":50113":          "127.0.0.1:50113",
		"127.0.0.1:50113": "127.0.0.1:50113",
		"0.0.0.0:50113":   "127.0.0.1:50113",
		"":                "127.0.0.1:50008",
	}
	for in, want := range cases {
		c := Default()
		c.Server.Address = in
		c.SetLocalOnly()
		if c.Server.Address != want {
			t.Fatalf("SetLocalOnly(%q) = %q, want %q", in, c.Server.Address, want)
		}
		if !c.Server.Local {
			t.Fatalf("SetLocalOnly(%q) did not mark the config local", in)
		}
	}
}

// A host without a port cannot be rewritten; it must stay untouched so Validate
// (or the bind) reports it instead of silently turning into a random port.
func TestSetLocalOnlyLeavesUnparseableAddressAlone(t *testing.T) {
	c := Default()
	c.Server.Address = "localhost"
	c.SetLocalOnly()
	if c.Server.Address != "localhost" {
		t.Fatalf("address = %q, want it untouched", c.Server.Address)
	}
}

func envOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// TestLoadLLMEnvAbsentIsNotAnError: no HARNESSLOOP_LLM_* at all is a legal
// configuration (compaction degrades), never a startup failure.
func TestLoadLLMEnvAbsentIsNotAnError(t *testing.T) {
	env, err := LoadLLMEnv(envOf(nil))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if env.Configured() {
		t.Fatal("an empty env must not report a configured summarizer")
	}
}

func TestLoadLLMEnvComplete(t *testing.T) {
	env, err := LoadLLMEnv(envOf(map[string]string{
		"HARNESSLOOP_LLM_BASE_URL": "https://gateway.example/v1",
		"HARNESSLOOP_LLM_API_KEY":  "sk-test",
		"HARNESSLOOP_LLM_MODEL":    "qwen-plus",
	}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !env.Configured() || env.APIKey != "sk-test" || env.Model != "qwen-plus" {
		t.Fatalf("env = %+v", env)
	}
}

// TestLoadLLMEnvNamesTheMissingVariable is the T-P2-36 regression: a partially
// injected trio must produce a message that names what is missing, instead of a
// silent exit(1) with an unreadable stderr line.
func TestLoadLLMEnvNamesTheMissingVariable(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "missing key",
			env: map[string]string{
				"HARNESSLOOP_LLM_BASE_URL": "https://gateway.example/v1",
				"HARNESSLOOP_LLM_MODEL":    "qwen-plus",
			},
			want: "HARNESSLOOP_LLM_API_KEY",
		},
		{
			name: "missing model",
			env: map[string]string{
				"HARNESSLOOP_LLM_BASE_URL": "https://gateway.example/v1",
				"HARNESSLOOP_LLM_API_KEY":  "sk-test",
			},
			want: "HARNESSLOOP_LLM_MODEL",
		},
		{
			name: "key without base url",
			env: map[string]string{
				"HARNESSLOOP_LLM_API_KEY": "sk-test",
			},
			want: "HARNESSLOOP_LLM_BASE_URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadLLMEnv(envOf(tc.env))
			if err == nil {
				t.Fatal("want an error naming the missing variable")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %s", err, tc.want)
			}
		})
	}
}

// Injecting the three variables must not change anything about yaml validation:
// the summarizer is optional, the required fields are still the required ones.
func TestLLMEnvDoesNotAffectValidate(t *testing.T) {
	c := Default()
	c.Postgres.DSN = "postgres://agent:agent@127.0.0.1:55432/agent_platform?sslmode=disable"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	t.Setenv("HARNESSLOOP_LLM_BASE_URL", "https://gateway.example/v1")
	t.Setenv("HARNESSLOOP_LLM_API_KEY", "sk-test")
	t.Setenv("HARNESSLOOP_LLM_MODEL", "qwen-plus")
	if err := c.Validate(); err != nil {
		t.Fatalf("LLM env must not affect validation: %v", err)
	}
}
