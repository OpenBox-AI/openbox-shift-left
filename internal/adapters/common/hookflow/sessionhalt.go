package hookflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// SourceSessionHalt the session-halt latch. The provider's strongest in-
// contract response stops only the current turn, so "cannot continue" needs
// durable state: one small file per halted session.
const SourceSessionHalt = "session-halt"

// SessionHaltInfo is what the latch preserves from the halting verdict: enough
// to re-render an honest refusal on every later call. Reason is the policy-
// authored text (the same string already shown locally on the halting deny;
// INV-2 class unchanged); the latch never stores tool content.
type SessionHaltInfo struct {
	Reason   string `json:"reason,omitempty"`
	PolicyID string `json:"policy_id,omitempty"`
	TS       string `json:"ts"`
	// Cause names the delivery failure class (client.FailureClass, or
	// "orphaned" for a drainer that died mid-delivery) when this latch's
	// origin was an unaccepted event rather than a HALT verdict; empty for a
	// verdict-originated latch. Additive: an old reader ignores it.
	Cause string `json:"cause,omitempty"`
	// EventType names the event OpenBox could not record, when this latch's
	// origin was a delivery failure; empty for a verdict-originated latch.
	// Additive: an old reader ignores it.
	EventType string `json:"event_type,omitempty"`
}

// DefaultHaltDir is where session-halt latches live: the env override, else a
// sibling of the other governance sinks (~config/openbox/halted-sessions).
func DefaultHaltDir() string {
	if p := os.Getenv(devconfig.EnvHaltDir); p != "" {
		return p
	}
	return filepath.Join(openboxConfigDir(), "halted-sessions")
}

// haltPath, WriteSessionHalt and SessionHalted all take a RUN id, not a
// session id, though the parameter is untyped and the name
// below stays "sessionID" for every existing caller and test: at generation
// 0 the run id IS the session id (client.runIDFor's own selection), so every
// caller that predates continue-as-new is unaffected and every existing
// latch fixture is byte-identical. A `/clear` or `--resume` that bumped the
// run gets a DIFFERENT id here -- core's own HALT is scoped per
// (workflow_id, run_id) row, and this latch now matches that scope: a
// continued run starts unlatched, and core re-evaluates its first gated call
// against the same policies (a policy that halted the old run halts the new
// one on the same condition). No remove path is added, and none should be:
// "presence is the decided state" stays true for the run it names.
func haltPath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(DefaultHaltDir(), sanitizeSessionID(sessionID)+"-"+hex.EncodeToString(sum[:4])+".json")
}

// WriteSessionHalt latches a run as halted (see the run-vs-session-id note
// above). Best-effort and off the blocking path: the halting response is
// already on stdout when this runs, so a write fault costs only the later
// calls' local refusal (they fall back to a fresh evaluation); logged
// loudly, never surfaced (INV-3). The write goes through atomicWriteFile,
// not os.WriteFile: lane daemons and hook flushers can latch the same run
// concurrently, and a reader (SessionHalted) must never observe a
// partially-written file.
func WriteSessionHalt(logger *log.Logger, sessionID string, e client.Evaluation) {
	info := SessionHaltInfo{Reason: e.Reason, PolicyID: e.PolicyID, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	path, line, ok := prepareLatchWrite(logger, sessionID, info)
	if !ok {
		return
	}
	if err := atomicWriteFile(path, line, 0o600); err != nil {
		logger.Printf("session halt latch skipped (write): %v", err)
		return
	}
	traceLatchSet(sessionID, info)
}

// WriteSessionHaltIfAbsent latches a run as halted, but ONLY when it is not
// already latched: an existing latch -- whichever cause reached it first, a
// live HALT verdict (WriteSessionHalt) or an earlier delivery failure -- is
// left exactly as it is. A run's first cause is the one every later call
// sees, never replaced by a second, unrelated one.
//
// O_CREATE|O_EXCL makes "is it latched, then write" one atomic filesystem
// step, closing the race two drainers (or a drainer and a gate escalation)
// hitting the same run's very first failure concurrently would otherwise
// have: each observing no latch, then both writing. Deliberately not
// atomicWriteFile (rename-into-place): that mechanism always replaces
// whatever is at the destination, which is the one thing this function must
// never do.
//
// This create-then-write is NOT the same guarantee WriteSessionHalt's doc
// comment describes: between the O_CREATE|O_EXCL and the Write call below, a
// concurrent reader can observe a present but EMPTY (0-byte) file. "Never
// observed partially written" is WriteSessionHalt's own property (rename-
// into-place), not this function's. This is accepted, not a bug: presence is
// still the decided state (SessionHalted's own contract), an empty file
// fails json.Unmarshal exactly like a corrupt one, and every reader already
// treats that the same way -- SessionHalted still reports halted=true (with
// a zero-value SessionHaltInfo), SessionHaltDecision still renders a
// generic, non-empty reason, and doctor still counts and shows it as
// "(unreadable)" rather than a blank line or a crash.
//
// Best-effort and off the blocking path, same as WriteSessionHalt: a write
// fault here costs only a later call's local refusal (INV-3).
func WriteSessionHaltIfAbsent(logger *log.Logger, sessionID string, info SessionHaltInfo) {
	path, line, ok := prepareLatchWrite(logger, sessionID, info)
	if !ok {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			logger.Printf("session halt latch skipped (write): %v", err)
		}
		return // already latched, by this cause or an earlier one: first wins
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		logger.Printf("session halt latch skipped (write): %v", err)
		return
	}
	traceLatchSet(sessionID, info)
}

// prepareLatchWrite is the marshal-and-mkdir prelude WriteSessionHalt and
// WriteSessionHaltIfAbsent share; only the final write step (rename-into-
// place vs. O_CREATE|O_EXCL) differs between them, and stays with each.
func prepareLatchWrite(logger *log.Logger, sessionID string, info SessionHaltInfo) (path string, line []byte, ok bool) {
	if sessionID == "" {
		logger.Printf("session halt latch skipped: empty session id")
		return "", nil, false
	}
	line, err := json.Marshal(info)
	if err != nil {
		logger.Printf("session halt latch skipped (marshal): %v", err)
		return "", nil, false
	}
	if err := os.MkdirAll(DefaultHaltDir(), 0o700); err != nil {
		logger.Printf("session halt latch skipped (mkdir): %v", err)
		return "", nil, false
	}
	return haltPath(sessionID), line, true
}

// SessionHalted reports whether a run is latched halted (see the
// run-vs-session-id note above WriteSessionHalt), with what the latch
// preserved. A latch that exists but will not parse still halts, with a
// generic reason: presence is the decided state, and a corrupt file must not
// quietly un-halt a session the control plane terminated.
func SessionHalted(sessionID string) (SessionHaltInfo, bool) {
	if sessionID == "" {
		return SessionHaltInfo{}, false
	}
	raw, err := os.ReadFile(haltPath(sessionID))
	if err != nil {
		return SessionHaltInfo{}, false
	}
	var info SessionHaltInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return SessionHaltInfo{}, true
	}
	return info, true
}

// ReplaySessionHalt renders a latched session's halt onto the current gated
// hook: the preserved HALT is logged, applied through the provider's contract
// (its session-stop shape; deny/block plus the stop) and recorded in the
// enforcement audit, with NO server round-trip: the latch is the decided
// state, and re-evaluating a halted session would be asking the question
// governance already answered.
func ReplaySessionHalt(logger *log.Logger, stdout io.Writer, info SessionHaltInfo, sessionID, toolName, toolKind string, c OutputContract) {
	dec := SessionHaltDecision(info)
	LogEnforceDecision(logger, toolName, dec, ResolveFailurePolicy())
	res := ApplyDecision(stdout, dec, false, nil, c)
	RecordEnforcement(logger, sessionID, toolKind, dec, res)
}

// SessionHaltDecision rebuilds the decision a latched session replays onto a
// later call: the preserved HALT, marked SessionHalt so the apply cascade
// renders the provider's session-stop shape again rather than a per-call deny.
func SessionHaltDecision(info SessionHaltInfo) decision.Decision {
	reason := info.Reason
	if reason == "" {
		// Generic and origin-agnostic on purpose: an empty Reason reaches
		// here for BOTH a latch this process cannot parse (corrupt, or
		// caught mid-write in WriteSessionHaltIfAbsent's own create-then-
		// write window -- see its doc comment) and, in principle, any future
		// writer that latched without one; the origin (a HALT verdict, or an
		// unaccepted event) cannot be recovered here either way, and must
		// never be guessed at.
		reason = "session halted; the run cannot continue. Start a new session to continue."
	}
	return decision.Decision{
		Evaluation:  client.Evaluation{Verdict: client.VerdictHalt, Reason: reason, PolicyID: info.PolicyID},
		Source:      SourceSessionHalt,
		SessionHalt: true,
	}
}
