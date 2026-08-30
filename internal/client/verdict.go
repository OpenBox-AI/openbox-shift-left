package client

import (
	"encoding/json"
	"fmt"
)

// Verdict is the canonical governance verdict, priority-ordered HALT > BLOCK >
// REQUIRE_APPROVAL > constrain > ALLOW (contract $defs.verdict). Observe mode
// (INV-3) treats every verdict as allow and never blocks the caller; callers
// parse it only for finops/audit and enforcement readiness.
type Verdict string

const (
	VerdictAllow           Verdict = "ALLOW"
	VerdictConstrain       Verdict = "CONSTRAIN"
	VerdictRequireApproval Verdict = "REQUIRE_APPROVAL"
	VerdictBlock           Verdict = "BLOCK"
	VerdictHalt            Verdict = "HALT"
	// VerdictUnknown is returned when the response carries no recognized verdict
	// (e.g. A fail-open drop, or an unmapped wire string). Never treated as deny.
	VerdictUnknown Verdict = ""
)

var wireToVerdict = map[string]Verdict{
	"allow":            VerdictAllow,
	"constrain":        VerdictConstrain,
	"require_approval": VerdictRequireApproval,
	"block":            VerdictBlock,
	"halt":             VerdictHalt,
}

var legacyActionToVerdict = map[string]Verdict{
	"continue":         VerdictAllow,
	"require-approval": VerdictRequireApproval,
	"stop":             VerdictBlock,
}

// AdvisoryRiskThreshold is the risk_score at/above which an evaluation is
// worth recording even when the verdict is ALLOW and no guardrail/constraint
// fired (the "non-trivial risk" clause of the Advisory-tier recording rule).
// Risk_score is a 0..1 float; 0.5 is a tunable mid-band heuristic, not an
// enforcement threshold (Advisory records, never blocks; INV-3).
const AdvisoryRiskThreshold = 0.5

// GuardrailReason is one structured guardrail finding; a category, never
// content (INV-2).
type GuardrailReason struct {
	Type   string `json:"type,omitempty"`
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GuardrailResult is the parsed subset of core's guardrails_result an Advisory
// record consumes: whether validation passed and the category reasons. The
// SDK's `redacted_input` and `raw_logs` are deliberately not parsed; they
// carry content and would cross INV-2.
type GuardrailResult struct {
	// Passed mirrors the wire `validation_passed`, which defaults true when the
	// field is absent (SDK GuardrailsCheckResult); modeled via *bool on the wire
	// so an omitted flag is not misread as a failure.
	Passed  bool
	Reasons []GuardrailReason
}

// DriftResult is the content-free subset of core's age_result (AGE = behavior
// and goal-alignment; the goal-drift / behavioral checks).
type DriftResult struct {
	// GoalDrifted mirrors age_result.goal_drifted; goal misalignment detected.
	GoalDrifted bool
	// GoalAlignmentChecked mirrors age_result.goal_alignment_checked; whether the
	// alignment classifier actually ran (false ⇒ the drift signal is not
	// meaningful).
	GoalAlignmentChecked bool
	// ViolationsCount mirrors age_result.violations_count; the number of
	// behavioral violations, a count only (never the violation strings, which are
	// free text).
	ViolationsCount int
}

// Detected reports whether this drift result carries a real finding worth
// surfacing: the classifier ran AND it saw drift or a behavioral violation.
func (d *DriftResult) Detected() bool {
	return d != nil && d.GoalAlignmentChecked && (d.GoalDrifted || d.ViolationsCount > 0)
}

// Evaluation is the rich, forward-compatible result of one /evaluate call; the
// Advisory-tier value: the resolved Verdict plus the sibling signals core
// returns alongside it (trust/risk/alignment scores, constraints, guardrail
// findings, policy/approval ids).
type Evaluation struct {
	Verdict              Verdict
	Reason               string
	PolicyID             string
	RiskScore            float64
	AlignmentScore       *float64 // absent (SDK Optional[float]) when nil
	TrustTier            string   // free-form; core may send a string or an int
	BehavioralViolations []string
	Constraints          []map[string]any // open shape (SDK List[Dict[str,Any]])
	ApprovalID           string
	GovernanceEventID    string
	Guardrail            *GuardrailResult // nil when core sends no guardrails_result
	Drift                *DriftResult     // nil when core sends no age_result
}

// WouldBlock is the Advisory label: whether this verdict would have blocked
// the tool call had enforcement been switched on.
func (e Evaluation) WouldBlock() bool {
	return e.Verdict == VerdictBlock || e.Verdict == VerdictHalt
}

// ApprovalRef is the server-side reference a developer or approver uses to
// find this evaluation's approval record; quoted on the approval prompt and
// carried into the enforcement audit.
func (e Evaluation) ApprovalRef() string {
	if e.ApprovalID != "" {
		return e.ApprovalID
	}
	return e.GovernanceEventID
}

// IsAdvisory reports whether an evaluation is worth recording: a non-ALLOW
// verdict, or any guardrail hit / constraint / non-trivial risk.
func (e Evaluation) IsAdvisory() bool {
	switch e.Verdict {
	case VerdictConstrain, VerdictRequireApproval, VerdictBlock, VerdictHalt:
		return true
	}
	if e.RiskScore >= AdvisoryRiskThreshold {
		return true
	}
	if len(e.Constraints) > 0 {
		return true
	}
	if e.Guardrail != nil && (!e.Guardrail.Passed || len(e.Guardrail.Reasons) > 0) {
		return true
	}
	if e.Drift.Detected() {
		return true
	}
	return false
}

type verdictResponse struct {
	Verdict              string           `json:"verdict"`
	Action               string           `json:"action"`
	Reason               string           `json:"reason"`
	PolicyID             string           `json:"policy_id"`
	RiskScore            float64          `json:"risk_score"`
	AlignmentScore       *float64         `json:"alignment_score"`
	TrustTier            any              `json:"trust_tier"`
	BehavioralViolations []string         `json:"behavioral_violations"`
	Constraints          []map[string]any `json:"constraints"`
	ApprovalID           string           `json:"approval_id"`
	GovernanceEventID    string           `json:"governance_event_id"`
	GuardrailsResult     *guardrailsWire  `json:"guardrails_result"`
	AGEResult            *ageWire         `json:"age_result"`
}

type guardrailsWire struct {
	ValidationPassed *bool             `json:"validation_passed"`
	Reasons          []GuardrailReason `json:"reasons"`
}

type ageWire struct {
	GoalAlignmentChecked bool `json:"goal_alignment_checked"`
	GoalDrifted          bool `json:"goal_drifted"`
	ViolationsCount      int  `json:"violations_count"`
}

func resolveVerdict(verdict, action string) Verdict {
	if v, ok := wireToVerdict[verdict]; ok {
		return v
	}
	if v, ok := legacyActionToVerdict[action]; ok {
		return v
	}
	return VerdictUnknown
}

type verdictOnly struct {
	Verdict string `json:"verdict"`
	Action  string `json:"action"`
}

// parseVerdict resolves a canonical Verdict from a raw /evaluate response
// body. It prefers the lowercase `verdict` field and falls back to the legacy
// `action` field; anything unrecognized yields VerdictUnknown (never deny).
func parseVerdict(body []byte) Verdict {
	var r verdictOnly
	if err := json.Unmarshal(body, &r); err != nil {
		return VerdictUnknown
	}
	return resolveVerdict(r.Verdict, r.Action)
}

func parseEvaluation(body []byte) Evaluation {
	var r verdictResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Evaluation{Verdict: parseVerdict(body)}
	}
	ev := Evaluation{
		Verdict:              resolveVerdict(r.Verdict, r.Action),
		Reason:               r.Reason,
		PolicyID:             r.PolicyID,
		RiskScore:            r.RiskScore,
		AlignmentScore:       r.AlignmentScore,
		TrustTier:            trustTierString(r.TrustTier),
		BehavioralViolations: r.BehavioralViolations,
		Constraints:          r.Constraints,
		ApprovalID:           r.ApprovalID,
		GovernanceEventID:    r.GovernanceEventID,
	}
	if g := r.GuardrailsResult; g != nil {
		ev.Guardrail = &GuardrailResult{
			Passed:  g.ValidationPassed == nil || *g.ValidationPassed, // absent ⇒ passed
			Reasons: g.Reasons,
		}
	}
	if a := r.AGEResult; a != nil {
		ev.Drift = &DriftResult{
			GoalDrifted:          a.GoalDrifted,
			GoalAlignmentChecked: a.GoalAlignmentChecked,
			ViolationsCount:      a.ViolationsCount,
		}
	}
	return ev
}

func trustTierString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
