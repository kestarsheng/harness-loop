package output

import (
	"strings"
	"testing"
)

func TestRedactMasksSecrets(t *testing.T) {
	cases := []string{
		`api key sk-abcdefghijklmnop1234`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9`,
		`password=supersecret123`,
		`{"api_key":"abcd1234efgh5678"}`,
	}
	for _, c := range cases {
		got := Redact(c)
		if strings.Contains(got, "sk-abcdefghijklmnop1234") ||
			strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") ||
			strings.Contains(got, "supersecret123") ||
			strings.Contains(got, "abcd1234efgh5678") {
			t.Fatalf("secret not redacted: %s", got)
		}
		if !strings.Contains(got, mask) {
			t.Fatalf("mask missing: %s", got)
		}
	}
}

func TestRedactKeepsSafeText(t *testing.T) {
	in := "task=abc status=completed stage=e"
	if got := Redact(in); got != in {
		t.Fatalf("safe text changed: %s", got)
	}
}

func TestRedactAnyMap(t *testing.T) {
	in := map[string]any{
		"api_key": "plain-secret",
		"nested":  map[string]any{"token": "abc", "note": "sk-1234567890abcdef"},
		"list":    []string{"ok", "Bearer abcdefgh"},
		"count":   3,
	}
	out := RedactAny(in).(map[string]any)
	if out["api_key"] != mask {
		t.Fatalf("api_key not masked: %v", out["api_key"])
	}
	nested := out["nested"].(map[string]any)
	if nested["token"] != mask {
		t.Fatalf("token not masked: %v", nested["token"])
	}
	if strings.Contains(nested["note"].(string), "sk-1234567890abcdef") {
		t.Fatalf("note not redacted: %v", nested["note"])
	}
	list := out["list"].([]string)
	if strings.Contains(list[1], "abcdefgh") {
		t.Fatalf("list not redacted: %v", list)
	}
	if out["count"] != 3 {
		t.Fatalf("non-string value changed: %v", out["count"])
	}
}

func TestExitCodeForStatus(t *testing.T) {
	cases := []struct {
		status, reason string
		want           int
	}{
		{"completed", "completed", ExitOK},
		{"failed", "failed", ExitFailed},
		{"aborted", "aborted", ExitAborted},
		{"running", "", ExitFailed},
		{"", "completed", ExitOK},
		{"", "aborted", ExitAborted},
	}
	for _, c := range cases {
		if got := ExitCodeForStatus(c.status, c.reason); got != c.want {
			t.Fatalf("%q/%q -> %d, want %d", c.status, c.reason, got, c.want)
		}
	}
}

func TestFailureEnvelopeRedacts(t *testing.T) {
	env := Failure("E_X", "failed with sk-abcdefgh12345678", map[string]any{"api_key": "secret-value"})
	if env.OK {
		t.Fatal("failure must not be ok")
	}
	if strings.Contains(env.Error.Message, "sk-abcdefgh12345678") {
		t.Fatalf("message not redacted: %s", env.Error.Message)
	}
	if env.Error.Details["api_key"] != mask {
		t.Fatalf("details not redacted: %v", env.Error.Details)
	}
}

func TestSuccessEnvelope(t *testing.T) {
	env := Success(Envelope{TaskID: "t1", Status: "completed"})
	if !env.OK || env.TaskID != "t1" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestRedactKeepsTaskIDs(t *testing.T) {
	for _, id := range []string{"task-abcdef1234567890", "task-1a2b3c4d5e6f", "acc-task-abc123def456"} {
		if got := Redact(id); got != id {
			t.Fatalf("task id %q was redacted to %q", id, got)
		}
	}
	if got := Redact("sk-abcdefgh12345678"); !strings.Contains(got, mask) {
		t.Fatalf("real secret not redacted: %s", got)
	}
}
