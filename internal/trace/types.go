// Package trace is the local, full-fidelity record of everything a shift-left
// process does on a developer machine -- proc lifecycle, hook I/O, capture
// outcomes, delivery attempts, elections, auth -- including raw bodies before
// and after redaction. It is deliberately local-only: nothing here egresses,
// so unlike the client/gateway/telemetry lanes it carries full secrets on
// disk under the 0600/0700 boundary documented in credentials-and-secrets.md.
//
// The package has zero repo imports (stdlib + github.com/gofrs/flock only) so
// any guarded subtree (internal/decision, internal/gateway,
// internal/telemetry) can be instrumented without creating an import cycle;
// callers inject a Writer or use the package-level default rather than this
// package reaching into them.
package trace

import "time"

// EnvDir is the environment variable a daemon reads to find its trace
// directory. A daemon has no $HOME, so the unit that starts it must carry
// this rather than let the daemon re-derive a path.
const EnvDir = "OPENBOX_TRACE_DIR"

// MaxBodyBytes is the per-body-field cap, chosen to match MaxRedactBody so a
// trace record never stores more of a body than the egress path already
// reasons about.
const MaxBodyBytes = 512 << 10

// RetainDays is the rotation window: today plus this many prior days are
// kept (today counts as one of them), so RetainDays=7 keeps today and the six
// days before it.
const RetainDays = 7

// Stage names. These are the vocabulary other packages emit against; treat them
// as a contract -- adding a stage is fine, renaming one is not, because a
// reader (openbox trace) and a reconciler (--against-core) match on the
// string.
const (
	StageProcStart      = "proc.start"
	StageProcExit       = "proc.exit"
	StageHookIn         = "hook.in"
	StageHookOut        = "hook.out"
	StageDecision       = "decision.local"
	StageCapture        = "capture.outcome"
	StageElection       = "election"
	StageSpoolAppend    = "spool.append"
	StageSpoolDiscard   = "spool.discard"
	StageSpoolRetire    = "spool.retire"
	StageDeliverAttempt = "deliver.attempt"
	StageDeliverResult  = "deliver.result"
	StagePoolDrop       = "pool.drop"
	StageQueueAbandon   = "queue.abandon"
	StageLatchSet       = "latch.set"
	StageDeliveryFailed = "delivery.failed"
	StageStripeWait     = "stripe.wait"
	StageUnit           = "unit"
	StageActivation     = "activation"
	StageAuth           = "auth"
	StageDoctor         = "doctor.finding"
	StageUninstall      = "uninstall.step"
	StageRelay          = "relay"
	StageLog            = "log"
	StageGateVerdict    = "gate.verdict"
)

// Record is one trace line. Emit always overwrites TS/PID/Proc regardless of
// what the caller set, so a record's provenance (when, which process) can
// never be forged by a caller that forgot to fill them in.
//
// Every identifier field is omitempty: most stages only know a handful of
// them (a proc.start knows no session yet; a hook.in knows session/run but no
// activity), and omitting the rest keeps a line legible instead of padded
// with "".
type Record struct {
	TS   time.Time `json:"ts"`
	PID  int       `json:"pid"`
	Proc string    `json:"proc"`

	Provider   string `json:"provider,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	ActivityID string `json:"activity_id,omitempty"`
	EventID    string `json:"event_id,omitempty"`
	EventType  string `json:"event_type,omitempty"`
	Lane       string `json:"lane,omitempty"`

	Stage   string `json:"stage"`
	Outcome string `json:"outcome,omitempty"`

	DurMS    float64 `json:"dur_ms,omitempty"`
	ErrClass string  `json:"err_class,omitempty"`
	Err      string  `json:"err,omitempty"`
	Attempt  int     `json:"attempt,omitempty"`

	Detail map[string]any `json:"detail,omitempty"`
}
