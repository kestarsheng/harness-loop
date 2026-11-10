package output

const (
	ExitOK             = 0
	ExitFailed         = 2
	ExitAborted        = 3
	ExitInvalidUsage   = 4
	ExitConnectionFail = 5
)

func ExitCodeForStatus(status, terminalReason string) int {
	switch status {
	case "completed":
		return ExitOK
	case "aborted":
		return ExitAborted
	case "failed":
		return ExitFailed
	}
	switch terminalReason {
	case "completed":
		return ExitOK
	case "aborted":
		return ExitAborted
	case "failed":
		return ExitFailed
	}
	return ExitFailed
}

type Envelope struct {
	OK             bool             `json:"ok"`
	TaskID         string           `json:"task_id,omitempty"`
	Status         string           `json:"status,omitempty"`
	TerminalReason string           `json:"terminal_reason,omitempty"`
	CurrentStage   string           `json:"current_stage,omitempty"`
	Stages         []StageSummary   `json:"stages,omitempty"`
	Tokens         int64            `json:"tokens,omitempty"`
	Cost           float64          `json:"cost,omitempty"`
	Events         []map[string]any `json:"events,omitempty"`
	Error          *ErrorDetail     `json:"error,omitempty"`
}

type StageSummary struct {
	Stage    string `json:"stage"`
	Attempts int    `json:"attempts"`
	Accepted bool   `json:"accepted"`
}

type ErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func Success(env Envelope) Envelope {
	env.OK = true
	return env
}

func Failure(code, message string, details map[string]any) Envelope {
	return Envelope{OK: false, Error: &ErrorDetail{Code: code, Message: Redact(message), Details: sanitize(details)}}
}

func sanitize(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		if isSecretKey(k) {
			out[k] = mask
			continue
		}
		out[k] = RedactAny(v)
	}
	return out
}
