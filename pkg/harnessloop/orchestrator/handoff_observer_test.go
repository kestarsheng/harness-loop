package orchestrator

import (
	"context"
	"testing"
)

type recordingObserver struct {
	statuses []HandoffStatus
}

func (o *recordingObserver) HandoffTransition(_ context.Context, record *HandoffRecord) {
	o.statuses = append(o.statuses, record.Status)
}

func TestHandoffObserverReceivesCreateAndReject(t *testing.T) {
	service := NewHandoffService(nil, &fakeTransport{}, NewMemoryHandoffStore(), NewMemoryEmitter())
	observer := &recordingObserver{}
	service.WithObserver(observer)

	record, _, err := service.Create(context.Background(), validHandoff(), handoffReq(), nil)
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}
	if err := service.Reject(context.Background(), record.HandoffID, "scope drift"); err != nil {
		t.Fatalf("reject handoff: %v", err)
	}

	if len(observer.statuses) != 2 {
		t.Fatalf("observer calls = %d, want 2", len(observer.statuses))
	}
	if observer.statuses[0] != HandoffCreated || observer.statuses[1] != HandoffRejected {
		t.Fatalf("observer statuses = %v", observer.statuses)
	}
}

func TestHandoffTextSpanNotRecordedWithoutObserver(t *testing.T) {
	service := NewHandoffService(nil, &fakeTransport{}, NewMemoryHandoffStore(), NewMemoryEmitter())
	if _, _, err := service.Create(context.Background(), validHandoff(), handoffReq(), nil); err != nil {
		t.Fatalf("create handoff without observer: %v", err)
	}
}
