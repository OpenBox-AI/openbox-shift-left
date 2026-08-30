package gitaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Advisory is the local sink for the Advisory governance tier on the deploy
// path: it records what OpenBox would enforce for a Deploy event; the verdict,
// a would_block label, and the trust/risk/guardrail/constraint signals;
// without ever gating the deploy (INV-3).
type Advisory struct {
	// Path is the jsonl sink.
	Path string
	// Log emits one terse, secret-free summary line per record. Nil ⇒ silent.
	Log Logger
}

type advisoryRecord struct {
	EventID          string                   `json:"event_id"`
	DeployID         string                   `json:"deploy_id,omitempty"`
	CommitSHA        string                   `json:"commit_sha,omitempty"`
	EventType        string                   `json:"event_type"`
	Verdict          string                   `json:"verdict"`
	WouldBlock       bool                     `json:"would_block"`
	TrustTier        string                   `json:"trust_tier,omitempty"`
	RiskScore        float64                  `json:"risk_score,omitempty"`
	Constraints      []map[string]any         `json:"constraints,omitempty"`
	GuardrailReasons []client.GuardrailReason `json:"guardrail_reasons,omitempty"`
	Timestamp        string                   `json:"ts,omitempty"`
}

// DefaultAdvisoryPath is where advisory records are written when no explicit
// path is set; the same location the adapter uses (~/.config/openbox),
// overridable with OPENBOX_ADVISORY_FILE.
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

// Record writes a deploy advisory when the evaluation is worth recording
// (Evaluation.IsAdvisory) and emits one summary line. Best-effort: any failure
// is logged and swallowed, never returned (INV-3).
func (a *Advisory) Record(ev client.DevEvent, eval client.Evaluation) {
	if a == nil || !eval.IsAdvisory() {
		return
	}
	rec := advisoryRecord{
		EventID:     ev.EventID,
		DeployID:    metaString(ev.Metadata, "deploy_id"),
		CommitSHA:   metaString(ev.Metadata, "commit_sha"),
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		a.logf("advisory mkdir failed: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.logf("advisory open failed: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		a.logf("advisory write failed for %s: %v", rec.EventID, err)
	}
}

func (a *Advisory) summary(rec advisoryRecord) {
	verdict := rec.Verdict
	if verdict == "" {
		verdict = string(client.VerdictUnknown)
	}
	a.logf("openbox-git-action: advisory recorded: deploy=%s verdict=%s would_block=%t risk=%.2f guardrails=%s constraints=%d",
		orDash(rec.DeployID), verdict, rec.WouldBlock, rec.RiskScore,
		reasonTypes(rec.GuardrailReasons), len(rec.Constraints))
}

func (a *Advisory) logf(format string, args ...any) {
	if a != nil && a.Log != nil {
		a.Log.Printf(format, args...)
	}
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func reasonTypes(reasons []client.GuardrailReason) string {
	if len(reasons) == 0 {
		return "[]"
	}
	var types []string
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
