package client

import "testing"

// TestParseEvaluation_FullFields parses a rich /evaluate response (mirroring
// the reference SDK GovernanceVerdictResponse) and asserts every sibling
// signal is carried onto the Evaluation; the Advisory-tier value (story-SL-9).
func TestParseEvaluation_FullFields(t *testing.T) {
	body := []byte(`{
		"verdict": "block",
		"reason": "policy X",
		"policy_id": "pol-1",
		"risk_score": 0.87,
		"alignment_score": 0.42,
		"trust_tier": "gold",
		"behavioral_violations": ["escalation"],
		"constraints": [{"type": "rate_limit", "value": 100}],
		"approval_id": "appr-9",
		"governance_event_id": "ge-1",
		"guardrails_result": {
			"validation_passed": false,
			"redacted_input": "SHOULD-NOT-BE-PARSED",
			"reasons": [{"type": "pii", "field": "email", "reason": "Contains PII"}]
		}
	}`)

	e := parseEvaluation(body)

	if e.Verdict != VerdictBlock {
		t.Errorf("verdict = %q, want BLOCK", e.Verdict)
	}
	if !e.WouldBlock() {
		t.Error("WouldBlock() = false, want true for BLOCK")
	}
	if !e.IsAdvisory() {
		t.Error("IsAdvisory() = false, want true")
	}
	if e.Reason != "policy X" || e.PolicyID != "pol-1" || e.ApprovalID != "appr-9" || e.GovernanceEventID != "ge-1" {
		t.Errorf("id/reason fields mismapped: %+v", e)
	}
	if e.RiskScore != 0.87 {
		t.Errorf("risk_score = %v, want 0.87", e.RiskScore)
	}
	if e.AlignmentScore == nil || *e.AlignmentScore != 0.42 {
		t.Errorf("alignment_score = %v, want 0.42", e.AlignmentScore)
	}
	if e.TrustTier != "gold" {
		t.Errorf("trust_tier = %q, want gold", e.TrustTier)
	}
	if len(e.BehavioralViolations) != 1 || e.BehavioralViolations[0] != "escalation" {
		t.Errorf("behavioral_violations = %v", e.BehavioralViolations)
	}
	if len(e.Constraints) != 1 || e.Constraints[0]["type"] != "rate_limit" {
		t.Errorf("constraints = %v", e.Constraints)
	}
	if e.Guardrail == nil {
		t.Fatal("guardrail = nil, want parsed")
	}
	if e.Guardrail.Passed {
		t.Error("guardrail.Passed = true, want false (validation_passed:false)")
	}
	if len(e.Guardrail.Reasons) != 1 {
		t.Fatalf("guardrail reasons = %v, want 1", e.Guardrail.Reasons)
	}
	r := e.Guardrail.Reasons[0]
	if r.Type != "pii" || r.Field != "email" || r.Reason != "Contains PII" {
		t.Errorf("guardrail reason = %+v", r)
	}
	// The advisory-record test asserts the persisted record likewise never
	// carries it (internal/adapters/claude-code advisory_test.go).
}

// TestParseEvaluation_VerdictOnly proves graceful degradation: a Phase-1 core
// that returns only `verdict` still yields a usable Evaluation with every rich
// field at its zero value (never an error); the load-bearing forward-compat
// AC.
func TestParseEvaluation_VerdictOnly(t *testing.T) {
	e := parseEvaluation([]byte(`{"verdict":"allow"}`))
	if e.Verdict != VerdictAllow {
		t.Errorf("verdict = %q, want ALLOW", e.Verdict)
	}
	if e.IsAdvisory() {
		t.Error("IsAdvisory() = true for a bare ALLOW, want false")
	}
	if e.RiskScore != 0 || e.AlignmentScore != nil || e.TrustTier != "" ||
		len(e.Constraints) != 0 || e.Guardrail != nil {
		t.Errorf("rich fields should be zero-valued, got %+v", e)
	}
}

// TestParseEvaluation_LegacyAction covers the pre-verdict core: only the
// legacy `action` field present, mapped through the compat table.
func TestParseEvaluation_LegacyAction(t *testing.T) {
	e := parseEvaluation([]byte(`{"action":"stop"}`))
	if e.Verdict != VerdictBlock {
		t.Errorf("action=stop → %q, want BLOCK", e.Verdict)
	}
}

// TestParseEvaluation_Malformed never errors: a body that will not decode into
// the rich shape falls back to VerdictUnknown (INV-3 fail-open, stop
// condition).
func TestParseEvaluation_Malformed(t *testing.T) {
	e := parseEvaluation([]byte(`not json`))
	if e.Verdict != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown on unparseable body", e.Verdict)
	}
	if e.IsAdvisory() {
		t.Error("IsAdvisory() = true on unknown, want false (nothing evaluated)")
	}
}

// TestParseEvaluation_GuardrailAbsentDefaultsPassed mirrors the SDK: an
// omitted validation_passed defaults to passed=true (not a false-y zero
// value).
func TestParseEvaluation_GuardrailAbsentDefaultsPassed(t *testing.T) {
	e := parseEvaluation([]byte(`{"verdict":"allow","guardrails_result":{"reasons":[]}}`))
	if e.Guardrail == nil {
		t.Fatal("guardrail = nil, want present")
	}
	if !e.Guardrail.Passed {
		t.Error("guardrail.Passed = false, want true when validation_passed is absent")
	}
}

// TestParseEvaluation_Drift parses core's age_result into the content-free
// DriftResult (story-E6-S11): only the boolean/count signals, never the free-
// text reason / final_trust_score / span_results detail.
func TestParseEvaluation_Drift(t *testing.T) {
	body := []byte(`{
		"verdict": "allow",
		"age_result": {
			"goal_alignment_checked": true,
			"goal_drifted": true,
			"violations_count": 2,
			"reason": "SECRET-DRIFT-DETAIL-SHOULD-NOT-BE-PARSED",
			"final_trust_score": {"trust_tier": "low"},
			"span_results": [{"behavioral_result": "SHOULD-NOT-BE-PARSED"}]
		}
	}`)
	e := parseEvaluation(body)
	if e.Drift == nil {
		t.Fatal("drift = nil, want present")
	}
	if !e.Drift.GoalDrifted || !e.Drift.GoalAlignmentChecked || e.Drift.ViolationsCount != 2 {
		t.Errorf("drift signals mismapped: %+v", e.Drift)
	}
	if !e.IsAdvisory() {
		t.Error("ALLOW with goal drift should be advisory")
	}
}

// TestParseEvaluation_DriftAbsent: no age_result → nil Drift, byte-identical
// parse.
func TestParseEvaluation_DriftAbsent(t *testing.T) {
	e := parseEvaluation([]byte(`{"verdict":"allow"}`))
	if e.Drift != nil {
		t.Errorf("drift = %+v, want nil when age_result absent", e.Drift)
	}
	if e.Drift.Detected() {
		t.Error("nil drift must report Detected()=false")
	}
	if e.IsAdvisory() {
		t.Error("plain ALLOW with no signals should not be advisory")
	}
}

// TestDriftDetected_NotCheckedIsNotAFinding: goal_drifted with the classifier
// NOT run is not a real finding (the signal is meaningless).
func TestDriftDetected_NotCheckedIsNotAFinding(t *testing.T) {
	d := &DriftResult{GoalDrifted: true, GoalAlignmentChecked: false, ViolationsCount: 3}
	if d.Detected() {
		t.Error("goal_drifted without goal_alignment_checked must not be Detected()")
	}
	if (Evaluation{Verdict: VerdictAllow, Drift: d}).IsAdvisory() {
		t.Error("unchecked drift must not make an ALLOW advisory")
	}
}

// TestParseEvaluation_TrustTierInt proves the ambiguous trust_tier wire type:
// an integer tier renders to a plain string (no ".0") without erroring.
func TestParseEvaluation_TrustTierInt(t *testing.T) {
	e := parseEvaluation([]byte(`{"verdict":"allow","trust_tier":3}`))
	if e.TrustTier != "3" {
		t.Errorf("trust_tier(int 3) = %q, want \"3\"", e.TrustTier)
	}
}

// TestIsAdvisory_RiskOnly: an ALLOW with non-trivial risk is still advisory.
func TestIsAdvisory_RiskOnly(t *testing.T) {
	if !(Evaluation{Verdict: VerdictAllow, RiskScore: AdvisoryRiskThreshold}).IsAdvisory() {
		t.Error("ALLOW with risk >= threshold should be advisory")
	}
	if (Evaluation{Verdict: VerdictAllow, RiskScore: 0.1}).IsAdvisory() {
		t.Error("ALLOW with trivial risk should not be advisory")
	}
}

// TestParseEvaluation_MalformedSiblingFieldKeepsVerdict a body whose rich
// fields do not decode must still yield its verdict.
func TestParseEvaluation_MalformedSiblingFieldKeepsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Verdict
	}{
		{"constraints is a string", `{"verdict":"block","constraints":"oops"}`, VerdictBlock},
		{"risk_score is a string", `{"verdict":"block","risk_score":"high"}`, VerdictBlock},
		{"guardrails is an array", `{"verdict":"require_approval","guardrails_result":[1,2]}`, VerdictRequireApproval},
		{"legacy action survives too", `{"action":"stop","behavioral_violations":{"a":1}}`, VerdictBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseEvaluation([]byte(tc.body)).Verdict; got != tc.want {
				t.Errorf("verdict = %v, want %v (a malformed sibling field must not discard the decision)", got, tc.want)
			}
		})
	}
}

// TestParseEvaluation_UnparseableBodyIsUnknown genuinely undecodable input
// still yields unknown; never a manufactured deny.
func TestParseEvaluation_UnparseableBodyIsUnknown(t *testing.T) {
	for _, body := range []string{``, `not json`, `[]`, `{"verdict":`} {
		if got := parseEvaluation([]byte(body)).Verdict; got != VerdictUnknown {
			t.Errorf("body %q: verdict = %v, want unknown", body, got)
		}
	}
}
