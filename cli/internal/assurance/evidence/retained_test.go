package evidence

import "testing"

func TestValidateRetainedJudgmentKeepsHistoricalPredicatesFailClosed(t *testing.T) {
	exploitable := []Fact{FactSDKAttemptBeforeEffect, FactPoisonMarkerProvenance, FactSafeSinkReceipt, FactCompleteObservation}
	if err := ValidateRetainedJudgment(OutcomeExploitable, exploitable, nil, nil, nil, 3); err != nil {
		t.Fatalf("valid historical predicate rejected: %v", err)
	}
	if err := ValidateRetainedJudgment(OutcomeBlocked, exploitable, nil, nil, nil, 3); err == nil {
		t.Fatal("historical predicate was relabeled")
	}
	conflicting := append(append([]Fact(nil), exploitable...), FactSafeSinkNotInvoked)
	if err := ValidateRetainedJudgment(OutcomeInconclusive, conflicting, nil, nil, []string{"unresolved"}, 3); err == nil {
		t.Fatal("derived contradiction was omitted")
	}
	if err := ValidateRetainedJudgment(OutcomeNotRunnable, nil, nil, nil, []string{"unsupported_runner"}, 0); err != nil {
		t.Fatalf("historical not-runnable predicate rejected: %v", err)
	}
}
