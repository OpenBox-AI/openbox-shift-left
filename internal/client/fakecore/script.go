package fakecore

import (
	"net/http"
	"time"
)

// Script is what the fake answers. A verdict is chosen per tool call, so one
// scenario can allow a Read and deny a Bash in the same session -- which is the
// only way to produce a legitimately single-sided activity.
type Script struct {
	// Default is the verdict JSON served when no per-call verdict matches,
	// including for every lifecycle event (which carries no tool_use_id).
	Default string
	// Verdicts maps a tool_use_id to the verdict JSON served for that call.
	Verdicts map[string]string
	// AlwaysStatus, when non-zero, answers every request with this status.
	AlwaysStatus int
	// Delay holds each response, for the timeout paths.
	Delay time.Duration
}

const allowVerdict = `{"governance_event_id":"ge","verdict":"allow","risk_score":0.1,"action":"continue","fallback_used":false}`

func (s Script) withDefaults() Script {
	if s.Default == "" {
		s.Default = allowVerdict
	}
	return s
}

// answer picks the verdict and status for one request.
func (s Script) answer(toolUseID string) (int, string) {
	status := http.StatusOK
	if s.AlwaysStatus != 0 {
		status = s.AlwaysStatus
	}
	if v, ok := s.Verdicts[toolUseID]; ok && toolUseID != "" {
		return status, v
	}
	return status, s.Default
}
