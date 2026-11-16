package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"agent-platform/internal/cli/tui/views"
)

type Subtask struct {
	ID          string
	ParentID    string
	DAGID       string
	NodeID      string
	ParallelSet string
	Role        string
	Status      string
	Depth       int
	Index       int
	Attempt     int
	DependsOn   []string
	HandoffID   string
	AllocTokens int64
	Tokens      int64
	Cost        float64
	Artifacts   []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ModelSelection struct {
	Stage         string
	Attempt       int
	ModelID       string
	Strategy      string
	ResolvedFrom  string
	Reason        string
	Operator      string
	ApprovalID    string
	Fallback      bool
	ContextWindow int
	TokensIn      int64
	TokensOut     int64
	Cost          float64
	CreatedAt     time.Time
}

type Handoff struct {
	ID        string
	ParentID  string
	SubtaskID string
	From      string
	To        string
	Status    string
	Reason    string
	UpdatedAt time.Time
}

type Checkpoint struct {
	ID        string
	Kind      string
	Stage     string
	Attempt   int
	Watermark int64
	Payload   string
	CreatedAt time.Time
}

type KBSearchResult struct {
	Title      string
	Snippet    string
	Source     string
	DocumentID string
	ChunkID    string
	Score      float64
}

type KBSource interface {
	Search(ctx context.Context, query string, topK int) ([]KBSearchResult, int64, time.Duration, error)
}

type panelMsg struct {
	Mode        ViewMode
	Subtasks    []Subtask
	Models      []ModelSelection
	Handoffs    []Handoff
	Checkpoints []Checkpoint
	Err         error
}

type kbSearchMsg struct {
	Items   []KBSearchResult
	Total   int64
	Latency time.Duration
	Err     error
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Model) refreshPanelCmd(mode ViewMode) tea.Cmd {
	src := m.src
	if src == nil {
		return nil
	}
	switch mode {
	case ViewSubtasks, ViewModels, ViewArtifacts:
	default:
		return nil
	}
	ctx := m.baseCtx()
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		switch mode {
		case ViewSubtasks:
			subtasks, err1 := src.Subtasks(callCtx)
			handoffs, err2 := src.Handoffs(callCtx)
			return panelMsg{Mode: mode, Subtasks: subtasks, Handoffs: handoffs, Err: firstErr(err1, err2)}
		case ViewArtifacts:
			subtasks, err1 := src.Subtasks(callCtx)
			checkpoints, err2 := src.Checkpoints(callCtx)
			return panelMsg{Mode: mode, Subtasks: subtasks, Checkpoints: checkpoints, Err: firstErr(err1, err2)}
		default:
			selections, err := src.ModelSelections(callCtx)
			return panelMsg{Mode: mode, Models: selections, Err: err}
		}
	}
}

func (m *Model) applyPanelMsg(msg panelMsg) {
	switch msg.Mode {
	case ViewSubtasks, ViewArtifacts:
		if msg.Subtasks != nil || msg.Err == nil {
			m.subagents.Subtasks = toSubtaskNodes(msg.Subtasks)
			if msg.Mode == ViewSubtasks {
				m.subagents.Handoffs = toHandoffLinks(msg.Handoffs)
			}
			if msg.Mode == ViewArtifacts {
				m.subagents.Checkpoints = toCheckpointItems(msg.Checkpoints)
			}
		}
		if msg.Err != nil {
			m.subagents.Error = "load failed: " + msg.Err.Error()
		} else {
			m.subagents.Error = ""
		}
	case ViewModels:
		if msg.Models != nil || msg.Err == nil {
			m.models.Selections = toModelItems(msg.Models)
		}
		if msg.Err != nil {
			m.models.Error = "load failed: " + msg.Err.Error()
		} else {
			m.models.Error = ""
		}
	}
	m.syncModelCounters()
}

func (m *Model) syncModelCounters() {
	m.models.Fallbacks = m.modelFallbacks
	m.models.Switches = m.modelSwitches
	m.models.Recovered = m.modelRecovered
	m.models.Unavailable = m.modelUnavailable
	m.models.Rejected = m.modelRejected
}

func (m *Model) kbSearchCmd() tea.Cmd {
	query := strings.TrimSpace(m.kb.Query)
	if query == "" {
		return nil
	}
	src := m.kbSrc
	ctx := m.baseCtx()
	topK := m.kbTopK
	if src == nil {
		m.kb.Loading = false
		m.kb.Error = "knowledge service is not configured (use --kb-server)"
		return nil
	}
	m.kb.Loading = true
	m.kb.Error = ""
	return func() tea.Msg {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		items, total, latency, err := src.Search(callCtx, query, topK)
		return kbSearchMsg{Items: items, Total: total, Latency: latency, Err: err}
	}
}

func (m *Model) applyKBSearchMsg(msg kbSearchMsg) {
	m.kb.Loading = false
	m.kb.Loaded = true
	if msg.Err != nil {
		m.kb.Error = "search failed: " + msg.Err.Error()
		m.kb.Items = nil
		return
	}
	m.kb.Error = ""
	m.kb.Total = msg.Total
	m.kb.Latency = msg.Latency
	m.kb.Items = make([]views.KBItem, 0, len(msg.Items))
	for _, item := range msg.Items {
		m.kb.Items = append(m.kb.Items, views.KBItem{
			Title:      item.Title,
			Snippet:    item.Snippet,
			Source:     item.Source,
			DocumentID: item.DocumentID,
			ChunkID:    item.ChunkID,
			Score:      item.Score,
		})
	}
}

func (m *Model) kbData() views.KBData {
	data := m.kb
	if m.kbSrc == nil {
		data.Hint = "knowledge service unavailable · start knowledge-service or pass --kb-server"
	}
	return data
}

func (m *Model) healthData() views.HealthData {
	data := views.HealthData{
		TaskID:            m.state.TaskID,
		Status:            m.state.Status,
		Connected:         m.connected,
		LastEvent:         m.healthLastEvent,
		EventsSeen:        m.healthEvents,
		Reconnecting:      m.reconnecting,
		ReconnectAttempt:  m.reconnectAttempt,
		Errors:            m.healthErrors,
		Warnings:          m.healthWarnings,
		RetryCount:        m.state.RetryCount,
		AcceptanceFailed:  m.state.AcceptanceFailCount,
		PendingApprovals:  len(m.pendingApprovals),
		FailedSubtasks:    m.subtaskFailed,
		CircuitOpen:       m.circuitOpen,
		ModelFallbacks:    m.modelFallbacks,
		ModelSwitches:     m.modelSwitches,
		ModelUnavailable:  m.modelUnavailable,
		CheckpointDrift:   m.checkpointDrift,
		MemoryDegraded:    m.memoryDegraded,
		CompactionDegrade: m.compactionDegrade,
		StateRebuilds:     m.stateRebuilds,
		SecurityDenied:    m.securityDenied,
		ToolDenied:        m.toolDenied,
		BudgetTokens:      m.state.BudgetTokens,
		ConsumedTokens:    m.state.ConsumedTokens,
		BudgetCost:        m.state.BudgetCost,
		ConsumedCost:      m.state.ConsumedCost,
		Incidents:         m.incidents,
	}
	if m.errText != "" {
		data.Error = m.errText
	}
	return data
}

func (m *Model) recordEventHealth(ev Event, typ string, payload map[string]any) {
	if typ == "" {
		return
	}
	m.healthEvents++
	if !ev.Time.IsZero() {
		m.healthLastEvent = ev.Time
	} else {
		m.healthLastEvent = m.now()
	}
	switch ev.Severity {
	case "error":
		m.healthErrors++
	case "warn":
		m.healthWarnings++
	}
	switch typ {
	case "model.fallback":
		m.modelFallbacks++
	case "model.switched":
		m.modelSwitches++
	case "model.recovered":
		m.modelRecovered++
	case "model.unavailable":
		m.modelUnavailable++
	case "model.capability_rejected":
		m.modelRejected++
	case "subtask.failed", "subtask.cancelled":
		m.subtaskFailed++
	case "subtask.circuit_open":
		m.circuitOpen++
	case "checkpoint.drift", "checkpoint.corrupted":
		m.checkpointDrift++
	case "memory.index_degraded", "memory.corrupted":
		m.memoryDegraded++
	case "compaction.degraded", "compaction.rejected":
		m.compactionDegrade++
	case "state.rebuild":
		m.stateRebuilds++
	case "security.violation":
		m.securityDenied++
	case "tool.denied":
		m.toolDenied++
	}
	m.trackApproval(ev, typ, payload)
	m.trackSubtasks(ev, typ, payload)
	if isIncident(typ, ev.Severity) {
		summary := eventHint(typ)
		if payload != nil {
			if detail := payloadString(payload, "reason"); detail != "" {
				summary = detail
			} else if detail := payloadString(payload, "summary"); detail != "" {
				summary = detail
			} else if detail := payloadString(payload, "decision"); detail != "" {
				summary = strings.TrimSpace(detail + " " + payloadString(payload, "target"))
			}
		}
		m.incidents = append(m.incidents, views.Incident{
			Time:     ev.Time,
			Seq:      ev.Seq,
			Type:     typ,
			Category: incidentCategory(typ),
			Stage:    ev.Stage,
			Summary:  summary,
			Severity: ev.Severity,
		})
		if len(m.incidents) > 200 {
			m.incidents = m.incidents[len(m.incidents)-200:]
		}
	}
	m.syncModelCounters()
}

func (m *Model) trackApproval(ev Event, typ string, payload map[string]any) {
	id := payloadString(payload, "approval_id")
	if id == "" {
		id = fmt.Sprintf("seq-%d", ev.Seq)
	}
	switch typ {
	case "approval.requested":
		m.pendingApprovals[id] = true
	case "approval.approved", "approval.rejected", "approval.timeout", "approval.cancelled":
		if _, ok := m.pendingApprovals[id]; ok {
			delete(m.pendingApprovals, id)
			return
		}
		for pending := range m.pendingApprovals {
			delete(m.pendingApprovals, pending)
			break
		}
	}
}

func (m *Model) trackSubtasks(ev Event, typ string, payload map[string]any) {
	switch {
	case strings.HasPrefix(typ, "subtask."):
		id := payloadString(payload, "subtask_id")
		if id == "" {
			id = payloadString(payload, "id")
		}
		if id == "" {
			return
		}
		status := payloadString(payload, "status")
		if status == "" {
			status = strings.TrimPrefix(typ, "subtask.")
		}
		node := m.findSubtask(id)
		if node == nil {
			m.subagents.Subtasks = append(m.subagents.Subtasks, views.SubtaskNode{
				ID:        id,
				ParentID:  payloadString(payload, "parent_task_id"),
				DAGID:     payloadString(payload, "dag_id"),
				NodeID:    payloadString(payload, "dag_node_id"),
				Role:      payloadString(payload, "role"),
				Depth:     payloadInt(payload, "depth"),
				Index:     payloadInt(payload, "index_in_parent"),
				Status:    status,
				Attempt:   payloadInt(payload, "attempt_no"),
				UpdatedAt: ev.Time,
			})
			return
		}
		node.Status = status
		if attempt := payloadInt(payload, "attempt_no"); attempt > 0 {
			node.Attempt = attempt
		}
		node.UpdatedAt = ev.Time
	case strings.HasPrefix(typ, "handoff."):
		id := payloadString(payload, "handoff_id")
		if id == "" {
			return
		}
		status := payloadString(payload, "status")
		if status == "" {
			status = strings.TrimPrefix(typ, "handoff.")
		}
		for i := range m.subagents.Handoffs {
			if m.subagents.Handoffs[i].ID == id {
				m.subagents.Handoffs[i].Status = status
				m.subagents.Handoffs[i].UpdatedAt = ev.Time
				return
			}
		}
		m.subagents.Handoffs = append(m.subagents.Handoffs, views.HandoffLink{
			ID:        id,
			ParentID:  payloadString(payload, "parent_task_id"),
			SubtaskID: payloadString(payload, "subtask_id"),
			From:      payloadString(payload, "from_agent_id"),
			To:        payloadString(payload, "to_agent_id"),
			Status:    status,
			Reason:    payloadString(payload, "reason"),
			UpdatedAt: ev.Time,
		})
	case typ == "checkpoint.saved" || typ == "checkpoint.restored":
		id := payloadString(payload, "checkpoint_id")
		if id == "" {
			return
		}
		for i := range m.subagents.Checkpoints {
			if m.subagents.Checkpoints[i].ID == id {
				return
			}
		}
		m.subagents.Checkpoints = append(m.subagents.Checkpoints, views.CheckpointItem{
			ID:        id,
			Kind:      payloadString(payload, "kind"),
			Stage:     payloadString(payload, "stage"),
			Attempt:   payloadInt(payload, "attempt"),
			Watermark: int64(payloadInt(payload, "event_seq_watermark")),
			Payload:   payloadString(payload, "payload_ref"),
			CreatedAt: ev.Time,
		})
	}
}

func (m *Model) findSubtask(id string) *views.SubtaskNode {
	for i := range m.subagents.Subtasks {
		if m.subagents.Subtasks[i].ID == id {
			return &m.subagents.Subtasks[i]
		}
	}
	return nil
}

func isIncident(typ, severity string) bool {
	switch typ {
	case "security.violation", "tool.denied", "error", "event.invalid",
		"invalid_finish", "stage.illegal_transition", "retry.exhausted",
		"checkpoint.drift", "checkpoint.corrupted", "memory.corrupted", "memory.index_degraded",
		"compaction.degraded", "compaction.rejected", "aggregation.conflict",
		"subtask.failed", "subtask.rejected", "subtask.cancelled", "subtask.circuit_open",
		"model.capability_rejected", "model.unavailable", "model.fallback",
		"approval.requested", "approval.rejected", "approval.timeout", "approval.cancelled",
		"state.rebuild":
		return true
	}
	return severity == "error" || severity == "warn"
}

func incidentCategory(typ string) string {
	switch {
	case typ == "security.violation":
		return "security"
	case typ == "tool.denied":
		return "tool"
	case strings.HasPrefix(typ, "approval."):
		return "approval"
	case strings.HasPrefix(typ, "subtask."), typ == "aggregation.conflict":
		return "subtask"
	case strings.HasPrefix(typ, "model."):
		return "model"
	case strings.HasPrefix(typ, "checkpoint."):
		return "checkpoint"
	case strings.HasPrefix(typ, "memory."), strings.HasPrefix(typ, "compaction."):
		return "memory"
	case typ == "retry.exhausted":
		return "retry"
	case typ == "state.rebuild":
		return "state"
	default:
		return "system"
	}
}

var eventHints = map[string]string{
	"task.started":                   "task started",
	"task.completed":                 "task completed",
	"task.aborted":                   "task aborted",
	"task.failed":                    "task failed",
	"task.paused":                    "task paused",
	"task.resumed":                   "task resumed",
	"stage.entered":                  "stage entered",
	"stage.exited":                   "stage exited",
	"stage.transition":               "stage transition",
	"stage.illegal_transition":       "illegal stage transition",
	"invalid_finish":                 "invalid stage finish",
	"acceptance.started":             "acceptance started",
	"acceptance.passed":              "acceptance passed",
	"acceptance.failed":              "acceptance failed",
	"acceptance.invalid":             "acceptance invalid",
	"acceptance.rollback_overridden": "acceptance rollback overridden",
	"retry.scheduled":                "retry scheduled",
	"retry.exhausted":                "retry exhausted",
	"checkpoint.saved":               "checkpoint saved",
	"checkpoint.restored":            "checkpoint restored",
	"checkpoint.drift":               "checkpoint drift",
	"checkpoint.corrupted":           "checkpoint corrupted",
	"compaction.triggered":           "compaction triggered",
	"compaction.completed":           "compaction completed",
	"compaction.degraded":            "compaction degraded",
	"compaction.rejected":            "compaction rejected",
	"memory.written":                 "memory written",
	"memory.merged":                  "memory merged",
	"memory.promoted":                "memory promoted",
	"memory.evicted":                 "memory evicted",
	"memory.decayed":                 "memory decayed",
	"memory.corrupted":               "memory corrupted",
	"memory.resync":                  "memory resync",
	"memory.index_degraded":          "memory index degraded",
	"index.queued":                   "index queued",
	"index.updated":                  "index updated",
	"index.failed":                   "index failed",
	"index.rebuilt":                  "index rebuilt",
	"tool.started":                   "tool started",
	"tool.result":                    "tool result",
	"tool.progress":                  "tool progress",
	"tool.denied":                    "tool denied",
	"subtask.plan_created":           "subtask plan created",
	"subtask.queued":                 "subtask queued",
	"subtask.started":                "subtask started",
	"subtask.handoff_waiting":        "subtask waiting for handoff",
	"subtask.accepted":               "subtask accepted",
	"subtask.rejected":               "subtask rejected",
	"subtask.failed":                 "subtask failed",
	"subtask.cancelled":              "subtask cancelled",
	"subtask.circuit_open":           "subtask circuit open",
	"handoff.created":                "handoff created",
	"handoff.accepted":               "handoff accepted",
	"handoff.rejected":               "handoff rejected",
	"aggregation.completed":          "aggregation completed",
	"aggregation.conflict":           "aggregation conflict",
	"model.selected":                 "model selected",
	"model.switched":                 "model switched",
	"model.fallback":                 "model fallback",
	"model.capability_rejected":      "model capability rejected",
	"model.recovered":                "model recovered",
	"model.unavailable":              "model unavailable",
	"model.registry_changed":         "model registry changed",
	"model.config_applied":           "model config applied",
	"approval.requested":             "approval requested",
	"approval.approved":              "approval approved",
	"approval.rejected":              "approval rejected",
	"approval.timeout":               "approval timeout",
	"approval.cancelled":             "approval cancelled",
	"error":                          "error",
	"security.violation":             "security violation",
	"state.rebuild":                  "state rebuild",
	"event.invalid":                  "invalid event",
}

func eventHint(typ string) string {
	if hint, ok := eventHints[typ]; ok {
		return hint
	}
	if typ == "" {
		return "event"
	}
	return strings.ReplaceAll(typ, ".", " ")
}

func toSubtaskNodes(items []Subtask) []views.SubtaskNode {
	out := make([]views.SubtaskNode, 0, len(items))
	for _, item := range items {
		out = append(out, views.SubtaskNode{
			ID:          item.ID,
			ParentID:    item.ParentID,
			DAGID:       item.DAGID,
			NodeID:      item.NodeID,
			ParallelSet: item.ParallelSet,
			Role:        item.Role,
			Status:      item.Status,
			Depth:       item.Depth,
			Index:       item.Index,
			Attempt:     item.Attempt,
			DependsOn:   item.DependsOn,
			HandoffID:   item.HandoffID,
			AllocTokens: item.AllocTokens,
			Tokens:      item.Tokens,
			Cost:        item.Cost,
			Artifacts:   item.Artifacts,
			CreatedAt:   item.CreatedAt,
			UpdatedAt:   item.UpdatedAt,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func toModelItems(items []ModelSelection) []views.ModelSelectionItem {
	out := make([]views.ModelSelectionItem, 0, len(items))
	for _, item := range items {
		out = append(out, views.ModelSelectionItem{
			Stage:         item.Stage,
			Attempt:       item.Attempt,
			ModelID:       item.ModelID,
			Strategy:      item.Strategy,
			ResolvedFrom:  item.ResolvedFrom,
			Reason:        item.Reason,
			Operator:      item.Operator,
			ApprovalID:    item.ApprovalID,
			Fallback:      item.Fallback,
			ContextWindow: item.ContextWindow,
			TokensIn:      item.TokensIn,
			TokensOut:     item.TokensOut,
			Cost:          item.Cost,
			CreatedAt:     item.CreatedAt,
		})
	}
	return out
}

func toHandoffLinks(items []Handoff) []views.HandoffLink {
	out := make([]views.HandoffLink, 0, len(items))
	for _, item := range items {
		out = append(out, views.HandoffLink{
			ID:        item.ID,
			ParentID:  item.ParentID,
			SubtaskID: item.SubtaskID,
			From:      item.From,
			To:        item.To,
			Status:    item.Status,
			Reason:    item.Reason,
			UpdatedAt: item.UpdatedAt,
		})
	}
	return out
}

func toCheckpointItems(items []Checkpoint) []views.CheckpointItem {
	out := make([]views.CheckpointItem, 0, len(items))
	for _, item := range items {
		out = append(out, views.CheckpointItem{
			ID:        item.ID,
			Kind:      item.Kind,
			Stage:     item.Stage,
			Attempt:   item.Attempt,
			Watermark: item.Watermark,
			Payload:   item.Payload,
			CreatedAt: item.CreatedAt,
		})
	}
	return out
}
