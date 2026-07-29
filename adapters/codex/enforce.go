package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Enforcement — the synchronous pre-execution gate for the Codex adapter,
// the port of the shipped Claude Code enforce cascade.
//
// This is the provider-shaped edge of the shared enforce stack: obtain a
// local decision, apply the per-org failure policy, then apply the
// verdict onto Codex's PreToolUse hook output contract. The middle —
// decision.InProcessDecider (ADR-0006, no socket/daemon, fail-open on
// every fault), the native policy evaluator (ADR-0005) and the Tier-1
// secret detector — is consumed unchanged from decision/. This file adds
// nothing to decision/; it only maps its Decision onto Codex's wire shape.
//
// INV-3b (the carve-out to "observe never blocks"): an enforce PreToolUse
// hook may block, but only pre-execution, hard-bounded, and fail-open by
// default. The T1 decision takes no network I/O — it evaluates the synced
// local bundle in-process (microseconds). With enforce off (ResolveEnforce
// false) none of this runs and the observe path is byte-identical.
//
// ── Codex-shaped deltas from the Claude Code port (each grounded @
//    rust-v0.145.0 + the binary-embedded per-event JSON Schemas):
//
//   1. permissionDecision literals. Codex's PreToolUse output enum is
//      allow|deny|ask (schema.rs PreToolUsePermissionDecisionWire), but
//      the runtime output parser (hooks/src/engine/output_parser.rs
//      unsupported_pre_tool_use_hook_specific_output) rejects:
//        - "ask"                              → "unsupported permissionDecision:ask"
//        - "allow" without updatedInput       → "unsupported permissionDecision:allow"
//        - updatedInput without "allow"       → "updatedInput without permissionDecision:allow"
//      A rejected output marks the hook Failed and the decision is
//      discarded → the tool proceeds (a failed/timed-out PreToolUse hook
//      fails open). So the only usable levers are: deny+reason (block),
//      and allow+updatedInput (redact-and-proceed). This is the inverse
//      of Claude Code, which emits updatedInput alone on the proceed
//      path.
//
//   2. REQUIRE_APPROVAL → deny. Claude Code maps it to `ask`; Codex
//      rejects `ask` (delta 1) and a no-decision fallthrough under
//      approval_policy=never auto-runs the tool ungoverned. No
//      approval-policy mode could be proven to surface a native prompt
//      within the harness (codex exec is non-interactive), so every
//      REQUIRE_APPROVAL quadrant emits a content-free deny — strictly
//      tighter, never a silent proceed.
//
//   3. Redaction content field is "command" (delta from CC's
//      content/new_string). apply_patch's PreToolUse tool_input is
//      {"command":<raw patch text>} (core registry
//      ApplyPatchHandler.pre_tool_use_payload) and updatedInput is
//      re-parsed via updated_hook_command → tool_input["command"] as a
//      string. So the redactable file body rides "command" and the
//      rewrite swaps "command".
//
//   4. allow+updatedInput does not loosen (tighten-only preserved). Codex
//      resolves competing hooks by "any deny wins" (hook_runtime
//      should_block = any) and updated_input is taken only when not
//      blocked; an allow from us cannot override another hook's deny,
//      and there is no "approve/bypass approval" hook lever
//      (PreToolUseHookResult is Continue{updated_input} | Blocked) — so
//      allow+updatedInput is "proceed via Codex's own approval/sandbox
//      flow, with redacted input", never a grant. We emit "allow" only
//      bundled with a non-empty redacting updatedInput; a bare allow is
//      structurally impossible here.
//
// Exit code is always 0 (we speak JSON, not Codex's exit-2 block signal),
// so a non-blocking verdict is byte-identical to observe mode.

// maxCommandLen bounds the shell command carried on the local decision
// request, in bytes (JSON escaping can expand control bytes ×6; a rune
// cap would let an adversarial command overrun). 8 KiB is ample: the
// command is only a policy match axis (never redacted), local-only,
// never egressed (INV-2). Truncation can only make a policy miss (→
// allow), never a wrong block (fail-open).
const maxCommandLen = 8 << 10 // 8 KiB (bytes)

// maxRedactBody bounds the file body (apply_patch patch text) handed to
// the in-process secret detector. A body over the cap is not scanned
// (Content stays nil) → the tool proceeds unredacted (fail-open) rather
// than pay a slow scan on the hot path. It is a skip threshold, never a
// truncation: a truncated patch reconstructed into updatedInput would
// corrupt the write, so we send the whole body or none. 512 KiB covers
// the real secret-paste surface (CC parity).
const maxRedactBody = 512 << 10 // 512 KiB (bytes)

// maxJSONCompareBytes bounds the jsonEqual double-parse. Over the cap
// jsonEqual returns not-equal — the safe direction (apply the redaction;
// only forgo suppressing an identical-but-huge no-op rewrite).
const maxJSONCompareBytes = 256 << 10 // 256 KiB (bytes)

// EnforceDecision is the PreToolUse enforce gate: it synchronously obtains
// a governance decision from the in-process decider for the tool about to
// run. It never errors and never blocks — the decider fails open
// (VerdictUnknown/allow) on any fault. It reads no secret (identity is
// the DID only — INV-1) and takes no network I/O and no IPC (evaluated
// in-memory — INV-3b).
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
}

// newDecider builds the enforce-hook decision transport. There is exactly
// one (ADR-0006): the local bundle is evaluated in-process — no daemon,
// no socket. It fails open on any fault (absent/unreadable bundle →
// cold-start fail-open), so an infra failure never blocks the developer
// (INV-3b).
func newDecider() decision.Decider {
	return decision.NewInProcessDecider(decision.InProcessConfig{
		BundlePath: ResolveBundlePath(),
	})
}

// ── Fail-open / fail-closed failure policy (port verbatim) ───────────────
//
// The failure policy decides what the gate does when the decider could
// not deliver a real verdict (decision.Decision.FailOpen==true). It is
// the Go port of the reference SDK's _handle_api_error: fail-open → no
// verdict → proceed; fail-closed → synthesized HALT → the same apply
// cascade denies. Mirroring that shape keeps mapVerdict/applyDecision
// entirely policy-agnostic — a fail-closed deny travels the identical
// path as a real BLOCK.

// FailurePolicy is the per-org enforce failure posture. FailOpen is the
// zero value and the default: an OpenBox outage degrades to observe
// (proceed).
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure — the tool
	// proceeds (default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies on an evaluation failure (explicit per-org
	// opt-in). An OpenBox outage blocks work rather than letting it
	// through ungoverned.
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
// opted into fail-closed: it then synthesizes a HALT (content-free
// reason) so the unchanged, policy-agnostic mapVerdict cascade denies via
// its normal HALT path.
//
// Every other case returns the decision unchanged:
//   - fail-open (default): a fail-open decision stays VerdictUnknown →
//     mapVerdict emits nothing → proceed (byte-identical to observe).
//   - a real verdict (dec.FailOpen==false) under either policy: the
//     failure policy governs only the evaluation-unavailable case, never
//     a real ALLOW/CONSTRAIN/BLOCK answer — a loaded-bundle decider's
//     allow proceeds under fail-closed.
//
// It only ever converts a would-be proceed into a deny, upholding
// tighten-only and INV-3b (the block is still synchronous and
// pre-execution). The reachable-but-unbundled case
// (Source=fail-open:no-bundle → FailOpen) denies under fail-closed — a
// deliberate deviation from the reference SDK, reconciled entirely
// upstream in decision.Decision.FailOpen.
func applyFailurePolicy(dec decision.Decision, policy FailurePolicy) decision.Decision {
	if !dec.FailOpen || policy != FailClosed {
		return dec
	}
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = failClosedReason(dec.Evaluation.Reason)
	return dec
}

// failClosedReason builds the content-free deny reason for a fail-closed
// no-verdict case. govReason prepends "OpenBox governance: ". cause is the
// decider's internal diagnostic (content-free) — never tool content (INV-2).
func failClosedReason(cause string) string {
	r := "request denied — no governance decision could be obtained and this session is fail-closed"
	if cause != "" {
		r += " (" + cause + ")"
	}
	return r
}

// buildDecisionRequest assembles the local decision request from a
// PreToolUse payload, reusing the mapper's tool classification
// (classifyTool) so the enforce gate and the observe event classify a
// tool identically. It carries the metadata axes a local policy matches
// on — tool name/kind, MCP server, file operation, permission mode, and
// (local-only, never egressed) the shell command.
//
// Content (INV-2) is populated when localRedaction is true — i.e. Tier-1
// secret detection (default on) or content capture. For the file class
// (apply_patch) the redactable body is the patch text, which Codex
// carries in tool_input["command"] (delta 3). Like the command axis it
// stays in-process and is never egressed (the observe Mapper is
// untouched, still metadata-only unless content capture is on). With
// localRedaction off, Content stays nil and the request is byte-identical
// to the no-redaction path.
func buildDecisionRequest(id Identity, e *HookEvent, localRedaction bool) decision.DecisionRequest {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	// Metadata axes only (INV-2). compactAny drops empty values so a rule matching
	// on an absent attribute fails to match rather than matching "".
	attrs := map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	}
	switch {
	case isFileSemantic(sem):
		// Codex apply_patch carries NO structural file_path (the patch body is the
		// locator), so only the operation is a metadata match axis; the patch body
		// rides Content for redaction (below), never as a match attribute.
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// Local-only: the shell command is the axis a policy matches a dangerous
		// action on. It goes ONLY to the in-process decider and is never egressed/
		// logged (HookEvent.command). Bounded to keep the local request small.
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
	// (apply_patch patch text) is carried. A body over maxRedactBody is NOT sent —
	// the tool proceeds unredacted (fail-open) rather than be truncated (a
	// truncated reconstruction would corrupt the patch). Left nil for a non-file
	// tool, an empty body, or an oversized body.
	if localRedaction && isFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= maxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
}

// capCommand bounds the local-only command to maxCommandLen BYTES, truncating at
// a UTF-8 rune boundary so a multibyte rune is never split. An empty command
// yields "" (compactAny then drops it).
func capCommand(s string) string {
	if len(s) <= maxCommandLen {
		return s
	}
	cut := maxCommandLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// compactAny drops empty-string values from an attribute map and returns nil when
// nothing is left, so the request carries no empty axes for a policy to spuriously
// not-match on.
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

// isFileSemantic reports whether a semantic type is one of core's file_* types
// (CC-parity). For Codex only "file_write" is produced today (apply_patch), but
// the full set is kept so a future classifier widening needs no edit here.
func isFileSemantic(sem string) bool {
	switch sem {
	case "file_read", "file_write", "file_open", "file_delete":
		return true
	}
	return false
}

// logEnforceDecision emits one terse, secret-free (INV-1) and content-free
// (INV-2) diagnostic line for an obtained decision — verdict / source /
// fail_open / stale only, never the command, patch body, or reason free
// text. stderr only (never stdout — Codex parses hook stdout as output
// JSON).
func logEnforceDecision(logger *log.Logger, e *HookEvent, dec decision.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN"
	}
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t stale=%t policy=%s",
		capStr(e.ToolName), verdict, dec.Evaluation.WouldBlock(), orDash(dec.Source), dec.FailOpen, dec.Stale, policy)
}

// ── apply(verdict) onto Codex's PreToolUse output contract ───────────────

// Codex PreToolUse permissionDecision values (hooks/src/schema.rs). The
// enum also carries "ask", but the runtime output parser rejects it
// (delta 1) — so this adapter only ever emits deny (block) or allow (only
// bundled with a redacting updatedInput — delta 4). A bare allow is
// structurally impossible here.
const (
	codexDecisionDeny  = "deny"
	codexDecisionAllow = "allow"
)

// preToolUseOutput is the Codex PreToolUse hook stdout contract. The
// output schema is additionalProperties:false at every level
// (binary-embedded pre-tool-use.command.output), so we emit exactly the
// documented keys. An exit-0 hook printing this JSON has its
// permissionDecision honored: deny blocks (Codex feeds the reason back
// to the model), allow+updatedInput rewrites tool_input before the tool
// runs. All strings are local (stdout → Codex on this machine, no
// egress) and content-free (INV-2).
type preToolUseOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input. Codex takes it
	// only with permissionDecision:"allow" and re-parses
	// updated_input["command"] as the new patch text. Reconstructed from
	// the original tool_input with only the "command" field swapped
	// (redactToolInput) — never sourced whole from the decision.
	// Content-bearing but local (never egressed — INV-2). omitempty ⇒
	// absent on the deny path and when no redaction.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

// applyDecision maps an obtained decision onto a Codex PreToolUse hook
// output and writes it to stdout. It returns the applied decision
// ("deny", "allow" for a redact-and-proceed, or "" on silent proceed) and
// whether anything was emitted.
//
// Two levers, in the SDK's own order:
//   - mapVerdict yields "deny" (HALT/BLOCK/guardrail-fail/REQUIRE_APPROVAL)
//     → emit permissionDecision:deny + a content-free reason.
//   - else (the proceed path — CONSTRAIN/ALLOW/UNKNOWN) → apply input
//     redaction. On Codex a rewrite must ride permissionDecision:"allow"
//   - updatedInput (delta 1/4) — so when a non-empty redaction exists
//     we emit allow+updatedInput; otherwise we write nothing (silent
//     proceed via Codex's own flow, byte-identical to observe).
//
// Tighten-only: stdout carries only deny, or allow+content-stripping
// updatedInput (never a grant — delta 4). When nothing applies it writes
// nothing.
//
// It never wedges the tool call: a nil stdout or any marshal/write fault
// degrades to "proceed" (fail-open). Enforcement can only add a
// deny/redaction.
func applyDecision(stdout io.Writer, dec decision.Decision, localRedaction bool, origInput json.RawMessage) (applied string, emitted bool) {
	if stdout == nil {
		return "", false // fail-open: nowhere to write
	}
	verdictDecision, reason := mapVerdict(dec.Evaluation)
	hso := hookSpecificOutput{HookEventName: string(HookPreToolUse)}

	if verdictDecision == codexDecisionDeny {
		hso.PermissionDecision = codexDecisionDeny
		hso.PermissionDecisionReason = reason
	} else if rebuilt := applyInputRedaction(dec, localRedaction, origInput); len(rebuilt) > 0 {
		// Codex requires allow to carry a rewrite (delta 1/4). allow is
		// emitted only here — bundled with a non-empty redacting
		// updatedInput.
		hso.PermissionDecision = codexDecisionAllow
		hso.UpdatedInput = rebuilt
		applied = codexDecisionAllow
	}

	if hso.PermissionDecision == "" {
		return "", false // proceed, nothing to say → write nothing
	}
	line, err := json.Marshal(preToolUseOutput{HookSpecificOutput: hso})
	if err != nil {
		return "", false // fail-open: never wedge a tool call on a marshal fault
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		return "", false // fail-open: a write fault degrades to proceed
	}
	if hso.PermissionDecision == codexDecisionDeny {
		return codexDecisionDeny, true
	}
	return applied, true
}

// applyInputRedaction turns a local redaction (secret detection) into the
// Codex updatedInput bytes, or nil to emit nothing. Invoked only on the
// proceed path (no deny) — as the reference SDK applies
// _apply_input_redaction only after enforce_verdict returns without
// raising.
//
// nil (no rewrite) unless all hold: local redaction on; the Decision
// carries a non-empty RedactedContent.FileText; and reconstructing the
// original tool_input with only the "command" field replaced yields a
// valid object differing from the original (a no-op / unparseable
// original is skipped, never rewritten to garbage).
//
// The structural guarantee: the emitted object is the original
// tool_input with the single recognized content field swapped for the
// redacted body — every other field is carried over verbatim, never from
// the decision. A buggy/compromised detector can only change a content
// value, never add/drop/alter a structural field. Local: it travels
// stdout → Codex on this machine, never egressed (INV-2 — see
// decision.DecisionResponse.RedactedContent).
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
// the same precedence HookEvent.fileText() reads them. For Codex there is
// exactly one: "command" — apply_patch's PreToolUse tool_input is
// {"command":<patch text>} and updated_input is re-parsed via
// updated_hook_command → updated_input["command"] (grounded @
// rust-v0.145.0 core ApplyPatchHandler + handlers/mod.rs). Bash also uses
// "command", but Bash is the shell class and never carries Content (its
// command is a match axis, not a redactable body), so it never reaches
// here.
var contentFieldKeys = []string{"command"}

// redactToolInput rebuilds a tool_input object with only the recognized
// content field replaced by redactedBody, preserving every other field
// byte-for-byte. It returns nil when the original is not a JSON object or
// carries no recognized non-empty content field. This is where the
// content-only guarantee is enforced: structural fields are copied from
// the original as opaque json.RawMessage and are never sourced from the
// decision.
func redactToolInput(origInput json.RawMessage, redactedBody string) json.RawMessage {
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

// jsonEqual reports whether two JSON documents are semantically equal,
// ignoring key order and insignificant whitespace. Either side
// unparseable ⇒ not-equal (so a redaction is applied rather than
// suppressed). Bounded re-parse.
func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > maxJSONCompareBytes || len(b) > maxJSONCompareBytes {
		return false
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

// mapVerdict is the SDK enforce_verdict cascade ported to Codex decisions,
// in the same priority order HALT > BLOCK > guardrails > REQUIRE_APPROVAL
// > CONSTRAIN > ALLOW. It returns the Codex decision and a content-free
// reason, or ("","") meaning "emit nothing — proceed".
//
//   - HALT / BLOCK → deny.
//   - A failed guardrail validation → deny, checked after HALT/BLOCK but
//     before approval and independent of the verdict value (never
//     silently swallowed).
//   - REQUIRE_APPROVAL → deny (delta 2). Claude Code maps this to `ask`;
//     Codex rejects `ask` and a no-decision under approval_policy=never
//     auto-runs the tool ungoverned, so REQUIRE_APPROVAL denies with a
//     content-free "requires approval" reason — strictly tighter.
//   - CONSTRAIN / ALLOW / UNKNOWN (fail-open) → nothing (proceed). On
//     this proceed path applyDecision then applies input redaction when
//     localRedaction is on — mapVerdict itself never rewrites the input.
func mapVerdict(e client.Evaluation) (verdictDecision, reason string) {
	switch e.Verdict {
	case client.VerdictHalt:
		return codexDecisionDeny, govReason(e, "action halted by OpenBox governance policy")
	case client.VerdictBlock:
		return codexDecisionDeny, govReason(e, "action blocked by OpenBox governance policy")
	}
	if g := e.Guardrail; g != nil && !g.Passed {
		return codexDecisionDeny, guardrailReason(g)
	}
	if e.Verdict == client.VerdictRequireApproval {
		return codexDecisionDeny, approvalReason(e)
	}
	return "", ""
}

// approvalReason builds the local, content-free deny reason for a
// REQUIRE_APPROVAL verdict (delta 2). Unlike Claude Code's `ask` (a
// native interactive prompt), Codex has no usable per-hook approval lever
// from PreToolUse output, so REQUIRE_APPROVAL is realized as a deny
// carrying the policy-authored approval context (govReason) plus
// e.ApprovalID — a server correlation id (same class as policy_id, INV-2
// safe), never tool content. Shown on this machine only.
func approvalReason(e client.Evaluation) string {
	msg := govReason(e, "this action requires human approval per OpenBox governance policy (denied — Codex has no interactive approval hook lever; re-run under an approved policy)")
	if e.ApprovalID != "" {
		msg += " (approval: " + e.ApprovalID + ")"
	}
	return msg
}

// govReason builds the local, content-free deny reason shown to the
// developer. It surfaces the policy-authored reason (the bundle/OPA
// rule's own text) and the policy id — text authored in the policy, not
// derived from tool content (INV-2). Shown on this machine only (stdout →
// Codex); never egressed.
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
// category types only (e.g. "[pii,secrets]") — never the guardrail
// reason free text (INV-2).
func guardrailReason(g *client.GuardrailResult) string {
	return "OpenBox guardrails validation failed " + reasonTypes(g.Reasons)
}

// enforcementRecord is one line in the enforcement audit sink: the
// decision actually applied to a tool call. Strictly content-free
// (INV-1/INV-2): verdict/ids/flags plus the guardrail category types
// only — never tool content, the policy reason free text, or the
// guardrail reason free text.
type enforcementRecord struct {
	SessionID           string           `json:"session_id"`
	ToolKind            string           `json:"tool_kind,omitempty"`
	Verdict             string           `json:"verdict"`
	WouldBlock          bool             `json:"would_block"`
	AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|allow(redact)|"" (proceed)
	Source              string           `json:"source,omitempty"`
	FailOpen            bool             `json:"fail_open"`
	Stale               bool             `json:"stale,omitempty"`
	PolicyID            string           `json:"policy_id,omitempty"`
	ApprovalID          string           `json:"approval_id,omitempty"`
	Constraints         []map[string]any `json:"constraints,omitempty"`
	GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
	Redacted            bool             `json:"redacted,omitempty"`
	RedactionCategories []string         `json:"redaction_categories,omitempty"`
}

// DefaultEnforcementPath is the enforcement audit sink, a sibling of the advisory
// sink (~/.config/openbox/enforcements.jsonl), overridable via
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

// recordEnforcement appends one enforcement-decision audit line for an applied
// decision. Best-effort and OFF the blocking path: it runs after the stdout
// decision is written, and any failure is logged to stderr and swallowed (INV-3).
// Content-free (INV-1/INV-2).
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
		ApprovalID:      dec.Evaluation.ApprovalID,
		Constraints:     dec.Evaluation.Constraints,
	}
	if g := dec.Evaluation.Guardrail; g != nil {
		rec.GuardrailCategories = reasonTypeCategories(g.Reasons)
	}
	// Record the redaction signal only when a rewrite was actually applied
	// to Codex — i.e. the proceed path (applied=="allow") produced a
	// non-nil reconstruction — so the audit never over-reports a
	// category-hit that did not reach updatedInput. redactToolInput is
	// the same pure function applyDecision used, so this stays consistent
	// with what was emitted.
	if applied == codexDecisionAllow && dec.RedactedContent != nil &&
		len(redactToolInput(e.ToolInput, dec.RedactedContent.FileText)) > 0 {
		rec.Redacted = true
		rec.RedactionCategories = dec.RedactionCategories
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

// ── Enforce timeout clamps — adapter-owned, derived from the installed
//    Codex hook timeout, not copied from Claude Code's magic numbers ────
//
// Derivation: when a PreToolUse hook exceeds its configured `timeout`,
// Codex kills it and fails open — the tool runs (marker written; log
// "hook: PreToolUse Failed" then "exec … succeeded"; wall ≈ the timeout).
// That is the same correctness hazard as Claude Code's 5s kill: for a
// fail-closed org, a hook that overruns its timeout silently lets the
// call through. So our verdict must land before Codex kills the hook.
//
// The ceiling is therefore a function of the timeout we install, not
// Codex's 600s default: the installer writes hotHookTimeoutSec on every
// PreToolUse handler (installer.go). We derive the whole-hook budget from
// that value minus a margin for the config reads + apply + audit that
// bracket the gate. If an org raises the installed hot-hook timeout,
// these clamps scale with it automatically (they read the same constant)
// — no second edit, no drift. Codex's 600s default is the headroom
// available (far more than Claude Code's 5s), but the default budgets
// stay conservative until there's reason for more; more headroom is an
// optimization, not a requirement.

// installedHotHookTimeout is the per-hook `timeout` the installer writes
// on the PreToolUse handler (installer.go hotHookTimeoutSec). The enforce
// budgets are derived from this, so raising it raises them in lock-step.
const installedHotHookTimeout = time.Duration(hotHookTimeoutSec) * time.Second

// hookBudgetMargin is the slack reserved under installedHotHookTimeout
// for the non-gate work in the hook (config reads, classify, apply,
// spool, audit) so our verdict is written well before Codex's kill. 1s is
// generous for microsecond in-process work + one bounded stdout write.
const hookBudgetMargin = 1 * time.Second

// maxEnforceHookBudget caps the whole enforce PreToolUse hook's wall
// clock (T1 decider + a possible T2 /evaluate, run sequentially) so the
// per-tier budgets can never jointly push the hook past Codex's kill (→
// fail-open, which would defeat a fail-closed org). Derived: installed
// timeout − margin.
const maxEnforceHookBudget = installedHotHookTimeout - hookBudgetMargin

// maxEnforceTimeout clamps the configurable T1 decider budget. The
// in-process decider is microseconds (no network — ADR-0006), so this is
// a defensive bound, kept conservative and well under
// maxEnforceHookBudget.
const maxEnforceTimeout = 2 * time.Second
