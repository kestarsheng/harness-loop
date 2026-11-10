package event

type Severity string

const (
	SeverityDebug Severity = "debug"
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

func (s Severity) Valid() bool {
	switch s {
	case SeverityDebug, SeverityInfo, SeverityWarn, SeverityError:
		return true
	}
	return false
}

type EventType string

const (
	TaskStarted   EventType = "task.started"
	TaskCompleted EventType = "task.completed"
	TaskAborted   EventType = "task.aborted"
	TaskFailed    EventType = "task.failed"
	TaskPaused    EventType = "task.paused"
	TaskResumed   EventType = "task.resumed"

	StageEntered           EventType = "stage.entered"
	StageExited            EventType = "stage.exited"
	StageTransition        EventType = "stage.transition"
	StageIllegalTransition EventType = "stage.illegal_transition"
	InvalidFinish          EventType = "invalid_finish"

	AcceptanceStarted            EventType = "acceptance.started"
	AcceptancePassed             EventType = "acceptance.passed"
	AcceptanceFailed             EventType = "acceptance.failed"
	AcceptanceInvalid            EventType = "acceptance.invalid"
	AcceptanceRollbackOverridden EventType = "acceptance.rollback_overridden"

	RetryScheduled EventType = "retry.scheduled"
	RetryExhausted EventType = "retry.exhausted"

	CheckpointSaved     EventType = "checkpoint.saved"
	CheckpointRestored  EventType = "checkpoint.restored"
	CheckpointDrift     EventType = "checkpoint.drift"
	CheckpointCorrupted EventType = "checkpoint.corrupted"

	CompactionTriggered EventType = "compaction.triggered"
	CompactionCompleted EventType = "compaction.completed"
	CompactionDegraded  EventType = "compaction.degraded"
	CompactionRejected  EventType = "compaction.rejected"

	MemoryWritten       EventType = "memory.written"
	MemoryMerged        EventType = "memory.merged"
	MemoryPromoted      EventType = "memory.promoted"
	MemoryEvicted       EventType = "memory.evicted"
	MemoryDecayed       EventType = "memory.decayed"
	MemoryCorrupted     EventType = "memory.corrupted"
	MemoryResync        EventType = "memory.resync"
	MemoryIndexDegraded EventType = "memory.index_degraded"

	IndexQueued  EventType = "index.queued"
	IndexUpdated EventType = "index.updated"
	IndexFailed  EventType = "index.failed"
	IndexRebuilt EventType = "index.rebuilt"

	ToolStarted  EventType = "tool.started"
	ToolResult   EventType = "tool.result"
	ToolProgress EventType = "tool.progress"

	Error EventType = "error"

	SecurityViolation EventType = "security.violation"

	StateRebuild EventType = "state.rebuild"

	EventInvalid EventType = "event.invalid"
)

var knownTypes = map[EventType]struct{}{
	TaskStarted: {}, TaskCompleted: {}, TaskAborted: {}, TaskFailed: {}, TaskPaused: {}, TaskResumed: {},
	StageEntered: {}, StageExited: {}, StageTransition: {}, StageIllegalTransition: {}, InvalidFinish: {},
	AcceptanceStarted: {}, AcceptancePassed: {}, AcceptanceFailed: {}, AcceptanceInvalid: {}, AcceptanceRollbackOverridden: {},
	RetryScheduled: {}, RetryExhausted: {},
	CheckpointSaved: {}, CheckpointRestored: {}, CheckpointDrift: {}, CheckpointCorrupted: {},
	CompactionTriggered: {}, CompactionCompleted: {}, CompactionDegraded: {}, CompactionRejected: {},
	MemoryWritten: {}, MemoryMerged: {}, MemoryPromoted: {}, MemoryEvicted: {}, MemoryDecayed: {}, MemoryCorrupted: {}, MemoryResync: {}, MemoryIndexDegraded: {},
	IndexQueued: {}, IndexUpdated: {}, IndexFailed: {}, IndexRebuilt: {},
	ToolStarted: {}, ToolResult: {}, ToolProgress: {},
	Error:             {},
	SecurityViolation: {},
	StateRebuild:      {},
	EventInvalid:      {},
}

func IsKnownType(t EventType) bool {
	_, ok := knownTypes[t]
	return ok
}

func KnownTypes() []EventType {
	out := make([]EventType, 0, len(knownTypes))
	for t := range knownTypes {
		out = append(out, t)
	}
	return out
}
