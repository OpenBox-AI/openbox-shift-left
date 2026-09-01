package hookflow

// EvidenceState is the completeness of a session's telemetry at session end;
type EvidenceState struct {
	// Undelivered counts events in carry-over files: session-scoped, as ever.
	Undelivered int
	// Discarded counts events given up on entirely: machine-wide.
	Discarded int
}

func (e EvidenceState) Metadata() map[string]any {
	state := "complete"
	if e.Undelivered > 0 || e.Discarded > 0 {
		state = "degraded"
	}
	m := map[string]any{"evidence_state": state}
	if e.Undelivered > 0 {
		m["evidence_undelivered"] = e.Undelivered
	}
	if e.Discarded > 0 {
		m["evidence_discarded"] = e.Discarded
	}
	return m
}
