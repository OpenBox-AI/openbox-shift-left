package hookflow

import "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"

// FailurePolicy is what happens when OpenBox cannot produce a real verdict.
// It is a property of the org's posture, not of the developer tool, so every
// provider shares this one definition.
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure — the tool
	// proceeds (default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies the tool call on an evaluation failure (explicit
	// per-org opt-in). An OpenBox outage blocks work rather than letting it
	// through ungoverned.
	FailClosed
)

// ResolveFailurePolicy reads the org's configured failure policy.
func ResolveFailurePolicy() FailurePolicy {
	if devconfig.ResolveFailClosed() {
		return FailClosed
	}
	return FailOpen
}

// String renders the policy for the enforce diagnostic line, so a fail-closed
// deny — a synthesized HALT carrying FailOpen==true — is legible rather than
// reading as a contradiction.
func (p FailurePolicy) String() string {
	if p == FailClosed {
		return "fail_closed"
	}
	return "fail_open"
}
