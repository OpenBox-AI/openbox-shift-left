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

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

const (
	// rewakeMarkerGrace bounds how long the watcher waits for the gate to file an
	// approval.
	rewakeMarkerGrace = 10 * time.Second

	rewakePollInterval = 5 * time.Second

	rewakeMaxWait = 45 * time.Minute
)

// PendingApproval is the marker the gate leaves for the watcher.
type PendingApproval struct {
	Key  client.ApprovalKey `json:"key"`
	Tool string             `json:"tool"`
}

// PendingApprovalPath is where the marker for one call lives. It is derived
// from the key, so the gate and the watcher; separate processes that never
// talk; address the same file without either scanning a directory.
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

// ClaimPendingApproval removes the marker and reports whether this process was
// the one that removed it. Exactly one of the gate and the watcher can win, so
// an outcome is acted on once; the gate applying it to the blocked call, or
// the watcher waking the session, never both.
func ClaimPendingApproval(key client.ApprovalKey) bool {
	return os.Remove(PendingApprovalPath(key)) == nil
}

// AwaitRewake is the watcher: it waits for this call's approval to be filed
// and then decided, and returns the content-free message to wake the session
// with. Ok is false when there is nothing to say; no approval was filed, the
// gate handled the outcome itself, or the window closed undecided; and the
// caller exits 0 rather than interrupting the developer.
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
			logger.Printf("rewake watcher: poll degraded: %v", err)
		}
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
// the outcome, the server reference, and the policy-authored reason; the same
// class of fields the deny reason carries, never the command or file body.
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
