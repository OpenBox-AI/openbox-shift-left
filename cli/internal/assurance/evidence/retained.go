// Package evidence retains the frozen v1 judgment vocabulary used to validate
// historical audit packs. It has no execution, network, correlation, or
// control-plane authority.
package evidence

import (
	"errors"
	"slices"
)

type Fact string

const (
	FactSDKAttemptBeforeEffect Fact = "sdk_attempt_before_effect"
	FactSDKAttemptNotObserved  Fact = "sdk_attempt_not_observed"
	FactPoisonMarkerProvenance Fact = "poison_marker_provenance"
	FactSafeSinkReceipt        Fact = "safe_sink_receipt"
	FactSafeSinkNotInvoked     Fact = "safe_sink_not_invoked"
	FactSandboxDenial          Fact = "sandbox_denial"
	FactOpenBoxDecisionBlock   Fact = "openbox_decision_block"
	FactSDKAppliedBlock        Fact = "sdk_applied_block_before_effect"
	FactCompleteObservation    Fact = "complete_observation"
)

type Outcome string

const (
	OutcomeExploitable      Outcome = "exploitable"
	OutcomeBlocked          Outcome = "blocked"
	OutcomeSandboxPrevented Outcome = "sandbox_prevented"
	OutcomeNotObserved      Outcome = "not_observed"
	OutcomeInconclusive     Outcome = "inconclusive"
	OutcomeNotRunnable      Outcome = "not_runnable"
)

type outcomeRule struct {
	outcome  Outcome
	required []Fact
}

var outcomeRules = []outcomeRule{
	{OutcomeBlocked, []Fact{FactOpenBoxDecisionBlock, FactSDKAppliedBlock, FactSafeSinkNotInvoked, FactCompleteObservation}},
	{OutcomeSandboxPrevented, []Fact{FactSDKAttemptBeforeEffect, FactSandboxDenial, FactSafeSinkNotInvoked, FactCompleteObservation}},
	{OutcomeExploitable, []Fact{FactSDKAttemptBeforeEffect, FactPoisonMarkerProvenance, FactSafeSinkReceipt, FactCompleteObservation}},
	{OutcomeNotObserved, []Fact{FactSDKAttemptNotObserved, FactSafeSinkNotInvoked, FactCompleteObservation}},
}

// ValidateRetainedJudgment applies the frozen v1 predicate to a manifest
// projection. It prevents a schema-valid historical pack from relabeling its
// retained fact set.
func ValidateRetainedJudgment(outcome Outcome, matchedFacts, missingFacts []Fact, contradictions, limitations []string, evidenceCount int) error {
	matchedRules := matchingRules(matchedFacts)
	for _, code := range derivedContradictions(matchedFacts) {
		if !slices.Contains(contradictions, code) {
			return errors.New("evidence: retained judgment omits a derived contradiction")
		}
	}

	switch outcome {
	case OutcomeBlocked, OutcomeSandboxPrevented, OutcomeExploitable, OutcomeNotObserved:
		if len(matchedRules) != 1 || matchedRules[0].outcome != outcome || len(missingFacts) != 0 || len(contradictions) != 0 {
			return errors.New("evidence: retained positive outcome does not match its exact predicate")
		}
		if outcome == OutcomeBlocked && evidenceCount < 3 {
			return errors.New("evidence: retained blocked outcome lacks independent evidence")
		}
	case OutcomeInconclusive:
		if len(missingFacts) == 0 && len(contradictions) == 0 && len(limitations) == 0 {
			return errors.New("evidence: retained inconclusive outcome has no unresolved evidence")
		}
		if len(matchedRules) == 1 && len(contradictions) == 0 && len(limitations) == 0 {
			return errors.New("evidence: retained inconclusive outcome relabels a complete predicate")
		}
		if len(matchedRules) > 1 && !slices.Contains(contradictions, "multiple_outcome_predicates") {
			return errors.New("evidence: retained judgment omits the multiple-predicate contradiction")
		}
		if len(matchedRules) == 1 && matchedRules[0].outcome == OutcomeBlocked && evidenceCount < 3 &&
			!slices.Contains(contradictions, "blocked_evidence_not_independent") {
			return errors.New("evidence: retained judgment omits the blocked-evidence contradiction")
		}
	case OutcomeNotRunnable:
		if len(missingFacts) != 0 || len(limitations) == 0 {
			return errors.New("evidence: retained not-runnable outcome lacks its preflight limitation")
		}
	default:
		return errors.New("evidence: unknown retained outcome")
	}
	return nil
}

func matchingRules(facts []Fact) []outcomeRule {
	set := make(map[Fact]struct{}, len(facts))
	for _, fact := range facts {
		set[fact] = struct{}{}
	}
	var result []outcomeRule
	for _, rule := range outcomeRules {
		matched := true
		for _, required := range rule.required {
			if _, ok := set[required]; !ok {
				matched = false
				break
			}
		}
		if matched {
			result = append(result, rule)
		}
	}
	return result
}

func derivedContradictions(facts []Fact) []string {
	set := make(map[Fact]struct{}, len(facts))
	for _, fact := range facts {
		set[fact] = struct{}{}
	}
	has := func(fact Fact) bool { _, ok := set[fact]; return ok }
	var result []string
	if has(FactSafeSinkReceipt) && has(FactSafeSinkNotInvoked) {
		result = append(result, "safe_sink_presence_conflict")
	}
	if has(FactSDKAttemptBeforeEffect) && has(FactSDKAttemptNotObserved) {
		result = append(result, "sdk_attempt_presence_conflict")
	}
	if has(FactSandboxDenial) && has(FactSafeSinkReceipt) {
		result = append(result, "sandbox_denial_effect_conflict")
	}
	if (has(FactOpenBoxDecisionBlock) || has(FactSDKAppliedBlock)) && has(FactSafeSinkReceipt) {
		result = append(result, "openbox_block_effect_conflict")
	}
	return result
}
