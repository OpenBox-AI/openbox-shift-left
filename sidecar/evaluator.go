package sidecar

import (
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Evaluator is the local decision seam. Given a DecisionRequest it returns the
// governance Evaluation — synchronously, with NO network I/O (INV-3b). This is
// the extension point ADR-0003 describes: the Phase-1 default is bundleEvaluator
// (a local rule bundle); a future embedded-OPA evaluator that loads core's rego
// and queries data.org.<org>.policy_<id> (feeding it BuildOPAInput) implements
// the SAME interface and drops in with zero change to the server or protocol.
//
// An Evaluator MUST be safe for concurrent use (the server serves connections in
// parallel) and MUST NOT block on I/O — it answers from already-resident policy.
type Evaluator interface {
	Evaluate(req DecisionRequest) client.Evaluation
}

// bundleEvaluator is the default local Evaluator: it matches the request against
// the rules of an in-memory Bundle and yields a client.Evaluation.
//
// Aggregation is MAX-SEVERITY across all matching rules, mirroring core's
// HighestPriorityVerdict (openbox-core internal/content/governance.go:117-131,
// priority ALLOW<CONSTRAIN<REQUIRE_APPROVAL<BLOCK<HALT). This is deliberately
// safer than first-match-wins: overlapping rules can never *under*-block. The
// SDK's full enforce cascade (HALT>BLOCK>guardrails>REQUIRE_APPROVAL>CONSTRAIN>
// ALLOW, verdict_handler.enforce_verdict) is applied by E6-S2 when it turns this
// Evaluation into a Claude Code permissionDecision; here we produce the verdict
// the cascade acts on.
type bundleEvaluator struct {
	bundle *Bundle
}

func newBundleEvaluator(b *Bundle) *bundleEvaluator { return &bundleEvaluator{bundle: b} }

func (e *bundleEvaluator) Evaluate(req DecisionRequest) client.Evaluation {
	b := e.bundle
	if b == nil {
		// No policy loaded → allow (the server handles cold-start fail-open; this is
		// belt-and-suspenders so a nil bundle can never deny).
		return client.Evaluation{Verdict: client.VerdictAllow}
	}

	// Start from the bundle default (validated to be allow-class; OD9 fail-open).
	best := client.Evaluation{
		Verdict:  decisionToVerdict(b.DefaultDecision),
		PolicyID: "",
	}
	bestPri := verdictPriority(best.Verdict)

	for _, r := range b.Rules {
		if !r.Match.matches(req) {
			continue
		}
		v := decisionToVerdict(r.Decision)
		if p := verdictPriority(v); p >= bestPri {
			bestPri = p
			best = client.Evaluation{
				Verdict:     v,
				Reason:      r.Reason,
				PolicyID:    r.PolicyID,
				Constraints: r.Constraints,
			}
		}
	}
	return best
}

// verdictPriority orders verdicts by severity, matching core's
// HighestPriorityVerdict (governance.go:38-44). VerdictUnknown is treated as the
// lowest (allow-class) so a stray unknown never wins over a real allow.
func verdictPriority(v client.Verdict) int {
	switch v {
	case client.VerdictHalt:
		return 4
	case client.VerdictBlock:
		return 3
	case client.VerdictRequireApproval:
		return 2
	case client.VerdictConstrain:
		return 1
	case client.VerdictAllow:
		return 0
	default: // VerdictUnknown / anything else
		return -1
	}
}

// matches reports whether every SET predicate field matches the request. Unset
// fields are wildcards. All comparisons are metadata-only (INV-2).
func (m RuleMatch) matches(req DecisionRequest) bool {
	if m.EventType != "" && !strings.EqualFold(m.EventType, string(req.EventType)) {
		return false
	}
	if m.ToolName != "" && m.ToolName != req.Tool.Name {
		return false
	}
	if m.ToolKind != "" && !strings.EqualFold(m.ToolKind, string(req.Tool.Kind)) {
		return false
	}
	if m.MCPServer != "" && m.MCPServer != req.Tool.MCPServer {
		return false
	}
	for k, sub := range m.AttributeContains {
		if !strings.Contains(attrString(req.Attributes, k), sub) {
			return false
		}
	}
	for k, want := range m.AttributeEquals {
		if attrString(req.Attributes, k) != want {
			return false
		}
	}
	return true
}

// attrString renders the named attribute as a string for matching. Only string
// attributes participate in matching; a non-string (or absent) attribute yields
// "" (so a Contains/Equals against it fails closed to "no match", never panics).
func attrString(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	if s, ok := attrs[key].(string); ok {
		return s
	}
	return ""
}
