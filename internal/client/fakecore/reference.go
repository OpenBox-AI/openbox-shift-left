package fakecore

// Reference graders are the two wrong answers, kept executable.
//
// The phase that introduced pairing stated its real deliverable as two drills:
// swapping the grader for a parity check must reject the blocked session, and
// exempting every single-sided start must accept a dropped completion. Run by
// hand those are review anecdotes that rot the moment someone edits the
// grader. Registered here, CI re-proves on every run that the fixtures still
// exercise the distinction -- if a future fixture change made the two cases
// indistinguishable, the real grader could stay green while these stop
// disagreeing with it.
//
// They are never registered in the grader set; nothing grades a scenario with
// them.

// ParityPairing is the naive answer: count activity rows, demand an even
// total. It cannot name which call is wrong, and it rejects any session
// containing a legitimately blocked call.
func ParityPairing() Grader {
	return Grader{
		Name: "reference/parity",
		Check: func(_ Scenario, run Run) []string {
			n := 0
			for _, r := range run.Inbox {
				if et := r.EventType(); et == WireActivityStarted || et == WireActivityCompleted {
					n++
				}
			}
			if n%2 != 0 {
				return []string{"odd number of activity rows"}
			}
			return nil
		},
	}
}

// ExemptEverySingle is the other naive answer: treat every single-sided start
// as legitimately blocked. It accepts a session that lost a completion, which
// is the failure the suite exists to catch.
func ExemptEverySingle() Grader {
	return Grader{
		Name: "reference/exempt-all",
		Check: func(_ Scenario, run Run) []string {
			starts, completes := map[string]int{}, map[string]int{}
			for _, r := range run.Inbox {
				switch r.EventType() {
				case WireActivityStarted:
					starts[r.ActivityID()]++
				case WireActivityCompleted:
					completes[r.ActivityID()]++
				}
			}
			var reasons []string
			for id, n := range completes {
				if starts[id] == 0 {
					reasons = append(reasons, "completion with no start for "+id)
				}
				_ = n
			}
			return reasons
		},
	}
}
