package hookflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
)

// The rewake channel (E9 §2.2).
//
// The bounded hold covers approvers that answer in seconds. A human who does
// not is the tail the hold deliberately refuses to wait for: the call is denied
// with the reference in the reason, and the developer works elsewhere. What is
// missing at that point is the other half — telling the session when the answer
// finally lands, minutes later, so nobody has to sit and re-run the tool call to
// find out.
//
// Claude Code's `asyncRewake` handler is exactly that channel: a hook that runs
// in the BACKGROUND (it cannot block, so it cannot be the gate) and, on exit
// code 2, has its stderr shown to the model as a system reminder. So the gating
// hook and the watcher are two handlers on the same event, and they coordinate
// through one marker file:
//
//	gate: approval filed          → write the marker
//	gate: hold answered it        → remove the marker (nothing to wake for)
//	gate: hold exhausted          → leave it; deny with the reference
//	watcher: no marker within 5s  → exit 0 (this call had no approval)
//	watcher: marker, then decided → remove it and exit 2 with the outcome
//
// Removing the marker is also the claim: os.Remove succeeds for exactly one of
// the two processes, so a decision that lands in the instant between the hold's
// last poll and its timeout wakes the session once or not at all, never twice.
//
// Hosts without a rewake primitive (Codex today) keep the advisories.jsonl
// findings loop as the fallback channel — slower, but already shipped.

const (
	// rewakeMarkerGrace bounds how long the watcher waits for the gate to file
	// an approval. The escalation is a single bounded round-trip, so a marker
	// that has not appeared by now is not going to: the overwhelming majority of
	// tool calls need no approval, and this is what keeps their watcher a
	// short-lived idle process rather than one that outlives the hold.
	//
	// It must comfortably exceed the WORST-CASE time to file — a full Tier-2
	// escalation budget (adapter-clamped, 4s today) plus process startup — or
	// the watcher gives up while the gate is still escalating, and the rewake
	// silently never fires for exactly the slow-control-plane case it is most
	// wanted in. 10s is ~2.5× that worst case; the cost of the margin is a few
	// extra idle seconds in a background process nothing waits on, which is the
	// right side to be wrong on.
	//
	// That cost is paid far more often since ADR-0017. The watcher used to start
	// only for shell and MCP calls; every gated class evaluates inline now, so a
	// session doing many small tool calls holds one idle watcher per call for up
	// to this grace. The trade is unchanged in shape — an idle process nothing
	// waits on, versus an approval that can never wake the session — but it is
	// no longer a rare path, and that is the number to revisit first if watcher
	// count ever becomes the problem.
	rewakeMarkerGrace = 10 * time.Second

	// rewakePollInterval is the cadence while waiting on a human. Far slower
	// than the hold's: nobody is blocked, and the wait is minutes.
	rewakePollInterval = 5 * time.Second

	// rewakeMaxWait hard-bounds the watcher so a background process can never
	// outlive the session that spawned it by much. Core's default approval
	// window is 30 minutes; this leaves room for a longer org setting while
	// still terminating.
	rewakeMaxWait = 45 * time.Minute
)

// PendingApproval is the marker the gate leaves for the watcher. It carries no
// content (INV-2): the poll key, and the tool's name so the wake message can
// say what was being approved.
type PendingApproval struct {
	Key  client.ApprovalKey `json:"key"`
	Tool string             `json:"tool"`
}

// PendingApprovalPath is where the marker for one call lives. It is derived
// from the key, so the gate and the watcher — separate processes that never
// talk — address the same file without either scanning a directory.
func PendingApprovalPath(key client.ApprovalKey) string {
	sum := sha256.Sum256([]byte(key.WorkflowID + "\x1f" + key.RunID + "\x1f" + key.ActivityID))
	return filepath.Join(pendingApprovalDir(), hex.EncodeToString(sum[:8])+".json")
}

func pendingApprovalDir() string {
	if d := os.Getenv(devconfig.EnvPendingApprovalDir); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "pending-approvals")
}

// RecordPendingApproval marks that an approval has been filed for this call.
// Best-effort: a failure costs the rewake, never the decision, so it is logged
// and swallowed like every other off-path write.
func RecordPendingApproval(logger *log.Logger, key client.ApprovalKey, tool string) {
	raw, err := json.Marshal(PendingApproval{Key: key, Tool: CapIdent(tool)})
	if err != nil {
		logger.Printf("pending-approval marker skipped (marshal): %v", err)
		return
	}
	path := PendingApprovalPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Printf("pending-approval marker skipped: %v", err)
		return
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		logger.Printf("pending-approval marker skipped: %v", err)
	}
}

// ClaimPendingApproval removes the marker and reports whether THIS process was
// the one that removed it. Exactly one of the gate and the watcher can win, so
// an outcome is acted on once — the gate applying it to the blocked call, or
// the watcher waking the session, never both.
func ClaimPendingApproval(key client.ApprovalKey) bool {
	return os.Remove(PendingApprovalPath(key)) == nil
}

// AwaitRewake is the watcher: it waits for this call's approval to be filed and
// then decided, and returns the content-free message to wake the session with.
// ok is false when there is nothing to say — no approval was filed, the gate
// handled the outcome itself, or the window closed undecided — and the caller
// exits 0 rather than interrupting the developer.
func AwaitRewake(ctx context.Context, logger *log.Logger, key client.ApprovalKey, newClient func(*log.Logger) (Governor, error)) (string, bool) {
	if !key.Valid() {
		return "", false
	}
	marker, ok := awaitMarker(ctx, key)
	if !ok {
		return "", false // this call needed no approval — the common case
	}

	cl, err := newClient(logger)
	if err != nil {
		logger.Printf("rewake watcher stopping (client init): %v", err)
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, rewakeMaxWait)
	defer cancel()

	tick := time.NewTicker(rewakePollInterval)
	defer tick.Stop()
	for {
		st, err := cl.PollApproval(cctx, key)
		switch {
		case err == nil && st.Decided():
			if !ClaimPendingApproval(key) {
				return "", false // the gate got there first; the call already saw it
			}
			return rewakeMessage(marker.Tool, st), true
		case err == nil && windowClosed(st):
			ClaimPendingApproval(key)
			return "", false
		case err != nil && !errors.Is(err, client.ErrApprovalNotFound) && cctx.Err() == nil:
			// Same guard as the hold: the wait ending cancels the in-flight
			// poll, and that is not a fault worth reporting as one.
			logger.Printf("rewake watcher: poll degraded: %v", err)
		}
		// The gate answering removes the marker: stop rather than duplicate it.
		if _, err := os.Stat(PendingApprovalPath(key)); err != nil {
			return "", false
		}
		select {
		case <-cctx.Done():
			return "", false
		case <-tick.C:
		}
	}
}

// awaitMarker waits out the grace period for the gate to file an approval.
func awaitMarker(ctx context.Context, key client.ApprovalKey) (PendingApproval, bool) {
	deadline := time.Now().Add(rewakeMarkerGrace)
	path := PendingApprovalPath(key)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			var p PendingApproval
			if json.Unmarshal(raw, &p) == nil {
				return p, true
			}
			return PendingApproval{}, false // corrupt marker — nothing safe to say
		}
		if !time.Now().Before(deadline) {
			return PendingApproval{}, false
		}
		select {
		case <-ctx.Done():
			return PendingApproval{}, false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// rewakeMessage renders the wake text. Content-free (INV-2): the tool's name,
// the outcome, the server reference, and the policy-authored reason — the same
// class of fields the deny reason carries, never the command or file body.
//
// Telling the model to re-run is only sound because the approval key is the
// OPERATION, not the invocation: the retry addresses the same record and gets
// the decided verdict. When those two identities were conflated, this same
// instruction was an unbounded loop — the retry addressed a different record,
// filed a fresh request, and burned one human decision per pass (three attempts,
// three approval ids, no output, seen live). That is what
// TestApprovalKeyIsStableAcrossProcessesAndRetries and
// TestHighRiskClassesHaveAStableOperationID exist to keep true.
func rewakeMessage(tool string, st client.ApprovalStatus) string {
	msg := "OpenBox governance: the approval for " + OrDash(tool)
	if st.Verdict == client.VerdictAllow {
		msg += " was granted — re-run it to proceed"
	} else {
		msg += " was refused"
		if st.Reason != "" {
			msg += ": " + st.Reason
		}
	}
	if st.EventID != "" {
		msg += " (approval: " + st.EventID + ")"
	}
	return msg
}
