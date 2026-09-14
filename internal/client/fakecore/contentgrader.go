package fakecore

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// contentFields are the three keys a tool's own words may ride on. Scoped
// deliberately: a canary that turned up in metadata instead would satisfy a
// whole-body substring search while having routed around the content gate,
// which reads these fields and not that one.
var contentFields = []string{"activity_input", "activity_output", "signal_args"}

// ContentGateGrader: the capture posture decides whether a developer's words
// leave the machine, in BOTH directions.
//
// Only the negative direction is the obvious one to write, and a suite with
// only that direction produces a system that over-captures: capture would be
// free to be broken-off and every test would still pass. The positive
// direction is what says the feature works at all.
func ContentGateGrader(canary string) Grader {
	return Grader{
		Name: "content-gate",
		// Removing the canary, not the posture. Flipping the posture would
		// move the expectation with it, since the expectation IS the posture;
		// removing the value leaves a grader still looking for it.
		Mutate: func(s Scenario) (Scenario, []string) { return StripCanary(s, canary), nil },
		Check: func(sc Scenario, run Run) []string {
			on := sc.Posture.ContentCapture == "1"
			var reasons []string

			carried := 0
			for _, r := range run.Inbox {
				whole := string(r.Raw)
				inFields := false
				for _, f := range contentFields {
					if v, ok := r.Body[f]; ok {
						b, _ := json.Marshal(v)
						if strings.Contains(string(b), canary) {
							inFields = true
							carried++
						}
					}
				}
				if !on && strings.Contains(whole, canary) {
					reasons = append(reasons, fmt.Sprintf(
						"content capture is off but a %s row carried the canary", r.EventType()))
					continue
				}
				if on && !inFields && strings.Contains(whole, canary) {
					reasons = append(reasons, fmt.Sprintf(
						"a %s row carries the canary outside %s; a content key the gate does not read routes around it",
						r.EventType(), strings.Join(contentFields, "/")))
				}
			}
			if on && carried == 0 {
				reasons = append(reasons, "content capture is on but no delivered row carried the canary in a content field; capture is not working, and the capture-off case alone would never have said so")
			}
			return reasons
		},
	}
}

// RedactionGrader: a secret is rewritten BEFORE the body is attached, so it
// reaches neither the disk nor the wire.
//
// The ordering is the only in-transit control there is. Redaction running
// after attachment would leave the wire copy holding the secret while the
// local file looked clean, and a grader checking only one half would report
// success.
func RedactionGrader(secret string) Grader {
	return Grader{
		Name: "redaction",
		// No input mutation. The gate computes its local redaction as
		// secret-detection OR content-capture, so redaction is unconditional
		// whenever a body could be attached at all: turning detection off
		// while content is captured does not disable it, and turning capture
		// off means nothing is attached for it to miss. That is a good product
		// property and it leaves this grader with nothing a scenario can
		// switch off -- so it is proven able to fail against a synthetic run
		// instead, which is honest where a mutation that cannot bite would not
		// be.
		Mutate: nil,
		Check: func(_ Scenario, run Run) []string {
			var reasons []string
			for _, r := range run.Inbox {
				if strings.Contains(string(r.Raw), secret) {
					reasons = append(reasons, fmt.Sprintf(
						"the secret reached the wire on a %s row; redaction must run before the body is attached", r.EventType()))
				}
			}
			// The disk half. Walked rather than pointed at one file: the spool,
			// the ledger and the advisory sink all live under this directory
			// and any of them holding it is the same leak.
			_ = filepath.WalkDir(run.Dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				b, readErr := os.ReadFile(p)
				if readErr != nil {
					return nil
				}
				if strings.Contains(string(b), secret) {
					rel, _ := filepath.Rel(run.Dir, p)
					// The file is named; its contents never are.
					reasons = append(reasons, "the secret was persisted to "+rel)
				}
				return nil
			})
			return reasons
		},
	}
}
