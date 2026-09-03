package hookflow

// EvidenceState is the completeness of a session's telemetry at session end;
type EvidenceState struct {
	// Undelivered counts events in carry-over files: session-scoped, as ever.
	Undelivered int
	// Discarded counts events given up on entirely: machine-wide.
	Discarded int
}

func (e EvidenceState) Metadata() map[string]any {
	// state describes THIS session, so only this session's undelivered count
	// decides it. Discarded is machine-wide and cumulative -- the `.discarded` log
	// is append-only, reset only at its size cap -- so folding it in here made one
	// old loss mark every later session "degraded" forever, and the field stopped
	// distinguishing anything. The cumulative number keeps its own labelled key.
	state := "complete"
	if e.Undelivered > 0 {
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
