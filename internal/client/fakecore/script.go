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
	// Status forces an HTTP status on the nth accepted request (1-based),
	// for the fail-closed and retry paths. Absent entries answer 200.
	Status map[int]int
	// AlwaysStatus, when non-zero, answers every request with this status.
	AlwaysStatus int
	// Delay holds each response, for the timeout paths.
	Delay time.Duration
	// SeedB64, when set, is the key the fake verifies against instead of
	// minting its own. Call sites with an existing fixed test identity keep it
	// and gain real verification.
	SeedB64 string
}

const allowVerdict = `{"governance_event_id":"ge","verdict":"allow","risk_score":0.1,"action":"continue","fallback_used":false}`

func (s Script) withDefaults() Script {
	if s.Default == "" {
		s.Default = allowVerdict
	}
	return s
}

// answer picks the verdict for the nth accepted request. n is 1-based and
// counts accepted requests only, which is what a test scripting "the third
// call 500s" means.
func (s Script) answer(n int, toolUseID string) (int, string) {
	status := http.StatusOK
	if s.AlwaysStatus != 0 {
		status = s.AlwaysStatus
	}
	if st, ok := s.Status[n]; ok {
		status = st
	}
	if v, ok := s.Verdicts[toolUseID]; ok && toolUseID != "" {
		return status, v
	}
	return status, s.Default
}
