package hookflow

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const maxIdentLen = 512

// CapIdent bounds an identifier to maxIdentLen runes.
func CapIdent(s string) string {
	r := []rune(s)
	if len(r) <= maxIdentLen {
		return s
	}
	return string(r[:maxIdentLen])
}

// OutputContract is the provider-specific half of enforcement.
type OutputContract interface {
	// ApprovalDecision is what a REQUIRE_APPROVAL verdict becomes. Codex rejects
	// `ask` outright, and its no-decision fallthrough auto-runs the tool under
	// approval_policy=never, so it maps to `deny`; strictly tighter, never a
	// silent proceed (OD-SL7-ASK).
	ApprovalDecision() string

	// ContentFieldKeys names the tool_input fields that may hold a redactable
	// body, in the same precedence order the adapter's own reader uses.
	ContentFieldKeys() []string

	// Render turns a mapped decision plus an optional redacted tool input into
	// the exact bytes to write, and reports what was applied. Implementations
	// must never emit a response that loosens what the tool would otherwise do
	// (tighten-only).
	Render(decision, reason string, updatedInput json.RawMessage) (line []byte, applied string)
}

// ApplyResult is what the apply leg actually did, so the audit can record the
// truth instead of re-deriving it.
type ApplyResult struct {
	// Decision is the provider literal that was applied; "" means proceed.
	Decision string
	// Redacted reports that a rewritten tool input was emitted.
	Redacted bool
	// Emitted reports that anything was written at all.
	Emitted bool
}

// DecisionDeny is the one decision literal every provider spells the same way.
const DecisionDeny = "deny"

// DecisionHalt is the session-terminating decision: deny this call AND stop
// the session (Claude Code: `continue:false` + the deny; a provider with no
// session-stop lever renders its strongest per-call refusal instead; Codex
// maps it to deny).
const DecisionHalt = "halt"

// ApplyDecision writes the provider's hook response for a decision and reports
// what was applied. On the proceed path with nothing to say it writes nothing,
// byte-identical to observe. It never wedges a tool call: a nil stdout or any
// marshal/write fault degrades to proceed.
func ApplyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage, c OutputContract) ApplyResult {
	if stdout == nil {
		return ApplyResult{} // fail-open: nowhere to write
	}
	d, reason := MapVerdict(dec.Evaluation, c)
	if d == DecisionHalt && !dec.SessionHalt {
		d = DecisionDeny
	}

	var updated json.RawMessage
	if d == "" {
		updated = ApplyInputRedaction(dec, localRedaction, origInput, c.ContentFieldKeys())
	}

	line, applied := c.Render(d, reason, updated)
	if len(line) == 0 {
		return ApplyResult{}
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		return ApplyResult{} // fail-open: a write fault degrades to proceed
	}
	return ApplyResult{Decision: applied, Redacted: len(updated) > 0, Emitted: true}
}

// MaxCommandLen bounds the shell command carried on the local decision
// request, measured in bytes (not runes) so the marshaled DecisionRequest
// stays small even after JSON escaping expands control bytes (up to ×6); a
// rune cap would let an adversarial multibyte/control-heavy command overrun
// the intended bound. 8 KiB is ample: the command is only a policy match axis
// (not redacted), and Bash commands are far smaller in practice.
const MaxCommandLen = 8 << 10 // 8 KiB (bytes)

// MaxRedactBody bounds the body handed to the in-process secret detector, to
// keep a slow scan off the hot path.
//   - The file path (buildDecisionRequest) skips: over the cap, Content stays
//     nil and the tool proceeds unredacted (fail-open).
//   - The telemetry path (RedactText) truncates and then scans.
const MaxRedactBody = 512 << 10 // 512 KiB (bytes)

// MaxJSONCompareBytes bounds the jsonEqual double-parse. 256 KiB is ample for
// any real tool_input.
const MaxJSONCompareBytes = 256 << 10 // 256 KiB (bytes)

// RedactText redacts a content body for secrets before it is attached to an
// event.
//   - The file path must not truncate because the redacted body is replayed
//     into the developer's actual write; a short reconstruction corrupts the
//     file.
//   - Skipping was fail-open on the one in-transit control, and it left a real
//     hole: the client caps at 65536 runes (capBody), which is at most 256 KiB
//     of UTF-8; strictly less than this 512 KiB cap.
func RedactText(r *decision.Redactor, s string) string {
	if r == nil {
		return s
	}
	if len(s) > MaxRedactBody {
		s = TruncateBytes(s, MaxRedactBody)
	}
	out, _, _ := r.RedactText(s)
	return out
}

// TruncateBytes cuts s to at most n bytes on a rune boundary, so a truncated
// body is still valid UTF-8 (a split rune would land on the wire as U+fffd and
// could break a JSON body the caller is carrying).
func TruncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// NewDecider builds the local step that runs before the evaluation: secret
// redaction, and nothing else.
func NewDecider() decision.Decider { return decision.NewRedactor() }

// ApplyFailurePolicy is the Go analog of the SDK's _handle_api_error, applied
// between obtain and apply.
//   - Fail-open (default): a fail-open decision stays VerdictUnknown →
//     mapVerdict emits nothing → proceed (byte-identical to observe).
//   - A real verdict (dec.FailOpen==false) under either policy: the failure
//     policy governs only the evaluation-unavailable case, never a real
//     ALLOW/constrain/BLOCK answer; a loaded-bundle decider's allow still
//     proceeds under fail-closed.
func ApplyFailurePolicy(dec decision.Decision, policy FailurePolicy) decision.Decision {
	if !dec.FailOpen || policy != FailClosed {
		return dec
	}
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = failClosedReason(dec.Evaluation.Reason)
	return dec
}

// failClosedReason the cause is the decider's internal diagnostic (empty on a
// cold-start fail-open today); a fixed, content-free string, never tool
// content (INV-2).
func failClosedReason(cause string) string {
	r := "request denied — no governance decision could be obtained and this session is fail-closed"
	if cause != "" {
		r += " (" + cause + ")"
	}
	return r
}

// CapCommand bounds the local-only command to MaxCommandLen bytes, truncating
// at a UTF-8 rune boundary so a multibyte rune is never split (which would
// corrupt the JSON string).
func CapCommand(s string) string {
	return TruncateBytes(s, MaxCommandLen)
}

// CompactAny drops empty-string values from an attribute map (absent-when-
// unknown, like the Mapper's compact) and returns nil when nothing is left, so
// the request carries no empty axes for a policy to spuriously not-match on.
func CompactAny(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// LogEnforceDecision emits one terse, secret-free (INV-1) and content-free
// (INV-2) diagnostic line for an obtained enforce decision; verdict / source /
// fail_open / stale only, never the command, file path, or reason free text.
func LogEnforceDecision(logger *log.Logger, toolName string, dec decision.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN" // VerdictUnknown ("") — a fail-open / unevaluated decision
	}
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t policy=%s session_halt=%t",
		CapIdent(toolName), verdict, dec.Evaluation.WouldBlock(), OrDash(dec.Source), dec.FailOpen, policy, dec.SessionHalt)
}

// ApplyInputRedaction turns a local redaction (secret detection) into the
// Claude Code `updatedInput` to emit, or nil to emit nothing.
//   - Local redaction is on (secret detection [default on] or content
//     capture).
//   - The Decision carries a non-empty RedactedContent.FileText.
//   - Reconstructing the original tool_input with only the content field
//     replaced produces a valid object that differs from the original.
func ApplyInputRedaction(dec decision.Decision, localRedaction bool, origInput json.RawMessage, contentFieldKeys []string) json.RawMessage {
	if !localRedaction {
		return nil
	}
	red := dec.RedactedContent
	if red == nil || red.FileText == "" {
		return nil
	}
	rebuilt := RedactToolInput(origInput, red.FileText, contentFieldKeys)
	if len(rebuilt) == 0 {
		return nil // no recognized content field / unparseable original → skip
	}
	if jsonEqual(rebuilt, origInput) {
		return nil // redaction changed nothing after reconstruction → no-op
	}
	return rebuilt
}

// RedactToolInput rebuilds a tool_input object with only the recognized
// content field replaced by redactedBody, preserving every other field byte-
// for-byte.
func RedactToolInput(origInput json.RawMessage, redactedBody string, contentFieldKeys []string) json.RawMessage {
	if len(origInput) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(origInput, &obj); err != nil || len(obj) == 0 {
		return nil // not a (non-empty) object → nothing safe to reconstruct
	}
	key := ""
	for _, k := range contentFieldKeys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue // absent / non-string / empty → not what fileText() would read
		}
		key = k
		break
	}
	if key == "" {
		return nil // no recognized non-empty content field to redact
	}
	val, err := json.Marshal(redactedBody)
	if err != nil {
		return nil
	}
	obj[key] = val
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > MaxJSONCompareBytes || len(b) > MaxJSONCompareBytes {
		return false // oversized → not-equal (apply the redaction); bound the re-parse
	}
	ca, err1 := canonicalJSON(a)
	cb, err2 := canonicalJSON(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// MapVerdict is the SDK enforce_verdict cascade ported to Claude Code
// decisions, in the same priority order.
//   - HALT → halt (the SDK terminates the workflow; here the provider's
//     session-stop shape; ApplyDecision downgrades it to deny unless the
//     decision is marked session-halting).
//   - BLOCK → deny (the SDK raises a non-retryable block).
//   - A failed guardrail validation → deny, checked after HALT/BLOCK but
//     before approval and independent of the verdict value; exactly as the
//     SDK, so a guardrail failure is never silently swallowed by an approval
//     flow.
func MapVerdict(e client.Evaluation, c OutputContract) (decision, reason string) {
	switch e.Verdict {
	case client.VerdictHalt:
		return DecisionHalt, GovReason(e, "action halted by OpenBox governance policy")
	case client.VerdictBlock:
		return DecisionDeny, GovReason(e, "action blocked by OpenBox governance policy")
	}
	if g := e.Guardrail; g != nil && !g.Passed {
		return DecisionDeny, GuardrailReason(g)
	}
	if e.Verdict == client.VerdictRequireApproval {
		return c.ApprovalDecision(), ApprovalReason(e)
	}
	return "", ""
}

// ApprovalReason builds the local, content-free permissionDecisionReason shown
// on the CC `ask` prompt for a REQUIRE_APPROVAL verdict.
func ApprovalReason(e client.Evaluation) string {
	msg := GovReason(e, "this action requires human approval per OpenBox governance policy")
	if ref := e.ApprovalRef(); ref != "" {
		msg += " (approval: " + ref + ")"
	}
	return msg
}

// GovReason builds the local, content-free permissionDecisionReason shown to
// the developer for a deny/ask. It is shown on this machine only (stdout →
// Claude Code) and is never egressed.
func GovReason(e client.Evaluation, fallback string) string {
	reason := e.Reason
	if reason == "" {
		reason = fallback
	}
	msg := "OpenBox governance: " + reason
	if e.PolicyID != "" {
		msg += " (policy: " + e.PolicyID + ")"
	}
	return msg
}

// GuardrailReason renders a guardrail-failure deny reason from the category
// types only (e.g. "[pii,secrets]"); never the guardrail reason free text,
// which can describe detected content (INV-2).
func GuardrailReason(g *client.GuardrailResult) string {
	return "OpenBox guardrails validation failed " + ReasonTypes(g.Reasons)
}

// DefaultEnforcementPath is the enforcement audit sink, a sibling of the
// advisory sink (~/.config/openbox/enforcements.jsonl), overridable via
// OPENBOX_ENFORCEMENT_FILE (tests point it at a temp file).
func DefaultEnforcementPath() string {
	if p := os.Getenv(devconfig.EnvEnforcementFile); p != "" {
		return p
	}
	return filepath.Join(openboxConfigDir(), "enforcements.jsonl")
}

// RecordEnforcement appends one enforcement-decision audit line for an applied
// decision. It is best-effort and off the blocking path: it runs after the
// stdout decision is already written, and any failure (marshal / mkdir / open
// / write) is logged to stderr and swallowed, never surfaced (INV-3).
func RecordEnforcement(logger *log.Logger, sessionID, toolKind string, dec decision.Decision, res ApplyResult) {
	rec := EnforcementRecord{
		SessionID:       sessionID,
		ToolKind:        toolKind,
		Verdict:         string(dec.Evaluation.Verdict),
		WouldBlock:      dec.Evaluation.WouldBlock(),
		AppliedDecision: res.Decision,
		Source:          dec.Source,
		FailOpen:        dec.FailOpen,
		PolicyID:        dec.Evaluation.PolicyID,
		ApprovalRef:     dec.Evaluation.ApprovalRef(), // correlates an ask to the governance approval; id only, no content
		Constraints:     dec.Evaluation.Constraints,
	}
	if g := dec.Evaluation.Guardrail; g != nil {
		rec.GuardrailCategories = ReasonTypeCategories(g.Reasons) // category types only (INV-2)
	}
	if res.Redacted {
		rec.Redacted = true
		rec.RedactionCategories = dec.RedactionCategories // category names only (INV-2)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		logger.Printf("enforcement record skipped (marshal): %v", err)
		return
	}
	if err := AppendJSONL(DefaultEnforcementPath(), line); err != nil {
		logger.Printf("enforcement record skipped: %v", err)
	}
}

// EnforcementRecord is one line in the enforcement audit sink: the governance
// decision that was actually applied to a tool call; distinct from an Advisory
// record, which captures what OpenBox would enforce on the observe/flush path.
type EnforcementRecord struct {
	SessionID           string           `json:"session_id"`
	ToolKind            string           `json:"tool_kind,omitempty"`
	Verdict             string           `json:"verdict"`
	WouldBlock          bool             `json:"would_block"`
	AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|ask|block|halt|"" (proceed) — the provider literal that was applied
	Source              string           `json:"source,omitempty"`
	FailOpen            bool             `json:"fail_open"`
	Stale               bool             `json:"stale,omitempty"`
	PolicyID            string           `json:"policy_id,omitempty"`
	ApprovalRef         string           `json:"approval_ref,omitempty"` // server correlation id for a REQUIRE_APPROVAL (INV-2 safe); see client.Evaluation.ApprovalRef
	Constraints         []map[string]any `json:"constraints,omitempty"`
	GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
	// Redacted / RedactionCategories record a Tier-1 redact-and-continue: whether
	// the tool body was rewritten and which secret categories fired (aws_key,
	// entropy, …); content-free (INV-2): category names only, never the secret or
	// the body.
	Redacted            bool     `json:"redacted,omitempty"`
	RedactionCategories []string `json:"redaction_categories,omitempty"`
}
