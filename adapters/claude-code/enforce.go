package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Enforcement — the synchronous pre-execution gate.
//
// In enforce mode (ResolveEnforce), a PreToolUse hook must obtain a
// governance decision from the local decision engine before the tool runs
// — the INV-3b carve-out to INV-3 ("observation never blocks"): an enforce
// path may block, but only pre-execution and fail-open by default. This
// mirrors the reference SDK's activity-boundary gate, which awaits
// GovernanceClient.evaluate_event on ActivityStarted and then runs
// enforce_verdict before the activity executes. The decisive difference: a
// synchronous round-trip to core's /evaluate is ~0.8-1.6s (a Temporal
// workflow) — far over budget — so the decision is computed in-process
// from a synced local policy bundle (microseconds, no socket, no daemon;
// ADR-0006), never a network call.
//
// Scope of this file: obtain + record the decision only. It returns the
// decision.Decision (carrying the client.Evaluation) and never writes a
// blocking signal — turning a BLOCK/HALT verdict into an actual Claude
// Code `deny`/`ask` (the enforce_verdict cascade) is the apply path, which
// consumes this Decision. So enforce mode here is safe by construction: the
// tool always proceeds, exactly as observe mode does, while the sync path
// + fail-open + latency bound are exercised and validated.

// maxCommandLen bounds the shell command carried on the local decision
// request, measured in bytes (not runes) so the marshaled DecisionRequest
// stays small even after JSON escaping expands control bytes (up to ×6) —
// a rune cap would let an adversarial multibyte/control-heavy command
// overrun the intended bound. 8 KiB is ample: the command is only a policy
// match axis (not redacted), and Bash commands are far smaller in
// practice. Truncation can only ever cause a policy to miss a match (→
// allow), never a wrong block — consistent with fail-open. The command is
// local-only and never egressed (see HookEvent.command / INV-2).
const maxCommandLen = 8 << 10 // 8 KiB (bytes)

// maxRedactBody bounds the file body handed to the in-process secret
// detector for redaction. A body over this cap is not scanned (Content
// stays nil), so the tool proceeds unredacted (fail-open) rather than risk
// a slow scan on the hot path. The cap is a skip threshold, never a
// truncation: a truncated body reconstructed into updatedInput would drop
// the file's tail and corrupt the write, so we send the whole body or
// none. 512 KiB comfortably covers the .env/config/key pastes that are the
// real secret-leak surface; larger-body scanning is a noted follow-up
// (bigger local request cap or streaming scan).
const maxRedactBody = 512 << 10 // 512 KiB (bytes)

// maxJSONCompareBytes bounds the jsonEqual double-parse. The redacted
// input is produced in-process (bounded by maxRedactBody), but the
// original tool_input comes from the hook payload; this defends the
// local-only equality check from an oversized document forcing a large
// re-parse. Over the cap, jsonEqual returns not-equal — the safe
// direction: a differing redaction is applied, so we only ever forgo
// suppressing an identical-but-huge rewrite (a harmless no-op), never
// corrupt or drop a real redaction. 256 KiB is ample for any real
// tool_input.
const maxJSONCompareBytes = 256 << 10 // 256 KiB (bytes)

// EnforceDecision is the PreToolUse enforce gate: it synchronously obtains
// a governance decision from the in-process decider for the tool that is
// about to run. It never errors and never blocks — the decider fails open
// (VerdictUnknown/allow) on any fault (no bundle loaded, unusable
// request), so the returned Decision is always safe to proceed on. The
// returned Decision is the seam the apply path consumes to map the
// verdict onto a Claude Code permissionDecision.
//
// It reads no secret (identity is the DID only, already resolved on the
// hot path — INV-1) and takes no network I/O and no IPC (evaluated
// in-memory — INV-3b).
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
}

// newDecider builds the enforce-hook decision transport. There is exactly
// one (ADR-0006): the local bundle is evaluated in-process — no resident
// daemon, no socket, nothing for the developer to start. The evaluator is
// pure-Go and in-memory, so the hook (a short-lived per-tool-call process)
// loads the same bundle `openbox dev sync` wrote and decides directly. It
// fails open on any fault (absent/unreadable bundle → cold-start
// fail-open), so an infra failure never blocks the developer (INV-3b).
// This is what makes enforcement ambient after `openbox dev init` with
// zero runtime setup.
func newDecider() decision.Decider {
	return decision.NewInProcessDecider(decision.InProcessConfig{
		BundlePath: ResolveBundlePath(),
	})
}

// ── Fail-open / fail-closed failure policy ───────────────────────────────
//
// The failure policy decides what the enforce gate does when the
// in-process decider could not deliver a real verdict (no policy bundle
// loaded, or an unusable request — i.e. decision.Decision.FailOpen==true).
// It is the Go port of the reference SDK's governance_policy /
// _handle_api_error: on an evaluate failure the SDK returns either None
// (fail-open → no verdict → the action proceeds) or a synthesized
// Verdict.HALT (fail-closed → the same enforce_verdict cascade runs → the
// action is blocked). We mirror that shape exactly so the apply cascade
// (mapVerdict/applyDecision) stays entirely policy-agnostic — a
// fail-closed deny travels the identical path as a real BLOCK.

// FailurePolicy is the per-org enforce failure posture. FailOpen is the
// zero value and the default: an OpenBox outage degrades to observe
// (proceed).
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure — the tool
	// proceeds (default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies the tool call on an evaluation failure (explicit
	// per-org opt-in). An OpenBox outage blocks work rather than letting
	// it through ungoverned.
	FailClosed
)

func (p FailurePolicy) String() string {
	if p == FailClosed {
		return "fail_closed"
	}
	return "fail_open"
}

// resolveFailurePolicy reads the configured failure posture (ResolveFailClosed).
func resolveFailurePolicy() FailurePolicy {
	if ResolveFailClosed() {
		return FailClosed
	}
	return FailOpen
}

// applyFailurePolicy is the Go analog of the SDK's _handle_api_error,
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
func applyFailurePolicy(dec decision.Decision, policy FailurePolicy) decision.Decision {
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
// no-verdict case. govReason prepends "OpenBox governance: ". The cause is
// the decider's internal diagnostic (empty on a cold-start fail-open
// today) — a fixed, content-free string, never tool content (INV-2).
func failClosedReason(cause string) string {
	r := "request denied — no governance decision could be obtained and this session is fail-closed"
	if cause != "" {
		r += " (" + cause + ")"
	}
	return r
}

// buildDecisionRequest assembles the local decision request from a
// PreToolUse payload, reusing the Mapper's tool classification
// (classifyTool / filePath) so the enforce gate and the observe event
// classify a tool identically. It carries the metadata axes a local
// policy matches on — tool name/kind, MCP server, file path/operation,
// permission mode, and (local-only, never egressed) the shell command.
//
// Content (INV-2) is populated when localRedaction is true — i.e. Tier-1
// secret detection (default on) or content capture is on. The local
// decider needs the tool's body to scan/redact, the analog of the
// reference SDK sending the full activity_input to its gate. Like the
// command axis it stays in-process and is never egressed (the observe
// Mapper egress path is unchanged, still metadata-only unless content
// capture is on). With both off, Content stays nil and the request is
// byte-identical to the fail-closed-only baseline.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	// Metadata axes only (INV-2). compact drops empty values so a rule matching on
	// an absent attribute fails to match rather than matching "".
	attrs := map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	}
	switch {
	case isFileSemantic(sem):
		attrs["file_path"] = capStr(e.filePath()) // structural locator (INV-2)
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// Local-only: the command is the axis a policy matches a dangerous shell
		// action on. It goes ONLY to the in-process decider and is never egressed/logged
		// (HookEvent.command). Bounded to keep the local request small.
		attrs["command"] = capCommand(e.command())
	}

	req := decision.DecisionRequest{
		Protocol:     decision.ProtocolVersion,
		SessionID:    e.SessionID,
		DeveloperDID: id.DeveloperDID,
		EventType:    client.EventToolCall, // the pre-execution gate is a ToolCall decision
		Tool:         tool,
		Attributes:   compactAny(attrs),
	}

	// Content is GATED on localRedaction and LOCAL-only (INV-2). Only the file BODY
	// is carried (the redactable tool input the secret detector scans). A body over
	// maxRedactBody is NOT sent — the tool proceeds unredacted (fail-open) rather
	// than be truncated (a truncated reconstruction would corrupt the write). Left
	// nil for a non-file tool, an empty body, or an oversized body.
	if localRedaction && isFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= maxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
}

// capCommand bounds the local-only command to maxCommandLen bytes,
// truncating at a UTF-8 rune boundary so a multibyte rune is never split
// (which would corrupt the JSON string). Bounding by bytes — not runes —
// keeps the marshaled request under the server's byte read-limit
// regardless of the command's encoding. An empty command yields ""
// (compactAny then drops it).
func capCommand(s string) string {
	if len(s) <= maxCommandLen {
		return s
	}
	cut := maxCommandLen
	// Back up off any continuation byte so we cut on a rune start.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// compactAny drops empty-string values from an attribute map (absent-when-unknown,
// like the Mapper's compact) and returns nil when nothing is left, so the request
// carries no empty axes for a policy to spuriously not-match on.
func compactAny(m map[string]any) map[string]any {
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
func logEnforceDecision(logger *log.Logger, e *HookEvent, dec decision.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN" // VerdictUnknown ("") — a fail-open / unevaluated decision
	}
	// policy is logged so a fail-closed deny (a synthesized HALT with
	// fail_open=true) is legible in the diagnostic — otherwise
	// "would_block=true fail_open=true" looks contradictory. See
	// applyFailurePolicy.
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t stale=%t policy=%s",
		capStr(e.ToolName), verdict, dec.Evaluation.WouldBlock(), orDash(dec.Source), dec.FailOpen, dec.Stale, policy)
}

// ── apply(verdict) — the enforce leg's teeth ─────────────────────────────
//
// EnforceDecision obtains a decision.Decision; this section applies it —
// mapping the governance verdict onto a Claude Code PreToolUse
// `permissionDecision` written to stdout, the moment WouldBlock() becomes a
// real block. This ports the reference SDK's enforce_verdict cascade — the
// full priority set HALT > BLOCK > guardrails > REQUIRE_APPROVAL >
// CONSTRAIN > ALLOW — onto Claude Code's hook contract:
//
//	SDK enforce_verdict                        →  CC PreToolUse permissionDecision
//	───────────────────────────────────────      ────────────────────────────────
//	HALT  → GovernanceHaltError (terminate)    →  deny  (strongest CC signal)
//	BLOCK → GovernanceBlockedError             →  deny
//	guardrails validation_passed == false      →  deny  (checked BEFORE approval)
//	REQUIRE_APPROVAL → requires_hitl            →  ask   (OD-HITL; E6-S6 approval reason)
//	CONSTRAIN        → logged allow             →  (nothing — proceed)
//	ALLOW / UNKNOWN (fail-open)                 →  (nothing — proceed)
//
// Invariant — governance only tightens. A non-blocking verdict writes
// nothing to stdout, so Claude Code's own permission flow is left
// untouched and behaves exactly as in observe mode. Only `deny`/`ask` are
// ever emitted — enforcement can add a restriction, never remove one of
// Claude Code's built-in prompts. This upholds INV-3b (blocks only
// pre-execution, within the timeout bound) and keeps the observe/advisory
// path byte-identical when nothing is blocked.

// Claude Code PreToolUse permissionDecision values (the hook stdout contract).
// Only deny/ask are emitted; allow is intentionally never written (tighten-only).
const (
	ccDecisionDeny = "deny"
	ccDecisionAsk  = "ask"
)

// preToolUseOutput is the Claude Code PreToolUse hook stdout contract: an
// exit-0 hook that prints this JSON has its permissionDecision honored —
// `deny` blocks the tool call (Claude sees the reason), `ask` shows the
// user a permission prompt. permissionDecisionReason is shown locally
// (stdout → Claude Code on the same machine, no egress) and carries the
// policy-authored reason, never the tool command/file/output content
// (INV-2).
type preToolUseOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input. Claude Code
	// treats it as a full replacement of tool_input, applied before the
	// tool runs. Reconstructed from the original tool_input with only the
	// content field swapped (redactToolInput) — never sourced whole from
	// the decision. Emitted alone on the proceed path (no
	// permissionDecision) so CC's own permission flow still applies.
	// omitempty ⇒ absent on the deny/ask paths and whenever there is no
	// redaction. Content-bearing but local (stdout → CC on this machine,
	// never egressed — INV-2).
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

// applyDecision maps an obtained enforce decision onto a Claude Code
// PreToolUse hook output and writes it to stdout. It returns the applied
// CC decision (`deny`/`ask`, or "" on proceed) and whether anything was
// emitted.
//
// Two levers, in the SDK's own order:
//   - mapVerdict yields deny/ask → emit permissionDecision.
//   - else (the proceed path — CONSTRAIN/ALLOW/UNKNOWN) → apply input
//     redaction: emit `updatedInput` alone (no permissionDecision), so
//     Claude Code's own permission flow still applies. This mirrors the
//     SDK, which runs _apply_input_redaction only after enforce_verdict
//     returns without raising. On deny the tool never runs; on ask the SDK
//     raises before redaction, so neither rewrites (ask-path redaction is
//     a deferred consideration).
//
// Tighten-only is preserved: stdout carries only deny/ask or a
// content-stripping updatedInput — never permissionDecision:allow. When
// nothing applies (no deny/ask and no redaction — e.g. both secret
// detection and content capture off) it writes nothing, byte-identical to
// observe.
//
// It never wedges the tool call: a nil stdout or any marshal/write fault
// degrades to "proceed" (fail-open) — enforcement can only add a
// deny/ask/redaction, never hang or fail a call on an apply-side error
// (INV-3b fail-open).
func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	if stdout == nil {
		return "", false // fail-open: nowhere to write
	}
	decision, reason := mapVerdict(dec.Evaluation)
	hso := hookSpecificOutput{
		HookEventName:            string(HookPreToolUse),
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}
	if decision == "" {
		hso.UpdatedInput = applyInputRedaction(dec, localRedaction, origInput)
	}
	if hso.PermissionDecision == "" && len(hso.UpdatedInput) == 0 {
		return "", false // proceed, nothing to say → write nothing (E6-S3 identical)
	}
	line, err := json.Marshal(preToolUseOutput{HookSpecificOutput: hso})
	if err != nil {
		return "", false // fail-open: never wedge a tool call on a marshal fault
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		return "", false // fail-open: a write fault degrades to proceed
	}
	return decision, true
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
func applyInputRedaction(dec decision.Decision, localRedaction bool, origInput json.RawMessage) json.RawMessage {
	if !localRedaction {
		return nil
	}
	red := dec.RedactedContent
	if red == nil || red.FileText == "" {
		return nil
	}
	rebuilt := redactToolInput(origInput, red.FileText)
	if len(rebuilt) == 0 {
		return nil // no recognized content field / unparseable original → skip
	}
	if jsonEqual(rebuilt, origInput) {
		return nil // redaction changed nothing after reconstruction → no-op
	}
	return rebuilt
}

// contentFieldKeys are the tool_input keys that carry a redactable body, in
// the same precedence HookEvent.fileText() reads them: Write's "content",
// then Edit's "new_string". redactToolInput swaps only the first key
// holding a non-empty string (mirroring fileText() exactly, so the field
// written back is the field that was scanned); every other key (file_path
// and any structural locator) is preserved verbatim. Extending this to
// MultiEdit's edits[].new_string[] is a noted follow-up (under-capture is
// INV-2-safe — nothing extra to redact).
var contentFieldKeys = []string{"content", "new_string"}

// redactToolInput rebuilds a tool_input object with only the recognized
// content field replaced by redactedBody, preserving every other field
// byte-for-byte. It returns nil when the original is not a JSON object or
// carries no recognized non-empty content field (nothing safe to
// rewrite). This is where the content-only guarantee is enforced:
// structural fields are copied from the original as opaque json.RawMessage
// and are never sourced from the decision.
func redactToolInput(origInput json.RawMessage, redactedBody string) json.RawMessage {
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
	if len(a) > maxJSONCompareBytes || len(b) > maxJSONCompareBytes {
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
//     dedicated approvalReason: unlike the SDK, which registers a
//     server-side approval and polls /governance/approval across Temporal
//     retries, CC's `ask` is the interactive local prompt — the developer
//     resolves it synchronously here, so the hook's only lever is this
//     content-free reason.
//   - CONSTRAIN / ALLOW / UNKNOWN (fail-open) → nothing (the SDK logs
//     CONSTRAIN and otherwise proceeds). On this proceed path
//     applyDecision then applies guardrail input redaction
//     (applyInputRedaction → updatedInput) when content capture is on —
//     mapVerdict itself never rewrites the input.
func mapVerdict(e client.Evaluation) (decision, reason string) {
	switch e.Verdict {
	case client.VerdictHalt:
		return ccDecisionDeny, govReason(e, "action halted by OpenBox governance policy")
	case client.VerdictBlock:
		return ccDecisionDeny, govReason(e, "action blocked by OpenBox governance policy")
	}
	if g := e.Guardrail; g != nil && !g.Passed {
		return ccDecisionDeny, guardrailReason(g)
	}
	if e.Verdict == client.VerdictRequireApproval {
		return ccDecisionAsk, approvalReason(e)
	}
	return "", ""
}

// approvalReason builds the local, content-free permissionDecisionReason
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
// approval'}") via govReason, plus e.ApprovalID — the one
// approval-specific field on GovernanceVerdictResponse. The approval id is
// a server correlation id (same class as policy_id / governance_event_id,
// already surfaced by govReason), not tool content, so an approver/auditor
// can tie this prompt to the governance approval record without crossing
// INV-2. Shown on this machine only (stdout → Claude Code); never
// egressed.
func approvalReason(e client.Evaluation) string {
	msg := govReason(e, "this action requires human approval per OpenBox governance policy")
	if e.ApprovalID != "" {
		msg += " (approval: " + e.ApprovalID + ")"
	}
	return msg
}

// govReason builds the local, content-free permissionDecisionReason shown
// to the developer for a deny/ask. It surfaces the policy-authored reason
// (the bundle/OPA rule's own text, e.g. "destructive recursive delete")
// and the policy id — text authored in the policy, not derived from the
// tool command/file/output content (INV-2). It is shown on this machine
// only (stdout → Claude Code) and is never egressed. Falls back to a
// generic message when the policy carried no reason.
func govReason(e client.Evaluation, fallback string) string {
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

// guardrailReason renders a guardrail-failure deny reason from the
// category types only (e.g. "[pii,secrets]") — never the guardrail reason
// free text, which can describe detected content (INV-2). Mirrors
// advisory.reasonTypes.
func guardrailReason(g *client.GuardrailResult) string {
	return "OpenBox guardrails validation failed " + reasonTypes(g.Reasons)
}

// enforcementRecord is one line in the enforcement audit sink: the
// governance decision that was actually applied to a tool call — distinct
// from an Advisory record, which captures what OpenBox would enforce on
// the observe/flush path. It is strictly content-free (INV-1/INV-2):
// verdict/ids/flags plus the guardrail category types only — never the
// tool content, the policy reason free text, or the guardrail reason free
// text. (This is deliberately stricter than advisoryRecord, which
// serializes the full guardrail reason struct; projecting that sink to
// categories too is a noted fast-follow.) Being category-only keeps the
// sink safe even if it's later egressed (e.g. to the dashboard) — no free
// text to leak.
type enforcementRecord struct {
	SessionID           string           `json:"session_id"`
	ToolKind            string           `json:"tool_kind,omitempty"`
	Verdict             string           `json:"verdict"`
	WouldBlock          bool             `json:"would_block"`
	AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|ask|"" (proceed)
	Source              string           `json:"source,omitempty"`
	FailOpen            bool             `json:"fail_open"`
	Stale               bool             `json:"stale,omitempty"`
	PolicyID            string           `json:"policy_id,omitempty"`
	ApprovalID          string           `json:"approval_id,omitempty"` // server correlation id for a REQUIRE_APPROVAL→ask (INV-2 safe)
	Constraints         []map[string]any `json:"constraints,omitempty"`
	GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
	// Redacted / RedactionCategories record a Tier-1 redact-and-continue:
	// whether the tool body was rewritten and which secret categories
	// fired (aws_key, entropy, …) — content-free (INV-2): category names
	// only, never the secret or the body.
	Redacted            bool     `json:"redacted,omitempty"`
	RedactionCategories []string `json:"redaction_categories,omitempty"`
}

// DefaultEnforcementPath is the enforcement audit sink, a sibling of the
// advisory sink (~/.config/openbox/enforcements.jsonl), overridable via
// OPENBOX_ENFORCEMENT_FILE (tests point it at a temp file).
func DefaultEnforcementPath() string {
	if p := os.Getenv(envEnforcementFile); p != "" {
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
func recordEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, applied string) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	rec := enforcementRecord{
		SessionID:       e.SessionID,
		ToolKind:        string(kind),
		Verdict:         string(dec.Evaluation.Verdict),
		WouldBlock:      dec.Evaluation.WouldBlock(),
		AppliedDecision: applied,
		Source:          dec.Source,
		FailOpen:        dec.FailOpen,
		Stale:           dec.Stale,
		PolicyID:        dec.Evaluation.PolicyID,
		ApprovalID:      dec.Evaluation.ApprovalID, // correlates an ask to the governance approval; id only, no content
		Constraints:     dec.Evaluation.Constraints,
	}
	if g := dec.Evaluation.Guardrail; g != nil {
		rec.GuardrailCategories = reasonTypeCategories(g.Reasons) // category types only (INV-2)
	}
	// Record the redaction signal only when a rewrite was actually applied
	// to CC — i.e. the proceed path (applied=="") produced a non-nil
	// reconstruction — so the audit's `redacted` bool never over-reports a
	// category-hit that did not reach updatedInput. redactToolInput is the
	// same pure function applyDecision used, so this stays consistent with
	// what was emitted.
	if applied == "" && dec.RedactedContent != nil &&
		len(redactToolInput(e.ToolInput, dec.RedactedContent.FileText)) > 0 {
		rec.Redacted = true
		rec.RedactionCategories = dec.RedactionCategories // category names only (INV-2)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		logger.Printf("enforcement record skipped (marshal): %v", err)
		return
	}
	if err := appendJSONL(DefaultEnforcementPath(), line); err != nil {
		logger.Printf("enforcement record skipped: %v", err)
	}
}
