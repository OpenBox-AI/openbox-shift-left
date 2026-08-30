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
}

// DefaultHaltDir is where session-halt latches live: the env override, else a
// sibling of the other governance sinks (~config/openbox/halted-sessions).
func DefaultHaltDir() string {
	if p := os.Getenv(devconfig.EnvHaltDir); p != "" {
		return p
	}
	return filepath.Join(openboxConfigDir(), "halted-sessions")
}

func haltPath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(DefaultHaltDir(), sanitizeSessionID(sessionID)+"-"+hex.EncodeToString(sum[:4])+".json")
}

// WriteSessionHalt latches a session as halted. Best-effort and off the
// blocking path: the halting response is already on stdout when this runs, so
// a write fault costs only the later calls' local refusal (they fall back to a
// fresh evaluation); logged loudly, never surfaced (INV-3).
func WriteSessionHalt(logger *log.Logger, sessionID string, e client.Evaluation) {
	if sessionID == "" {
		logger.Printf("session halt latch skipped: empty session id")
		return
	}
	info := SessionHaltInfo{Reason: e.Reason, PolicyID: e.PolicyID, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	line, err := json.Marshal(info)
	if err != nil {
		logger.Printf("session halt latch skipped (marshal): %v", err)
		return
	}
	if err := os.MkdirAll(DefaultHaltDir(), 0o700); err != nil {
		logger.Printf("session halt latch skipped (mkdir): %v", err)
		return
	}
	if err := os.WriteFile(haltPath(sessionID), line, 0o600); err != nil {
		logger.Printf("session halt latch skipped (write): %v", err)
	}
}

// SessionHalted reports whether a session is latched halted, with what the
// latch preserved. A latch that exists but will not parse still halts, with a
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
		reason = "session halted by a governance HALT verdict; start a new session"
	}
	return decision.Decision{
		Evaluation:  client.Evaluation{Verdict: client.VerdictHalt, Reason: reason, PolicyID: info.PolicyID},
		Source:      SourceSessionHalt,
		SessionHalt: true,
	}
}
