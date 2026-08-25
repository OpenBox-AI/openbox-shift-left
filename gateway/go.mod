module github.com/openbox-ai/openbox-shift-left/gateway

go 1.23

// Phase 05 (ADR-0021): the gateway emits through the SAME client and redacts with
// the SAME detector as the hook path. Reusing them is this repo's own rule;
// reimplementing AIP signing or secret detection here would be strictly worse.
// gateway/guard_test.go's allowlist enumerates exactly these two and fails on a
// third, so the credential guard's bounded scope stays a decision.
require (
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	github.com/openbox-ai/openbox-shift-left/decision v0.0.0
)

replace github.com/openbox-ai/openbox-shift-left/client => ../client

replace github.com/openbox-ai/openbox-shift-left/decision => ../decision

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../contracts/dev-event/conformance
