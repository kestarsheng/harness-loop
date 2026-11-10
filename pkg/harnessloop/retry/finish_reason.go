package retry

type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishOther         FinishReason = "other"
)

func normalizeRaw(raw string) FinishReason {
	switch raw {
	case "stop":
		return FinishStop
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishToolCalls
	case "content_filter":
		return FinishContentFilter
	default:
		return FinishOther
	}
}

func NormalizeFinishReason(raw string) FinishReason { return normalizeRaw(raw) }

func SuggestCategory(fr FinishReason, finishToolCalled bool) Category {
	switch fr {
	case FinishContentFilter:
		return ContentFilter
	case FinishLength:
		return Runtime
	case FinishStop:
		if !finishToolCalled {
			return NoFinish
		}
		return ""
	case FinishToolCalls:
		return ""
	default:
		return System
	}
}

func FinishReasonRetryable(fr FinishReason) bool {
	return fr != FinishContentFilter
}
