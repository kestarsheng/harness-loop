package retry

import "testing"

func TestNoFinishNormalization(t *testing.T) {
	cases := map[string]FinishReason{
		"stop":           FinishStop,
		"length":         FinishLength,
		"tool_calls":     FinishToolCalls,
		"function_call":  FinishToolCalls,
		"content_filter": FinishContentFilter,
		"":               FinishOther,
		"whatever":       FinishOther,
	}
	for raw, want := range cases {
		if got := NormalizeFinishReason(raw); got != want {
			t.Fatalf("%q -> %s, want %s", raw, got, want)
		}
	}
}

func TestNoFinishStopWithoutToolCall(t *testing.T) {
	d := DecideFinishAction(FinishStop, false, false)
	if d.Action != ActionRetry || d.Category != NoFinish {
		t.Fatalf("stop without finish must retry as no_finish: %+v", d)
	}
	d = DecideFinishAction(FinishStop, true, false)
	if d.Action != ActionDone {
		t.Fatalf("stop with finish must complete: %+v", d)
	}
}

func TestNoFinishLengthTriggersCompaction(t *testing.T) {
	d := DecideFinishAction(FinishLength, false, false)
	if d.Action != ActionCompact {
		t.Fatalf("length must compact first: %+v", d)
	}
	d = DecideFinishAction(FinishLength, false, true)
	if d.Action != ActionRetry || d.Category != Runtime {
		t.Fatalf("length after compaction must retry: %+v", d)
	}
}

func TestNoFinishContentFilterFailsImmediately(t *testing.T) {
	d := DecideFinishAction(FinishContentFilter, false, false)
	if d.Action != ActionFail || d.Category != ContentFilter {
		t.Fatalf("content_filter must fail: %+v", d)
	}

	b := NewBudget(DefaultPolicy())
	dec := b.RecordError(ContentFilter, FixedJitter)
	if dec.Allowed || !dec.Terminal {
		t.Fatalf("content_filter must not produce a retry: %+v", dec)
	}
	if dec.Backoff != 0 {
		t.Fatalf("content_filter must not back off: %v", dec.Backoff)
	}
	if b.Counters().RetryCount != 0 {
		t.Fatalf("content_filter must not consume retry budget: %+v", b.Counters())
	}
}

func TestNoFinishToolCallsContinue(t *testing.T) {
	d := DecideFinishAction(FinishToolCalls, false, false)
	if d.Action != ActionContinue {
		t.Fatalf("tool_calls must continue: %+v", d)
	}
}

func TestNoFinishUnknownFinishReason(t *testing.T) {
	d := DecideFinishAction(FinishOther, false, false)
	if d.Action != ActionRetry || d.Category != System {
		t.Fatalf("unknown finish reason must retry as system: %+v", d)
	}
}

func TestNoFinishClassifiedEvent(t *testing.T) {
	c := Classify(ErrorEnvelope{Kind: KindAgent, Code: "E_STAGE_NOT_FINISHED"})
	if c.Category != NoFinish || !c.Retryable {
		t.Fatalf("silent finish must classify as no_finish: %+v", c)
	}
	e := ErrorEvent{
		TaskID: "task-1", Stage: "c", AttemptID: "task-1-c-1",
		Category: NoFinish, Retryable: true, RetryIndex: 1, MaxRetries: 2,
		OriginalError: "silent timeout", OriginalFinishReason: "stop",
	}
	p := e.Payload()
	if p["category"] != "no_finish" || p["original_finish_reason"] != "stop" {
		t.Fatalf("event payload = %v", p)
	}
}
