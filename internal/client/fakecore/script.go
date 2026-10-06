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
	// VerdictsByActivityType maps a wire activity_type to the verdict JSON served
	// for every row of that type that no Verdicts entry claimed. It exists for
	// rows with no tool_use_id to key on -- a model-call gate ("model_call_gate")
	// -- so a scenario can deny one without touching Default, which would answer
	// every other event the same way. A tool_use_id match wins over it.
	VerdictsByActivityType map[string]string
	// AlwaysStatus, when non-zero, answers every request with this status.
	AlwaysStatus int
	// Delay holds each response, for the timeout paths.
	Delay time.Duration
	// DelayFor holds the response for one specific wire event_type (e.g.
	// "SessionStarted", "ToolCall") instead of every request, keyed by the
	// same string Received.EventType() reads back. A scenario proving "core
	// durably accepted this event but was slow to answer" (a caller's own
	// attempt gives up on the response, not on whether the event stuck)
	// wants this rather than Delay: the v3 evaluate route already appends an
	// accepted request to the inbox BEFORE holding the response either way,
	// so DelayFor changes nothing about acceptance, only which class of
	// event the hold applies to. Absent for a given event_type falls back to
	// Delay.
	DelayFor map[string]time.Duration
}

// delayFor picks this event's own hold: DelayFor[eventType] when present,
// else the blanket Delay.
func (s Script) delayFor(eventType string) time.Duration {
	if d, ok := s.DelayFor[eventType]; ok {
		return d
	}
	return s.Delay
}

const allowVerdict = `{"governance_event_id":"ge","verdict":"allow","risk_score":0.1,"action":"continue","fallback_used":false}`

func (s Script) withDefaults() Script {
	if s.Default == "" {
		s.Default = allowVerdict
	}
	return s
}

// answer picks the verdict and status for one request.
func (s Script) answer(toolUseID, activityType string) (int, string) {
	status := http.StatusOK
	if s.AlwaysStatus != 0 {
		status = s.AlwaysStatus
	}
	if v, ok := s.Verdicts[toolUseID]; ok && toolUseID != "" {
		return status, v
	}
	if v, ok := s.VerdictsByActivityType[activityType]; ok && activityType != "" {
		return status, v
	}
	return status, s.Default
}
