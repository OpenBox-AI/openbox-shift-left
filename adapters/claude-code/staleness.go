package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Session-start policy staleness (STORY-E6-S8, ADR-0005 §Decision-3).
//
// The daemon does ZERO network I/O; freshness is a best-effort CLIENT-SIDE
// compare here, on SessionStart (off the tool hot path). It pulls the agent's
// current backend policy PIN (id, updated_at) and compares it to the local
// bundle PIN:
//
//   - all-present + match, OR can't-determine (no org key in the hook env,
//     offline, fetch error, no local pin) → PROCEED on the last-good bundle;
//     NEVER deny at fetch time.
//   - mismatch + fail-open (default) → warn (stderr + SessionStart
//     additionalContext) and proceed on the STALE bundle.
//   - mismatch + fail-closed → write a content-free per-session STALE MARKER; the
//     PreToolUse enforce gate denies until `openbox dev sync` clears it (CC has no
//     "deny a session" primitive at SessionStart, so the block is realized where
//     enforce already has teeth — the tool-call gate).
//
// It is fully fail-safe: any error proceeds, and it never writes a
// non-additionalContext stdout in fail-open, and never blocks a session.

// staleTimeout bounds the session-start policy read. SessionStart runs off the
// tool hot path (INV-3b), but it must still be snappy and never hang a session
// — a slow/unreachable backend trips this and proceeds on the last-good bundle.
const staleTimeout = 3 * time.Second

// checkPolicyStaleness runs the session-start compare. stdout is the SessionStart
// additionalContext channel (fail-open warning only); a nil/failed write never
// blocks. It is best-effort and swallows every error.
func checkPolicyStaleness(logger *log.Logger, sessionID string, stdout interface{ Write([]byte) (int, error) }) {
	defer func() { _ = recover() }() // a fault here must never fail a session

	token := ResolveControlToken()
	backendURL := ResolveBackendURL()
	agentID := ResolveAgentID()
	localID, localUpdated, havePin := localBundlePin()

	// Can't-determine → proceed silently on the last-good bundle (never deny).
	if token == "" || backendURL == "" || agentID == "" || !havePin {
		logger.Printf("staleness check skipped (missing control token, backend url, agent id, or local pin)")
		return
	}

	backendID, backendUpdated, err := fetchPolicyPin(backendURL, token, agentID)
	if err != nil {
		// Offline / fetch error → proceed on the last-good bundle (ADR-0005 §Decision-3).
		logger.Printf("staleness check inconclusive (proceeding on last-good bundle): %v", err)
		return
	}

	if backendID == localID && backendUpdated == localUpdated {
		return // in sync — nothing to do
	}

	// Mismatch. Fail-open warns and proceeds; fail-closed marks the session stale.
	if resolveFailurePolicy() == FailClosed {
		if err := writeStaleMarker(sessionID); err != nil {
			// Even the marker is best-effort: a write failure must not block the
			// session. It degrades to "no marker" → the PreToolUse gate proceeds
			// (fail-open on the marker write itself), consistent with never
			// over-blocking on an infra fault (OD9).
			logger.Printf("staleness: could not write stale marker (session proceeds): %v", err)
		} else {
			logger.Printf("staleness: policy changed and session is fail-closed — marked stale; run `openbox dev sync`")
		}
		return
	}

	// Fail-open: a non-secret warning to stderr AND to the SessionStart
	// additionalContext stdout channel so it surfaces in the model's context.
	const msg = "OpenBox policy changed since last sync — run `openbox dev sync` to refresh the local enforcement bundle."
	logger.Printf("staleness: %s", msg)
	emitAdditionalContext(stdout, msg)
}

// localBundlePin reads the PIN (policy_id, updated_at) from the local bundle
// file. A missing/malformed/pinless bundle yields havePin=false (can't
// determine → proceed). No secret I/O.
func localBundlePin() (policyID, updatedAt string, havePin bool) {
	b, err := decision.LoadBundleFile(ResolveBundlePath())
	if err != nil || b == nil {
		return "", "", false
	}
	if b.PolicyID == "" && b.UpdatedAt == "" {
		return "", "", false // a no-policy/allow bundle has no pin to compare
	}
	return b.PolicyID, b.UpdatedAt, true
}

// fetchPolicyPin reads the current backend policy PIN over the control plane
// (GET /agent/<id>/policies/current). data==null (no current policy) is reported
// as an EMPTY pin ("","") — comparing it to a non-empty local pin is a genuine
// mismatch (the policy was removed). The org key and rego are never logged (INV-1).
func fetchPolicyPin(backendURL, token, agentID string) (policyID, updatedAt string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), staleTimeout)
	defer cancel()

	url := strings.TrimRight(backendURL, "/") + "/agent/" + agentID + "/policies/current"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	// Auto-classify the control credential exactly as cli/internal/backend does:
	// an obx_key_ org key → X-API-Key; anything else → Bearer JWT (+ x-openbox-client).
	if strings.HasPrefix(token, "obx_key_") {
		req.Header.Set("X-API-Key", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("x-openbox-client", "openbox-cli")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("policy read HTTP %d", resp.StatusCode)
	}
	var env struct {
		Data *struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", "", err
	}
	if env.Data == nil {
		return "", "", nil // no current policy → empty pin
	}
	return env.Data.ID, env.Data.UpdatedAt, nil
}

// emitAdditionalContext writes the SessionStart additionalContext JSON (the ONLY
// stdout SessionStart is permitted to write, and only in fail-open) so the
// warning reaches the model's context. Best-effort: a nil/failed write is
// swallowed (never blocks). Content-free — a fixed policy-refresh nudge, no tool
// content (INV-2).
func emitAdditionalContext(stdout interface{ Write([]byte) (int, error) }, msg string) {
	if stdout == nil {
		return
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     string(HookSessionStart),
			"additionalContext": msg,
		},
	}
	line, err := json.Marshal(out)
	if err != nil {
		return
	}
	_, _ = stdout.Write(append(line, '\n'))
}

// ── stale marker: the fail-closed session block realized at the PreToolUse gate ──

// staleMarkerDir is where per-session stale markers live. Content-free files
// keyed by session id: their EXISTENCE is the signal, nothing is written inside.
// OPENBOX_STALE_DIR overrides (tests). Default under the user config dir.
func staleMarkerDir() string {
	if d := os.Getenv(envStaleDir); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "stale")
}

// staleMarkerPath maps a session id to its marker file. The session id is
// sanitized to a safe base name so a crafted id cannot escape the marker dir
// (path-traversal guard); an empty/degenerate id yields "" (no marker).
func staleMarkerPath(sessionID string) string {
	safe := filepath.Base(strings.TrimSpace(sessionID))
	if safe == "" || safe == "." || safe == ".." || safe == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(staleMarkerDir(), safe)
}

// writeStaleMarker creates a content-free 0600 marker for the session (INV-2: no
// content, session id only — a structural identifier).
func writeStaleMarker(sessionID string) error {
	p := staleMarkerPath(sessionID)
	if p == "" {
		return fmt.Errorf("empty session id")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o600) // empty file — existence is the signal
}

// sessionIsStale reports whether a fail-closed stale marker exists for the
// session. A stat error other than not-exist is treated as "not stale"
// (fail-open on the marker read — never fabricate a block from an unrelated I/O
// fault; OD9).
func sessionIsStale(sessionID string) bool {
	p := staleMarkerPath(sessionID)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// clearStaleMarker removes the session's marker. Absent → no-op. Used by
// `openbox dev sync` after a successful re-pin.
func clearStaleMarker(sessionID string) error {
	p := staleMarkerPath(sessionID)
	if p == "" {
		return nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearAllStaleMarkers removes every per-session stale marker. `openbox dev sync`
// calls it after writing a fresh, re-pinned bundle so the PreToolUse gate stops
// denying and tools proceed again. Absent dir → no-op (nil). It is exported for
// the CLI's `dev sync`.
func ClearAllStaleMarkers() error {
	dir := staleMarkerDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// staleGateDecision returns a synthesized deny Decision when the session is
// marked stale AND the org is fail-closed — the PreToolUse realization of the
// SessionStart fail-closed staleness block (ADR-0005 §Decision-3). It reuses the
// unchanged apply cascade: a HALT verdict → mapVerdict deny. FailOpen is false
// (this is an intentional, real deny, not an outage fallback) and the Source is
// sourceLocalBundle-equivalent so telemetry reads it as a governed decision. The
// reason is content-free.
//
// It denies ONLY under fail-closed: fail-open never denies on staleness (it warned
// at SessionStart and proceeds stale). Returns (_, false) → the normal enforce
// path runs.
func staleGateDecision(sessionID string) (decision.Decision, bool) {
	if resolveFailurePolicy() != FailClosed || !sessionIsStale(sessionID) {
		return decision.Decision{}, false
	}
	return decision.Decision{
		Evaluation: client.Evaluation{
			Verdict: client.VerdictHalt,
			Reason:  "stale policy — run `openbox dev sync` to refresh the enforcement bundle",
		},
		FailOpen: false,
		Source:   "stale-policy",
		Stale:    true,
	}, true
}
