package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Advisory is the local sink for the Advisory governance tier (STORY-SL-9),
// ported unchanged from the Claude Code adapter: it RECORDS what OpenBox would
// enforce for a dev-runtime event — verdict, would_block, trust/risk/guardrail
// signals — without ever blocking, delaying, or erroring the tool call (INV-3).
// It sits on the FLUSH path only, never the Pre/PostToolUse hot path.
//
// It writes the SAME default sink as the CC adapter (advisories.jsonl under the
// user config dir) BY DESIGN: the sink is developer-scoped, not tool-scoped,
// and the Tier-3 findings loop (E6-S11; SL7-B for Codex) tails one file.
// Records are metadata/categories only (INV-2): no prompt/command/patch/output
// content and never the guardrail redacted_input. Writes are best-effort.
type Advisory struct {
	// Path is the JSONL sink. Empty ⇒ DefaultAdvisoryPath().
	Path string
	// Log emits one terse, secret-free (INV-1) summary line per record to
	// stderr. nil ⇒ silent. *log.Logger satisfies it.
	Log advisoryLogger
}

type advisoryLogger interface {
	Printf(format string, args ...any)
}

// advisoryRecord is one line in the advisories sink — the STORY-SL-9 schema,
// all categories/ids/scores, no content (INV-2).
type advisoryRecord struct {
	EventID          string                   `json:"event_id"`
	SessionID        string                   `json:"session_id"`
	EventType        string                   `json:"event_type"`
	Verdict          string                   `json:"verdict"`
	WouldBlock       bool                     `json:"would_block"`
	TrustTier        string                   `json:"trust_tier,omitempty"`
	RiskScore        float64                  `json:"risk_score,omitempty"`
	Constraints      []map[string]any         `json:"constraints,omitempty"`
	GuardrailReasons []client.GuardrailReason `json:"guardrail_reasons,omitempty"`
	// DriftDetected / DriftViolations are the CONTENT-FREE goal-drift signals: a
	// boolean + a count, never the violation free text (INV-2).
	DriftDetected   bool   `json:"drift_detected,omitempty"`
	DriftViolations int    `json:"drift_violations,omitempty"`
	Timestamp       string `json:"ts,omitempty"`
}

// DefaultAdvisoryPath is where advisory records are written when no explicit
// path is set — the developer-scoped sink shared with the CC adapter.
// Overridable with OPENBOX_ADVISORY_FILE (tests point it at a temp file).
func DefaultAdvisoryPath() string {
	if p := os.Getenv("OPENBOX_ADVISORY_FILE"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "advisories.jsonl")
}

// Record writes an advisory for one evaluated event when it is worth recording
// (Evaluation.IsAdvisory) and emits one stderr summary. A trivial ALLOW — or a
// fail-open transport drop (VerdictUnknown) — records nothing. Best-effort: any
// failure is logged and swallowed, never returned (INV-3). Safe on a nil sink.
func (a *Advisory) Record(ev client.DevEvent, eval client.Evaluation) {
	if a == nil || !eval.IsAdvisory() {
		return
	}
	rec := advisoryRecord{
		EventID:     ev.EventID,
		SessionID:   ev.SessionID,
		EventType:   string(ev.EventType),
		Verdict:     string(eval.Verdict),
		WouldBlock:  eval.WouldBlock(),
		TrustTier:   eval.TrustTier,
		RiskScore:   eval.RiskScore,
		Constraints: eval.Constraints,
		Timestamp:   ev.Timestamp,
	}
	if eval.Guardrail != nil {
		rec.GuardrailReasons = eval.Guardrail.Reasons
	}
	if eval.Drift.Detected() {
		rec.DriftDetected = true
		rec.DriftViolations = eval.Drift.ViolationsCount
	}
	a.write(rec)
	a.summary(rec)
}

func (a *Advisory) write(rec advisoryRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		a.logf("advisory marshal failed for %s: %v", rec.EventID, err)
		return
	}
	path := a.Path
	if path == "" {
		path = DefaultAdvisoryPath()
	}
	if err := appendJSONL(path, line); err != nil {
		a.logf("advisory write failed for %s: %v", rec.EventID, err)
	}
}

// appendJSONL appends one JSON line to a same-machine, owner-only JSONL sink,
// creating the directory (0700) and file (0600) as needed. A single small
// O_APPEND write is atomic under POSIX.
func appendJSONL(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// summary logs one line describing the recorded advisory — categories/ids/
// scores only (INV-1/INV-2), never guardrail free text or tool content.
func (a *Advisory) summary(rec advisoryRecord) {
	verdict := rec.Verdict
	if verdict == "" {
		verdict = string(client.VerdictUnknown)
	}
	a.logf("advisory recorded: event=%s type=%s verdict=%s would_block=%t risk=%.2f trust_tier=%s guardrails=%s constraints=%d drift=%t violations=%d",
		rec.EventID, rec.EventType, verdict, rec.WouldBlock, rec.RiskScore,
		orDash(rec.TrustTier), reasonTypes(rec.GuardrailReasons), len(rec.Constraints),
		rec.DriftDetected, rec.DriftViolations)
}

func (a *Advisory) logf(format string, args ...any) {
	if a != nil && a.Log != nil {
		a.Log.Printf(format, args...)
	}
}

// reasonTypes renders the guardrail reason CATEGORY types (the `type` field,
// e.g. "pii") as "[pii,validation]" — NEVER the reason free text or field name,
// which can describe detected content (INV-2).
func reasonTypes(reasons []client.GuardrailReason) string {
	if len(reasons) == 0 {
		return "[]"
	}
	types := make([]string, 0, len(reasons))
	for _, r := range reasons {
		t := r.Type
		if t == "" {
			t = "?"
		}
		types = append(types, t)
	}
	return "[" + strings.Join(types, ",") + "]"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
