package service

import (
	"context"

	"agent-platform/pkg/harnessloop/orchestrator"
	"agent-platform/pkg/harnessloop/tracing"
	"agent-platform/services/harnessloop-service/internal/metrics"
)

// HandoffObservability adapts handoff lifecycle transitions to DFX-M-01 metrics
// and the handoff child span. It implements orchestrator.HandoffObserver.
type HandoffObservability struct {
	Metrics *metrics.Harness
	Tracer  *tracing.Tracer
}

func (o HandoffObservability) HandoffTransition(ctx context.Context, record *orchestrator.HandoffRecord) {
	if record == nil {
		return
	}
	o.Metrics.Handoff(handoffStatusLabel(record.Status), record.FromRole, record.ToRole)
	span := o.Tracer.StartHandoffFromContext(ctx, record.HandoffID, record.FromRole, record.ToRole)
	span.End()
}

// handoffStatusLabel maps the persisted status to the metric label set
// (requested/accepted/rejected); "created" is the request stage.
func handoffStatusLabel(status orchestrator.HandoffStatus) string {
	switch status {
	case orchestrator.HandoffCreated:
		return "requested"
	case orchestrator.HandoffAccepted:
		return "accepted"
	case orchestrator.HandoffRejected:
		return "rejected"
	default:
		return string(status)
	}
}
