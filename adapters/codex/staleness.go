package codex

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

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// Session-start policy staleness for the Codex adapter (ported from the
// Claude Code adapter; ADR-0005). Byte-for-byte the same semantics.
//
// The in-process decider does zero network I/O; freshness is a best-effort
// client-side compare here, on SessionStart (off the tool hot path). It
// pulls the agent's current backend policy pin (id, updated_at) and
// compares it to the local bundle pin:
//
//   - all-present + match, or can't-determine (no org key in the env,
//     offline, fetch error, no local pin) → proceed on the last-good
//     bundle; never deny at fetch time.
//   - mismatch + fail-open (default) → warn (stderr + SessionStart
//     additionalContext, which Codex accepts) and proceed on the stale
//     bundle.
//   - mismatch + fail-closed → write a content-free per-session stale
//     marker; the PreToolUse enforce gate denies until `openbox dev sync`
//     clears it (Codex, like Claude Code, has no "deny a session"
//     primitive at SessionStart, so the block is realized where enforce
//     has teeth — the tool-call gate).
//
// Fully fail-safe: any error proceeds, it never writes a
// non-additionalContext SessionStart stdout in fail-open, and it never
// blocks a session.

// staleTimeout bounds the session-start policy read. Off the tool hot path but
// still snappy: a slow/unreachable backend trips this and proceeds on the last-good
// bundle.
const staleTimeout = 3 * time.Second

// checkPolicyStaleness runs the session-start compare. stdout is the SessionStart
// additionalContext channel (fail-open warning only). Best-effort; swallows every
// error.
//
// It returns its outcome so the session's posture can record it (E8-S5). The
// skip paths used to be stderr-only, which meant a session running policy of
// unknown freshness was indistinguishable from a verified-fresh one (report
// SL-03); naming which skip occurred is the whole point of the return value.
func checkPolicyStaleness(logger *log.Logger, sessionID string, stdout interface{ Write([]byte) (int, error) }) devconfig.Staleness {
	defer func() { _ = recover() }() // a fault here must never fail a session

	token := ResolveControlToken()
	backendURL := ResolveBackendURL()
	agentID := ResolveAgentID()
	localID, localUpdated, havePin := localBundlePin()

	if token == "" || backendURL == "" || agentID == "" {
		logger.Printf("staleness check skipped (missing control token, backend url, or agent id)")
		return devconfig.StalenessSkippedNoToken
	}
	if !havePin {
		logger.Printf("staleness check skipped (no local bundle pin — run `openbox dev sync`)")
		return devconfig.StalenessSkippedNoPin
	}

	backendID, backendUpdated, err := fetchPolicyPin(backendURL, token, agentID)
	if err != nil {
		logger.Printf("staleness check inconclusive (proceeding on last-good bundle): %v", err)
		return devconfig.StalenessError
	}

	if backendID == localID && backendUpdated == localUpdated {
		return devconfig.StalenessFresh
	}

	if resolveFailurePolicy() == FailClosed {
		if err := writeStaleMarker(sessionID); err != nil {
			// Even the marker is best-effort: a write failure must not
			// block the session; it degrades to "no marker" → the
			// PreToolUse gate proceeds. Report the honest outcome: policy
			// is stale and this session is NOT blocked.
			logger.Printf("staleness: could not write stale marker (session proceeds): %v", err)
			return devconfig.StalenessStaleWarned
		}
		logger.Printf("staleness: policy changed and session is fail-closed — marked stale; run `openbox dev sync`")
		return devconfig.StalenessStaleBlocked
	}

	const msg = "OpenBox policy changed since last sync — run `openbox dev sync` to refresh the local enforcement bundle."
	logger.Printf("staleness: %s", msg)
	emitAdditionalContext(stdout, msg)
	return devconfig.StalenessStaleWarned
}

// localBundlePin reads the PIN (policy_id, updated_at) from the local bundle file.
// A missing/malformed/pinless bundle yields havePin=false. No secret I/O.
func localBundlePin() (policyID, updatedAt string, havePin bool) {
	b, err := decision.LoadBundleFile(ResolveBundlePath())
	if err != nil || b == nil {
		return "", "", false
	}
	if b.PolicyID == "" && b.UpdatedAt == "" {
		return "", "", false
	}
	return b.PolicyID, b.UpdatedAt, true
}

// fetchPolicyPin reads the current backend policy PIN over the control plane
// (GET /agent/<id>/policies/current). data==null (no current policy) is an EMPTY
// pin ("",""). The org key and rego are never logged (INV-1).
func fetchPolicyPin(backendURL, token, agentID string) (policyID, updatedAt string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), staleTimeout)
	defer cancel()

	url := strings.TrimRight(backendURL, "/") + "/agent/" + agentID + "/policies/current"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
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
// stdout SessionStart is permitted to write, and only in fail-open) so the warning
// reaches the model's context. Codex accepts additionalContext on SessionStart
// (probe P2). Best-effort; content-free (INV-2).
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

// staleMarkerDir is where per-session stale markers live. Content-free files keyed
// by session id: their EXISTENCE is the signal. OPENBOX_STALE_DIR overrides (tests).
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

// staleMarkerPath maps a session id to its marker file. The session id is sanitized
// to a safe base name so a crafted id cannot escape the marker dir; an empty/
// degenerate id yields "" (no marker).
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
	return os.WriteFile(p, nil, 0o600)
}

// sessionIsStale reports whether a fail-closed stale marker exists for the
// session. A stat error other than not-exist is treated as "not stale"
// (fail-open on the marker read — never fabricate a block from an
// unrelated I/O fault).
func sessionIsStale(sessionID string) bool {
	p := staleMarkerPath(sessionID)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// clearStaleMarker removes the session's marker. Absent → no-op.
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
// denying. Absent dir → no-op. Exported for the CLI's `dev sync`.
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

// staleGateDecision returns a synthesized deny Decision when the session is marked
// stale AND the org is fail-closed — the PreToolUse realization of the SessionStart
// fail-closed staleness block. It reuses the unchanged apply cascade (a HALT verdict
// → mapVerdict deny). FailOpen is false (an intentional real deny), Source is
// content-free. It denies ONLY under fail-closed; fail-open never denies on
// staleness (it warned at SessionStart and proceeds stale). Returns (_, false) → the
// normal enforce path runs.
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
