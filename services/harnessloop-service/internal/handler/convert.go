package handler

import (
	"errors"

	"agent-platform/pkg/harnessloop/checkpoint"
	"agent-platform/pkg/harnessloop/event"
	"agent-platform/pkg/harnessloop/modelregistry"
	harnesspb "agent-platform/pkg/pb/harnessloop"
	"agent-platform/services/harnessloop-service/internal/repository"
)

func isNotFound(err error) bool { return errors.Is(err, repository.ErrTaskNotFound) }
func isConflict(err error) bool { return errors.Is(err, repository.ErrVersionConflict) }

func toProtoTask(t *repository.TaskRun) *harnesspb.TaskView {
	if t == nil {
		return nil
	}
	attempts := make(map[string]int32, len(t.AttemptNo))
	for k, v := range t.AttemptNo {
		attempts[k] = int32(v)
	}
	var retryTotal, failTotal int
	for _, v := range t.RetryCount {
		retryTotal += v
	}
	for _, v := range t.AcceptanceFail {
		failTotal += v
	}
	return &harnesspb.TaskView{
		TaskId:              t.TaskID,
		TenantId:            t.TenantID,
		ProjectId:           t.ProjectID,
		WorkspaceRoot:       t.WorkspaceRoot,
		Goal:                t.Goal,
		GoalHash:            t.GoalHash,
		CurrentStage:        string(t.CurrentStage),
		Status:              string(t.Status),
		TerminalReason:      t.TerminalReason,
		AttemptNoByStage:    attempts,
		RetryCount:          int32(retryTotal),
		AcceptanceFailCount: int32(failTotal),
		Budget:              &harnesspb.Budget{Tokens: t.BudgetTokens, Cost: t.BudgetCost},
		ConsumedTokens:      t.ConsumedTokens,
		ConsumedCost:        t.ConsumedCost,
		FsmEnabled:          t.FSMEnabled,
		Version:             t.Version,
		CreatedAtMs:         t.CreatedAt.UnixMilli(),
		UpdatedAtMs:         t.UpdatedAt.UnixMilli(),
	}
}

func toProtoEvent(e *event.Event) *harnesspb.HarnessEvent {
	if e == nil {
		return nil
	}
	pbEvent := &harnesspb.HarnessEvent{
		EventId:       e.EventID,
		EventType:     string(e.EventType),
		SchemaVersion: e.SchemaVersion,
		TaskId:        e.TaskID,
		TraceId:       e.TraceID,
		SpanId:        e.SpanID,
		Seq:           e.Seq,
		TimestampMs:   e.Timestamp.UnixMilli(),
		Severity:      string(e.Severity),
		TenantId:      e.TenantID,
		ProjectId:     e.ProjectID,
		AgentId:       e.AgentID,
		Payload:       e.Payload,
		PayloadRef:    e.PayloadRef,
		ParentTaskId:  e.ParentTaskID,
		SubtaskId:     e.SubtaskID,
		HandoffId:     e.HandoffID,
		DagId:         e.DagID,
		DagNodeId:     e.DagNodeID,
	}
	if e.Stage != "" {
		stage := e.Stage
		pbEvent.Stage = &stage
	}
	if e.Attempt != 0 {
		attempt := int32(e.Attempt)
		pbEvent.Attempt = &attempt
	}
	return pbEvent
}

func toProtoCheckpoint(cp *checkpoint.Checkpoint) *harnesspb.CheckpointView {
	if cp == nil {
		return nil
	}
	return &harnesspb.CheckpointView{
		CheckpointId:      cp.CheckpointID,
		TaskId:            cp.TaskID,
		Kind:              cp.Kind,
		Stage:             cp.Stage,
		Attempt:           int32(cp.AttemptNo),
		CreatedAtMs:       cp.CreatedAt.UnixMilli(),
		EventSeqWatermark: cp.EventSeqWatermark,
		PayloadRef:        cp.Checksum,
	}
}

func toModelEntry(v modelregistry.View) *harnesspb.ModelEntry {
	return &harnesspb.ModelEntry{
		Id:               v.ID,
		Provider:         v.Provider,
		ContextWindow:    int32(v.ContextWindow),
		MaxOutputTokens:  int32(v.MaxOutputTokens),
		ToolCall:         v.ToolCall,
		StructuredOutput: v.StructuredOutput,
		PriceInPer_1K:    v.PriceInPer1K,
		PriceOutPer_1K:   v.PriceOutPer1K,
		Status:           v.Status,
		Tags:             v.Tags,
	}
}
