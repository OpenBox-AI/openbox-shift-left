package hookflow

import "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"

// FailurePolicy is what happens when OpenBox cannot produce a real verdict.
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure; the tool proceeds
	// (default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies the tool call on an evaluation failure (explicit per-org
	// opt-in).
	FailClosed
)

// ResolveFailurePolicy reads the org's configured failure policy.
//
// Always FailClosed: delivery is always fail-closed now (every unaccepted
// event halts the run, HaltOnDeliveryFailure), so there is no longer a
// choice to read here. devconfig.ResolveFailClosed is still called so the
// deprecated `fail_closed`/OPENBOX_FAIL_CLOSED key is still PARSED (and can
// still warn through the deprecated-key path), even though its value no
// longer changes what this returns.
func ResolveFailurePolicy() FailurePolicy {
	_ = devconfig.ResolveFailClosed()
	return FailClosed
}

// String renders the policy for the enforce diagnostic line, so a fail-closed
// deny; a synthesized HALT carrying FailOpen==true; is legible rather than
// reading as a contradiction.
func (p FailurePolicy) String() string {
	if p == FailClosed {
		return "fail_closed"
	}
	return "fail_open"
}
