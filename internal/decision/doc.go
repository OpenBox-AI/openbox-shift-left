// Package decision is the local half of enforcement: it finds secrets in what a
// tool call is about to send and replaces them before the event leaves the
// machine.
//
// It decides nothing about policy. The control plane's /evaluate is the only
// decider, and this package's redaction runs before that call so the evaluator
// never sees a credential. There is no resident process and no socket: the scan
// is computed in-process, because a developer's Bash, Edit or Read cannot be
// blocked on a round trip.
//
// The reach is measured rather than assumed. Named formats come from gitleaks;
// beneath them sit an assignment-shape rule and an entropy floor, which are
// keyword-driven, so an unlabelled high-entropy value below the floor is
// invisible to this package. That limit is documented rather than closed:
// lowering the floor makes every git SHA and UUID a finding, and this redactor
// rewrites developer files.
package decision
