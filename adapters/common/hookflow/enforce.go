package hookflow

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// maxIdentLen bounds every externally-influenced identifier/path field before
// it is logged or carried, so an adversarial tool name cannot blow up a
// diagnostic line.
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
//
// Everything about the cascade except these three things is identical across
// providers: obtain a verdict, apply the failure policy, map the verdict to a
// decision, reconstruct a redacted tool input, write the response, audit it.
// What differs is only how a hook response is spelled and where a redactable
// body lives — so that is all this interface carries. Each adapter previously
// held a whole second copy of the cascade in order to express that much.
type OutputContract interface {
	// ApprovalDecision is what a REQUIRE_APPROVAL verdict becomes.
	//
	// Claude Code has a native permission prompt and maps it to `ask`. Codex
	// rejects `ask` outright, and its no-decision fallthrough auto-runs the tool
	// under approval_policy=never, so it maps to `deny` — strictly tighter,
	// never a silent proceed (OD-SL7-ASK).
	ApprovalDecision() string

	// ContentFieldKeys names the tool_input fields that may hold a redactable
	// body, in the same precedence order the adapter's own reader uses. Keeping
	// the write-back field equal to the scanned field is what makes redaction
	// reconstruction sound.
	ContentFieldKeys() []string

	// Render turns a mapped decision plus an optional redacted tool input into
	// the exact bytes to write, and reports what was applied. An empty line means
	// "write nothing" — the proceed path that stays byte-identical to observe.
	//
	// decision is "" on the proceed path; updatedInput is non-empty only when a
	// redaction was reconstructed. Implementations must never emit a response
	// that loosens what the tool would otherwise do (tighten-only).
	Render(decision, reason string, updatedInput json.RawMessage) (line []byte, applied string)
}

// ApplyResult is what the apply leg actually did, so the audit can record the
// truth instead of re-deriving it.
//
// Re-deriving was a real defect: each adapter tested a different literal for
// "this was the proceed path" — Claude Code's "" against Codex's "allow" — so
// any single predicate over those literals is wrong for one of them. The
// emitter knows what it wrote; it now says so.
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

// ApplyDecision writes the provider's hook response for a decision and reports
// what was applied. On the proceed path with nothing to say it writes nothing,
// byte-identical to observe.
//
// It never wedges a tool call: a nil stdout or any marshal/write fault degrades
// to proceed. Enforcement can only add a deny/ask/redaction — it can never hang
// or fail a call on an apply-side error (INV-3b fail-open).
func ApplyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage, c OutputContract) ApplyResult {
	if stdout == nil {
		return ApplyResult{} // fail-open: nowhere to write
	}
	d, reason := MapVerdict(dec.Evaluation, c)

	// Redaction is a proceed-path rewrite, computed only when no deny/ask is
	// being emitted — mirroring the reference SDK, which applies
	// _apply_input_redaction only after enforce_verdict returns without raising.
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
// stays small even after JSON escaping expands control bytes (up to ×6) —
// a rune cap would let an adversarial multibyte/control-heavy command
// overrun the intended bound. 8 KiB is ample: the command is only a policy
// match axis (not redacted), and Bash commands are far smaller in
// practice. Truncation can only ever cause a policy to miss a match (→
// allow), never a wrong block — consistent with fail-open. The command is
// local-only and never egressed (see HookEvent.command / INV-2).
const MaxCommandLen = 8 << 10 // 8 KiB (bytes)

// MaxRedactBody bounds the file body handed to the in-process secret
// detector for redaction. A body over this cap is not scanned (Content
// stays nil), so the tool proceeds unredacted (fail-open) rather than risk
// a slow scan on the hot path. The cap is a skip threshold, never a
// truncation: a truncated body reconstructed into updatedInput would drop
// the file's tail and corrupt the write, so we send the whole body or
// none. 512 KiB comfortably covers the .env/config/key pastes that are the
// real secret-leak surface; larger-body scanning is a noted follow-up
// (bigger local request cap or streaming scan).
const MaxRedactBody = 512 << 10 // 512 KiB (bytes)

// MaxJSONCompareBytes bounds the jsonEqual double-parse. The redacted
// input is produced in-process (bounded by MaxRedactBody), but the
// original tool_input comes from the hook payload; this defends the
// local-only equality check from an oversized document forcing a large
// re-parse. Over the cap, jsonEqual returns not-equal — the safe
// direction: a differing redaction is applied, so we only ever forgo
// suppressing an identical-but-huge rewrite (a harmless no-op), never
// corrupt or drop a real redaction. 256 KiB is ample for any real
// tool_input.
const MaxJSONCompareBytes = 256 << 10 // 256 KiB (bytes)

// RedactText redacts a content body for secrets before it is attached to an
// event, bounded exactly like the file-body path (MaxRedactBody).
//
// Over the cap the text is returned UNCHANGED rather than truncated, which is
// the same skip-not-truncate rule the file body follows and for a related
// reason: a truncated assistant message would silently misreport what the model
// said, and the cap exists to bound scan time on a hook, not to bound egress —
// capBody does that, at the client, over the whole body.
//
// The direction of that trade is worth being explicit about, because it is
// fail-open on a security control: an oversized body egresses unscanned. It is
// bounded by the same 512 KiB the file path already accepts, and the alternative
// — dropping the text — would silently disable the feature for long turns.
//
// A nil redactor is the `secret_detection:false` case and returns the text
// unchanged.
func RedactText(r *decision.Redactor, s string) string {
	if r == nil || len(s) > MaxRedactBody {
		return s
	}
	out, _, _ := r.RedactText(s)
	return out
}

// NewDecider builds the local step that runs before the evaluation: secret
// redaction, and nothing else.
//
// It used to construct the in-process policy evaluator over a signed local
// bundle. ADR-0017 made OpenBox the decider, so what remains here is content
// protection — deliberately still local, because it must run BEFORE the content
// leaves the machine and it sees the whole body where the server sees at most
// the first 64KB.
func NewDecider() decision.Decider { return decision.NewRedactor() }

// ApplyFailurePolicy is the Go analog of the SDK's _handle_api_error,
// applied between obtain and apply. It touches a decision only when the
// decider failed to deliver a real verdict (dec.FailOpen) and the org
// opted into fail-closed: it then synthesizes a HALT verdict (exactly as
// the SDK returns a synthetic Verdict.HALT) carrying a content-free
// reason, so the unchanged, policy-agnostic mapVerdict cascade denies the
// call via its normal HALT path.
//
// In every other case it returns the decision unchanged:
//   - fail-open (default): a fail-open decision stays VerdictUnknown →
//     mapVerdict emits nothing → proceed (byte-identical to observe).
//   - a real verdict (dec.FailOpen==false) under either policy: the
//     failure policy governs only the evaluation-unavailable case, never
//     a real ALLOW/CONSTRAIN/BLOCK answer — a loaded-bundle decider's
//     allow still proceeds under fail-closed.
//
// This only ever converts a would-be proceed into a deny, so it upholds
// the tighten-only invariant and INV-3b (the block is still synchronous
// and pre-execution).
//
// Note: the "no real verdict" case (dec.FailOpen) is the cold-start /
// no-policy-loaded state (Source=fail-open:no-bundle → FailOpen=true, via
// isRealVerdictSource). So a fail-closed org denies whenever OpenBox
// obtained no real verdict. This is a deliberate deviation from the
// reference SDK, which has no unbundled state and would proceed. This
// transform is unchanged — the reconciliation is entirely upstream, in
// how decision.Decision.FailOpen is set.
func ApplyFailurePolicy(dec decision.Decision, policy FailurePolicy) decision.Decision {
	if !dec.FailOpen || policy != FailClosed {
		return dec
	}
	// Synthesize the SDK's fail-closed HALT. WouldBlock() becomes true, so the
	// durable audit records a HALT with FailOpen==true — the unambiguous signature
	// of a fail-closed deny (a real HALT never carries FailOpen==true).
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = failClosedReason(dec.Evaluation.Reason)
	return dec
}

// failClosedReason builds the content-free deny reason for a fail-closed
// no-verdict case. GovReason prepends "OpenBox governance: ". The cause is
// the decider's internal diagnostic (empty on a cold-start fail-open
// today) — a fixed, content-free string, never tool content (INV-2).
func failClosedReason(cause string) string {
	r := "request denied — no governance decision could be obtained and this session is fail-closed"
	if cause != "" {
		r += " (" + cause + ")"
	}
	return r
}

// CapCommand bounds the local-only command to MaxCommandLen bytes,
// truncating at a UTF-8 rune boundary so a multibyte rune is never split
// (which would corrupt the JSON string). Bounding by bytes — not runes —
// keeps the marshaled request under the server's byte read-limit
// regardless of the command's encoding. An empty command yields ""
// (CompactAny then drops it).
func CapCommand(s string) string {
	if len(s) <= MaxCommandLen {
		return s
	}
	cut := MaxCommandLen
	// Back up off any continuation byte so we cut on a rune start.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// CompactAny drops empty-string values from an attribute map (absent-when-unknown,
// like the Mapper's compact) and returns nil when nothing is left, so the request
// carries no empty axes for a policy to spuriously not-match on.
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

// logEnforceDecision emits one terse, secret-free (INV-1) and content-free
// (INV-2) diagnostic line for an obtained enforce decision — verdict /
// source / fail_open / stale only, never the command, file path, or reason
// free text. It is the observable evidence that the sync gate ran; it goes
// to stderr (never stdout — INV-3) and never blocks. The apply path adds
// the actual apply (stdout permissionDecision) on top of this same
// Decision.
func LogEnforceDecision(logger *log.Logger, toolName string, dec decision.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN" // VerdictUnknown ("") — a fail-open / unevaluated decision
	}
	// policy is logged so a fail-closed deny (a synthesized HALT with
	// fail_open=true) is legible in the diagnostic — otherwise
	// "would_block=true fail_open=true" looks contradictory. See
	// ApplyFailurePolicy.
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t policy=%s",
		CapIdent(toolName), verdict, dec.Evaluation.WouldBlock(), OrDash(dec.Source), dec.FailOpen, policy)
}

// applyInputRedaction turns a local redaction (secret detection) into the
// Claude Code `updatedInput` to emit, or nil to emit nothing. The caller
// invokes it only on the proceed path (no deny/ask) — exactly as the
// reference SDK applies _apply_input_redaction only after enforce_verdict
// returns without raising.
//
// It returns nil (no rewrite) unless all hold:
//   - local redaction is on (secret detection [default on] or content
//     capture). Without it no tool body was ever scanned → nothing to
//     redact and the path must be byte-identical to the baseline (the
//     INV-2 gate).
//   - the Decision carries a non-empty RedactedContent.FileText.
//   - reconstructing the original tool_input with only the content field
//     replaced produces a valid object that differs from the original. A
//     no-op / unparseable original is skipped, never rewritten to garbage
//     — the analog of the SDK's "unexpected redacted_input → warn +
//     return original unchanged".
//
// The structural guarantee: the emitted object is the original tool_input
// with the single recognized content field swapped for the redacted body
// — every structural locator (file_path, …) is carried over from the
// original verbatim, never from the decision. A buggy/compromised detector
// can only change a content value; it can never add/drop/alter a
// structural field. So "content-only fields, never structural" is a
// structural property, not a promise.
//
// The returned bytes are Claude Code's full tool_input replacement.
// Content-bearing but local: it travels stdout → Claude Code on this
// machine and is never egressed (INV-2) — see
// DecisionResponse.RedactedContent.
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

// redactToolInput rebuilds a tool_input object with only the recognized
// content field replaced by redactedBody, preserving every other field
// byte-for-byte. It returns nil when the original is not a JSON object or
// carries no recognized non-empty content field (nothing safe to
// rewrite). This is where the content-only guarantee is enforced:
// structural fields are copied from the original as opaque json.RawMessage
// and are never sourced from the decision.
func RedactToolInput(origInput json.RawMessage, redactedBody string, contentFieldKeys []string) json.RawMessage {
	if len(origInput) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(origInput, &obj); err != nil || len(obj) == 0 {
		return nil // not a (non-empty) object → nothing safe to reconstruct
	}
	// Target the field fileText() actually read: the first content key holding a
	// NON-EMPTY string. This keeps the write-back field == the scanned field, so a
	// degenerate {"content":"","new_string":"<secret>"} redacts new_string (where the
	// secret is) rather than the empty content (which would leave the secret in place).
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

// jsonEqual reports whether two JSON documents are semantically equal, ignoring
// key order and insignificant whitespace (Go's json.Marshal emits map keys sorted,
// giving a canonical form). Either side unparseable ⇒ not-equal (so a redaction is
// applied rather than suppressed on an unparsable original).
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

// mapVerdict is the SDK enforce_verdict cascade ported to Claude Code
// decisions, in the same priority order. It returns the CC decision and a
// content-free reason, or ("","") meaning "emit nothing — proceed".
//
//   - HALT / BLOCK → deny (the SDK terminates / raises a non-retryable
//     block).
//   - A failed guardrail validation → deny, checked after HALT/BLOCK but
//     before approval and independent of the verdict value — exactly as
//     the SDK, so a guardrail failure is never silently swallowed by an
//     approval flow.
//   - REQUIRE_APPROVAL → ask (the SDK's requires_hitl). The reason is the
//     dedicated ApprovalReason: unlike the SDK, which registers a
//     server-side approval and polls /governance/approval across Temporal
//     retries, CC's `ask` is the interactive local prompt — the developer
//     resolves it synchronously here, so the hook's only lever is this
//     content-free reason.
//   - CONSTRAIN / ALLOW / UNKNOWN (fail-open) → nothing (the SDK logs
//     CONSTRAIN and otherwise proceeds). On this proceed path
//     applyDecision then applies guardrail input redaction
//     (applyInputRedaction → updatedInput) when content capture is on —
//     mapVerdict itself never rewrites the input.
func MapVerdict(e client.Evaluation, c OutputContract) (decision, reason string) {
	switch e.Verdict {
	case client.VerdictHalt:
		return DecisionDeny, GovReason(e, "action halted by OpenBox governance policy")
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

// ApprovalReason builds the local, content-free permissionDecisionReason
// shown on the CC `ask` prompt for a REQUIRE_APPROVAL verdict.
//
// The reference SDK treats REQUIRE_APPROVAL as an async, server-side flow:
// the interceptor sets pending_approval and raises a retryable
// ApprovalPending, then polls POST /governance/approval across Temporal
// retries until it resolves. That's deliberately rejected for the dev hot
// path — Claude Code's `ask` permissionDecision makes CC show the
// developer a native allow/deny prompt that resolves synchronously on this
// machine, so there is no poll, no expiry, no retry loop. The hook's only
// lever on that prompt is this string.
//
// It therefore surfaces the full content-free approval context the SDK
// reads off the evaluate response: the policy-authored reason (mirroring
// the SDK's "Approval required: {reason or 'Activity requires human
// approval'}") via GovReason, plus the approval reference — a server
// correlation id (same class as policy_id, already surfaced by GovReason),
// not tool content, so an approver/auditor can tie this prompt to the
// governance approval record without crossing INV-2. See
// client.Evaluation.ApprovalRef for why that reference is
// governance_event_id in practice. Shown on this machine only (stdout →
// Claude Code); never egressed.
func ApprovalReason(e client.Evaluation) string {
	msg := GovReason(e, "this action requires human approval per OpenBox governance policy")
	if ref := e.ApprovalRef(); ref != "" {
		msg += " (approval: " + ref + ")"
	}
	return msg
}

// GovReason builds the local, content-free permissionDecisionReason shown
// to the developer for a deny/ask. It surfaces the policy-authored reason
// (the bundle/OPA rule's own text, e.g. "destructive recursive delete")
// and the policy id — text authored in the policy, not derived from the
// tool command/file/output content (INV-2). It is shown on this machine
// only (stdout → Claude Code) and is never egressed. Falls back to a
// generic message when the policy carried no reason.
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

// GuardrailReason renders a guardrail-failure deny reason from the
// category types only (e.g. "[pii,secrets]") — never the guardrail reason
// free text, which can describe detected content (INV-2). Mirrors
// advisory.reasonTypes.
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
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "enforcements.jsonl")
}

// recordEnforcement appends one enforcement-decision audit line for an
// applied decision. It is the durable enforcement record — a
// same-machine, owner-only trail of what governance actually did. It is
// best-effort and off the blocking path: it runs after the stdout decision
// is already written, and any failure (marshal / mkdir / open / write) is
// logged to stderr and swallowed, never surfaced (INV-3). Content-free
// (INV-1/INV-2).
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
	// The redaction signal comes from what the apply leg actually emitted, so
	// the audit's `redacted` bool can never over-report a category hit that
	// did not reach the tool.
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

// enforcementRecord is one line in the enforcement audit sink: the
// governance decision that was actually applied to a tool call — distinct
// from an Advisory record, which captures what OpenBox would enforce on
// the observe/flush path. It is strictly content-free (INV-1/INV-2):
// verdict/ids/flags plus the guardrail category types only — never the
// tool content, the policy reason free text, or the guardrail reason free
// text. (This is deliberately stricter than AdvisoryRecord, which
// serializes the full guardrail reason struct; projecting that sink to
// categories too is a noted fast-follow.) Being category-only keeps the
// sink safe even if it's later egressed (e.g. to the dashboard) — no free
// text to leak.
type EnforcementRecord struct {
	SessionID           string           `json:"session_id"`
	ToolKind            string           `json:"tool_kind,omitempty"`
	Verdict             string           `json:"verdict"`
	WouldBlock          bool             `json:"would_block"`
	AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|ask|"" (proceed)
	Source              string           `json:"source,omitempty"`
	FailOpen            bool             `json:"fail_open"`
	Stale               bool             `json:"stale,omitempty"`
	PolicyID            string           `json:"policy_id,omitempty"`
	ApprovalRef         string           `json:"approval_ref,omitempty"` // server correlation id for a REQUIRE_APPROVAL (INV-2 safe); see client.Evaluation.ApprovalRef
	Constraints         []map[string]any `json:"constraints,omitempty"`
	GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
	// Redacted / RedactionCategories record a Tier-1 redact-and-continue:
	// whether the tool body was rewritten and which secret categories
	// fired (aws_key, entropy, …) — content-free (INV-2): category names
	// only, never the secret or the body.
	Redacted            bool     `json:"redacted,omitempty"`
	RedactionCategories []string `json:"redaction_categories,omitempty"`
}
