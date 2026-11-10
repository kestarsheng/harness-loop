package retry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyAllCategories(t *testing.T) {
	cases := []struct {
		name string
		env  ErrorEnvelope
		want Category
	}{
		{"network net error", ErrorEnvelope{Kind: KindNet}, Network},
		{"network 503", ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 503}, Network},
		{"network 429 rate limited", ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 429}, Network},
		{"network grpc unavailable", ErrorEnvelope{Kind: KindGRPC, GRPCStatus: "Unavailable"}, Network},
		{"runtime panic", ErrorEnvelope{Kind: KindPanic}, Runtime},
		{"runtime json", ErrorEnvelope{Kind: KindJSON}, Runtime},
		{"runtime agent", ErrorEnvelope{Kind: KindAgent, Code: "E_INTERNAL"}, Runtime},
		{"system os", ErrorEnvelope{Kind: KindOS}, System},
		{"system auth", ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 401}, System},
		{"quota 429 insufficient", ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 429, Code: "insufficient_quota"}, Quota},
		{"timeout 504", ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 504}, Timeout},
		{"timeout grpc deadline", ErrorEnvelope{Kind: KindGRPC, GRPCStatus: "DeadlineExceeded"}, Timeout},
		{"no_finish agent code", ErrorEnvelope{Kind: KindAgent, Code: "E_STAGE_NOT_FINISHED"}, NoFinish},
		{"invalid tool empty", ErrorEnvelope{Kind: KindTool, EmptyResult: true}, InvalidToolResult},
		{"invalid tool error code", ErrorEnvelope{Kind: KindTool, Code: "E_TOOL_FAILED"}, InvalidToolResult},
		{"verification error", ErrorEnvelope{Kind: KindVerifier}, VerificationError},
		{"content filter", ErrorEnvelope{Kind: KindFinish, FinishReason: "content_filter"}, ContentFilter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.env)
			if got.Category != c.want {
				t.Fatalf("category = %s, want %s (reason=%s)", got.Category, c.want, got.Reason)
			}
			if got.Fallback {
				t.Fatalf("exact envelope must not be marked fallback")
			}
		})
	}
}

func TestClassifyNeverReturnsUnknown(t *testing.T) {
	envelopes := []ErrorEnvelope{
		{},
		{Kind: "weird"},
		{Kind: KindHTTP, HTTPStatus: 418},
		{Kind: KindGRPC, GRPCStatus: "Weird"},
		{Kind: KindFinish, FinishReason: "something"},
	}
	for _, env := range envelopes {
		got := Classify(env)
		if !got.Category.Valid() {
			t.Fatalf("envelope %+v produced invalid category %q", env, got.Category)
		}
	}
	fallback := Classify(ErrorEnvelope{Kind: "weird"})
	if !fallback.Fallback || fallback.Category != System {
		t.Fatalf("unknown kind must fall back to system: %+v", fallback)
	}
}

func TestRetryableFlags(t *testing.T) {
	if Classify(ErrorEnvelope{Kind: KindFinish, FinishReason: "content_filter"}).Retryable {
		t.Fatal("content_filter must not be retryable")
	}
	if Classify(ErrorEnvelope{Kind: KindHTTP, HTTPStatus: 429, Code: "insufficient_quota"}).Retryable {
		t.Fatal("quota must not be retryable")
	}
	if !Classify(ErrorEnvelope{Kind: KindNet}).Retryable {
		t.Fatal("network must be retryable")
	}
}

func TestDefaultPolicyMatchesDesign(t *testing.T) {
	p := DefaultPolicy()
	want := map[Category]int{
		Network: 3, Runtime: 3, System: 3, Timeout: 2, NoFinish: 2,
		InvalidToolResult: 2, VerificationError: 2, Quota: 1, ContentFilter: 0,
	}
	for cat, n := range want {
		if got := p.MaxRetriesFor(cat); got != n {
			t.Fatalf("%s = %d, want %d", cat, got, n)
		}
	}
	if p.AcceptanceMaxFails != 5 || p.SilentThresholdSecs != 120 || p.QuotaHardFailSecs != 30 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
}

func TestLoadPolicyFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.yaml")
	body := `
version: 1
max_retries:
  network: 5
  no_finish: 3
  bogus_category: 9
backoff_base_ms:
  network: 2000
backoff_factor: 3
max_backoff_ms: 10000
jitter_ratio: 0
acceptance_max_fails: 2
silent_threshold_seconds: 60
quota_hard_fail_seconds: 5
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.MaxRetriesFor(Network) != 5 || p.MaxRetriesFor(NoFinish) != 3 {
		t.Fatalf("overrides not applied: %+v", p.MaxRetries)
	}
	if p.BackoffBase[Network] != 2*time.Second || p.BackoffFactor != 3 {
		t.Fatalf("backoff overrides not applied: %+v", p)
	}
	if p.AcceptanceMaxFails != 2 || p.SilentThresholdSecs != 60 || p.QuotaHardFailSecs != 5 {
		t.Fatalf("ints not applied: %+v", p)
	}
	if _, ok := p.MaxRetries[Category("bogus_category")]; ok {
		t.Fatal("unknown category must be ignored")
	}

	if _, err := LoadPolicy(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
	if _, err := LoadPolicy(""); err != nil {
		t.Fatalf("empty path must return defaults: %v", err)
	}
}

func TestBackoffSequence(t *testing.T) {
	p := DefaultPolicy()
	got := []time.Duration{
		p.BackoffFor(Network, 1, FixedJitter),
		p.BackoffFor(Network, 2, FixedJitter),
		p.BackoffFor(Network, 3, FixedJitter),
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if p.BackoffFor(Network, 10, FixedJitter) != 30*time.Second {
		t.Fatalf("backoff must cap at max: %v", p.BackoffFor(Network, 10, FixedJitter))
	}
	if p.BackoffFor(Quota, 1, FixedJitter) != 0 || p.BackoffFor(Timeout, 1, FixedJitter) != 0 {
		t.Fatal("quota and timeout must not back off")
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	p := DefaultPolicy()
	for i := 0; i < 50; i++ {
		d := p.BackoffFor(Network, 1, RandomJitter)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("jitter out of ±20%%: %v", d)
		}
	}
}

func TestNoFinishExhaustsAtFourthAttempt(t *testing.T) {
	p := DefaultPolicy()
	p.MaxRetries[NoFinish] = 3
	b := NewBudget(p)

	for i := 1; i <= 3; i++ {
		d := b.RecordError(NoFinish, FixedJitter)
		if !d.Allowed || d.Terminal {
			t.Fatalf("attempt %d should be allowed: %+v", i, d)
		}
		if d.RetryIndex != i {
			t.Fatalf("retry index = %d, want %d", d.RetryIndex, i)
		}
	}
	d := b.RecordError(NoFinish, FixedJitter)
	if d.Allowed || !d.Terminal {
		t.Fatalf("fourth attempt must fail: %+v", d)
	}
	if d.Reason != ErrRetriesExhausted.Error() {
		t.Fatalf("reason = %s", d.Reason)
	}
}

func TestCountersAreIsolated(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	b.RecordAcceptanceFailure()
	b.RecordAcceptanceFailure()
	d := b.RecordError(Network, FixedJitter)
	if !d.Allowed {
		t.Fatalf("network retry must be allowed: %+v", d)
	}
	c := b.Counters()
	if c.AcceptanceFailCount != 2 || c.RetryCount != 1 {
		t.Fatalf("counters mixed: %+v", c)
	}
}

func TestAcceptanceFailUpperBound(t *testing.T) {
	p := DefaultPolicy()
	p.AcceptanceMaxFails = 3
	b := NewBudget(p)
	if d := b.RecordAcceptanceFailure(); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := b.RecordAcceptanceFailure(); !d.Allowed {
		t.Fatalf("second: %+v", d)
	}
	d := b.RecordAcceptanceFailure()
	if d.Allowed || !d.Terminal {
		t.Fatalf("third must exhaust: %+v", d)
	}
}

func TestQuotaFailsFast(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	start := time.Now()
	d := b.RecordError(Quota, RandomJitter)
	if d.Allowed || !d.Terminal {
		t.Fatalf("quota must fail fast: %+v", d)
	}
	if d.Backoff != 0 {
		t.Fatalf("quota must not back off: %v", d.Backoff)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("quota termination exceeded 30s")
	}
}

func TestStageEnterResetsRetryOnly(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	b.RecordError(Network, FixedJitter)
	b.RecordAcceptanceFailure()
	b.OnStageEnter()
	c := b.Counters()
	if c.RetryCount != 0 {
		t.Fatalf("retry count must reset: %+v", c)
	}
	if c.AcceptanceFailCount != 1 {
		t.Fatalf("acceptance failures must be kept: %+v", c)
	}
}

func TestRemainingRetries(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	if got := b.RemainingRetries(Network); got != 3 {
		t.Fatalf("remaining = %d", got)
	}
	b.RecordError(Network, FixedJitter)
	if got := b.RemainingRetries(Network); got != 2 {
		t.Fatalf("remaining = %d", got)
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]FinishReason{
		"stop":           FinishStop,
		"length":         FinishLength,
		"tool_calls":     FinishToolCalls,
		"function_call":  FinishToolCalls,
		"content_filter": FinishContentFilter,
		"":               FinishOther,
		"weird":          FinishOther,
	}
	for raw, want := range cases {
		if got := NormalizeFinishReason(raw); got != want {
			t.Fatalf("%q -> %s, want %s", raw, got, want)
		}
	}
}

func TestSuggestCategoryFromFinishReason(t *testing.T) {
	if got := SuggestCategory(FinishStop, false); got != NoFinish {
		t.Fatalf("stop without finish call -> %s", got)
	}
	if got := SuggestCategory(FinishStop, true); got != "" {
		t.Fatalf("stop with finish call must not be an error: %s", got)
	}
	if got := SuggestCategory(FinishLength, false); got != Runtime {
		t.Fatalf("length -> %s", got)
	}
	if got := SuggestCategory(FinishContentFilter, true); got != ContentFilter {
		t.Fatalf("content filter -> %s", got)
	}
	if got := SuggestCategory(FinishToolCalls, true); got != "" {
		t.Fatalf("tool_calls must continue loop: %s", got)
	}
	if got := SuggestCategory(FinishOther, false); got != System {
		t.Fatalf("other -> %s", got)
	}
	if FinishReasonRetryable(FinishContentFilter) {
		t.Fatal("content filter must not be retryable")
	}
}

func TestErrorEventPayload(t *testing.T) {
	e := ErrorEvent{
		TraceID:              "trace-1",
		TaskID:               "task-1",
		Stage:                "c",
		AttemptID:            "task-1-c-2",
		Category:             Network,
		Retryable:            true,
		RetryIndex:           1,
		MaxRetries:           3,
		OriginalError:        "connection reset",
		OriginalFinishReason: "stop",
	}
	p := e.Payload()
	required := []string{
		"trace_id", "task_id", "stage", "attempt_id", "category",
		"retryable", "retry_index", "max_retries", "classification",
		"original_error", "original_finish_reason",
	}
	for _, key := range required {
		if _, ok := p[key]; !ok {
			t.Fatalf("payload missing %s: %v", key, p)
		}
	}
	if p["classification"] != "exact" {
		t.Fatalf("classification = %v", p["classification"])
	}
	e.Fallback = true
	if e.Payload()["classification"] != "fallback" {
		t.Fatal("fallback classification must be visible")
	}
}

func TestBudgetRestoreAndPolicyAccessor(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	b.Restore(Counters{RetryCount: 2, AcceptanceFailCount: 1})
	c := b.Counters()
	if c.RetryCount != 2 || c.AcceptanceFailCount != 1 {
		t.Fatalf("restore failed: %+v", c)
	}
	if b.Policy().AcceptanceMaxFails != 5 {
		t.Fatal("policy accessor returned wrong policy")
	}
}

func TestExclusionReason(t *testing.T) {
	p := DefaultPolicy()
	if p.ExclusionReason(ContentFilter) == "" || p.ExclusionReason(Quota) == "" {
		t.Fatal("terminal categories must explain exclusion")
	}
	p.MaxRetries[Network] = 0
	if p.ExclusionReason(Network) == "" {
		t.Fatal("zero-retry category must explain exclusion")
	}
	if p.ExclusionReason(Network) != "" && p.MaxRetries[Network] != 0 {
		t.Fatal("retryable category must not have exclusion reason")
	}
	if should := ShouldBackoff(Quota); should {
		t.Fatal("quota must not back off")
	}
	if should := ShouldBackoff(Network); !should {
		t.Fatal("network must back off")
	}
}

func TestRemainingRetriesNeverNegative(t *testing.T) {
	b := NewBudget(DefaultPolicy())
	if got := b.RemainingRetries(ContentFilter); got != 0 {
		t.Fatalf("content filter remaining = %d", got)
	}
}
