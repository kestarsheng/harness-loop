package retry

type FinishAction string

const (
	ActionDone     FinishAction = "done"
	ActionContinue FinishAction = "continue"
	ActionCompact  FinishAction = "compact"
	ActionRetry    FinishAction = "retry"
	ActionFail     FinishAction = "fail"
)

type FinishDecision struct {
	Action   FinishAction
	Category Category
	Reason   string
}

func DecideFinishAction(fr FinishReason, finishToolCalled bool, compactionAttempted bool) FinishDecision {
	switch fr {
	case FinishContentFilter:
		return FinishDecision{Action: ActionFail, Category: ContentFilter, Reason: "model refused to generate"}
	case FinishLength:
		if !compactionAttempted {
			return FinishDecision{Action: ActionCompact, Reason: "context exhausted, compact before retry"}
		}
		return FinishDecision{Action: ActionRetry, Category: Runtime, Reason: "context still exhausted after compaction"}
	case FinishStop:
		if finishToolCalled {
			return FinishDecision{Action: ActionDone, Reason: "finish tool call received"}
		}
		return FinishDecision{Action: ActionRetry, Category: NoFinish, Reason: "stream ended without finish tool call"}
	case FinishToolCalls:
		return FinishDecision{Action: ActionContinue, Reason: "tool calls pending"}
	default:
		return FinishDecision{Action: ActionRetry, Category: System, Reason: "unknown finish reason"}
	}
}
