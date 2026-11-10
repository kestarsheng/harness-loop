package checks

import (
	"context"
	"testing"

	"agent-platform/pkg/harnessloop/retry"
)

type negativePanicChecker struct{}

func (p *negativePanicChecker) ID() string             { return "negative.panic" }
func (p *negativePanicChecker) Supports(t string) bool { return t == "negative_panic" }
func (p *negativePanicChecker) Probe(ctx context.Context, req Request) (bool, string) {
	return true, ""
}
func (p *negativePanicChecker) Run(ctx context.Context, req Request) (Result, error) {
	panic("checker exploded")
}

func TestNegativeCheckerPanicIsError(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&negativePanicChecker{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "negative_panic"})
	if res.Status != StatusError || res.Err != CodeCheckPanic {
		t.Fatalf("panic must map to error/verification failure: %+v", res)
	}

	// Verification errors are retryable at most twice, then blocked.
	budget := retry.NewBudget(retry.DefaultPolicy())
	for i := 1; i <= 2; i++ {
		d := budget.RecordError(retry.VerificationError, retry.FixedJitter)
		if !d.Allowed || d.Terminal {
			t.Fatalf("verification error #%d should be retryable: %+v", i, d)
		}
	}
	d := budget.RecordError(retry.VerificationError, retry.FixedJitter)
	if d.Allowed || !d.Terminal {
		t.Fatalf("third verification error must terminate: %+v", d)
	}
}

func TestNegativeMissingCommandIsUnverified(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "tests_pass", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified || res.Err != CodeProbeFailed {
		t.Fatalf("missing go.mod must be unverified, never pass: %+v", res)
	}

	res = r.Run(context.Background(), Request{CriterionID: "c-2", Type: "lint_pass", WorkDir: t.TempDir()})
	if res.Status != StatusUnverified {
		t.Fatalf("missing lint tool must be unverified: %+v", res)
	}
}

func TestNegativeUnknownCriterionIsUnverified(t *testing.T) {
	r := NewRegistry()
	res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "made_up_check"})
	if res.Status != StatusUnverified || res.Err != CodeUnknownCheck {
		t.Fatalf("unknown checker must be unverified: %+v", res)
	}
}

func TestNegativeFailingCheckNeverPasses(t *testing.T) {
	dir := writeModule(t)
	r := NewRegistry()
	res := r.Run(context.Background(), Request{
		CriterionID: "c-1", Type: "tests_pass", Expr: "go test -run TestBroken ./...", WorkDir: dir,
	})
	if res.Status != StatusFail {
		t.Fatalf("failing test must be fail, got %+v", res)
	}
	if len(res.Evidence) == 0 {
		t.Fatal("failure must include evidence")
	}
}

func TestNegativePathEscapeInChecker(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry()
	for _, expr := range []string{"../secret", `..\secret`, "/abs/path", "%2e%2e%2fsecret"} {
		res := r.Run(context.Background(), Request{CriterionID: "c-1", Type: "artifact_exists", Expr: expr, WorkDir: dir})
		if res.Status != StatusError {
			t.Fatalf("%q must be rejected, got %+v", expr, res)
		}
	}
}
