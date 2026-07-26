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

// Enforcement — the synchronous pre-execution gate for the Codex adapter
// (STORY-SL7-B; the port of the shipped Claude Code E6 cascade).
//
// This is the provider-shaped EDGE of the shared enforce stack: OBTAIN a local
// decision (E6-S1), apply the per-org FAILURE POLICY (E6-S3), then APPLY the
// verdict onto Codex's PreToolUse hook output contract (E6-S2/E6-S4/E6-S9). The
// middle — decision.InProcessDecider (ADR-0006, no socket/daemon, fail-open on
// every fault), the native policy evaluator (ADR-0005/E6-S8) and the Tier-1
// secret detector (E6-S9) — is CONSUMED unchanged from decision/. This file adds
// nothing to decision/; it only maps its Decision onto Codex's wire shape.
//
// INV-3b (the carve-out to "observe never blocks"): an enforce PreToolUse hook
// MAY block, but only PRE-EXECUTION, hard-bounded, and fail-open by default
// (OD9). The T1 decision takes NO network I/O — it evaluates the synced local
// bundle in-process (microseconds). With enforce OFF (ResolveEnforce false) none
// of this runs and the SL7-A observe path is byte-identical.
//
// ── Codex-shaped deltas from the Claude Code port (each grounded @ rust-v0.145.0
//    + the binary-embedded per-event JSON Schemas, recorded in the SL7-B probes):
//
//   1. permissionDecision literals. Codex's PreToolUse output enum is
//      allow|deny|ask (schema.rs PreToolUsePermissionDecisionWire), BUT the
//      runtime output parser (hooks/src/engine/output_parser.rs
//      unsupported_pre_tool_use_hook_specific_output) REJECTS:
//        - "ask"                              → "unsupported permissionDecision:ask"
//        - "allow" WITHOUT updatedInput       → "unsupported permissionDecision:allow"
//        - updatedInput WITHOUT "allow"       → "updatedInput without permissionDecision:allow"
//      A rejected output marks the hook Failed and the decision is DISCARDED →
//      the tool PROCEEDS (probe P1: a failed/timed-out PreToolUse hook fails
//      OPEN). So the only USABLE levers are: deny+reason (block), and
//      allow+updatedInput (redact-and-proceed). This is the inverse of Claude
//      Code, which emits updatedInput ALONE on the proceed path.
//
//   2. REQUIRE_APPROVAL → deny (OD-SL7-ASK, ruled). Claude Code maps it to `ask`;
//      Codex rejects `ask` (delta 1) and a no-decision fallthrough under
//      approval_policy=never AUTO-RUNS the tool ungoverned (probe P3, live). No
//      approval-policy mode could be PROVEN to surface a native prompt within the
//      harness (codex exec is non-interactive), so per the ruling every
//      REQUIRE_APPROVAL quadrant emits a content-free DENY — strictly tighter,
//      never a silent proceed.
//
//   3. Redaction content field is "command" (delta from CC's content/new_string).
//      apply_patch's PreToolUse tool_input is {"command":<raw patch text>}
//      (core registry ApplyPatchHandler.pre_tool_use_payload) and updatedInput is
//      re-parsed via updated_hook_command → tool_input["command"] as a string. So
//      the redactable file body rides "command" and the rewrite swaps "command".
//
//   4. allow+updatedInput does NOT loosen (tighten-only preserved). Codex resolves
//      competing hooks by "any deny wins" (hook_runtime should_block = any) and
//      updated_input is taken only when NOT blocked; an allow from us cannot
//      override another hook's deny, and there is no "approve/bypass approval"
//      hook lever (PreToolUseHookResult is Continue{updated_input} | Blocked) — so
//      allow+updatedInput is "proceed via Codex's own approval/sandbox flow, with
//      redacted input", never a grant. We emit "allow" ONLY bundled with a
//      non-empty redacting updatedInput; a bare allow is structurally impossible
//      here. Flagged as OD-SL7-ALLOW-REWRITE for G3/G_SEC ratification.
//
// Exit code is ALWAYS 0 (we speak JSON, not Codex's exit-2 block signal), so a
// non-blocking verdict is byte-identical to observe mode.

// maxCommandLen bounds the shell command carried on the LOCAL decision request,
// in BYTES (JSON escaping can expand control bytes ×6; a rune cap would let an
// adversarial command overrun). 8 KiB is ample: the command is only a policy
// MATCH axis (never redacted), local-only, never egressed (INV-2). Truncation
// can only make a policy MISS (→ allow), never a wrong block (fail-open, OD9).
const maxCommandLen = 8 << 10 // 8 KiB (bytes)

// maxRedactBody bounds the file BODY (apply_patch patch text) handed to the
// in-process secret detector. A body over the cap is NOT scanned (Content stays
// nil) → the tool proceeds UNREDACTED (fail-open) rather than pay a slow scan on
// the hot path. It is a SKIP threshold, never a truncation: a truncated patch
// reconstructed into updatedInput would corrupt the write, so we send the whole
// body or none. 512 KiB covers the real secret-paste surface (E6-S9 parity).
const maxRedactBody = 512 << 10 // 512 KiB (bytes)

// maxJSONCompareBytes bounds the jsonEqual double-parse (E6-S4 G_SEC INFO-2).
// Over the cap jsonEqual returns not-equal — the SAFE direction (apply the
// redaction; only forgo suppressing an identical-but-huge no-op rewrite).
const maxJSONCompareBytes = 256 << 10 // 256 KiB (bytes)

// EnforceDecision is the PreToolUse enforce gate: it SYNCHRONOUSLY obtains a
// governance decision from the in-process decider for the tool about to run. It
// NEVER errors and NEVER blocks — the decider fails open (VerdictUnknown/allow)
// on any fault. It reads NO secret (identity is the DID only — INV-1) and takes
// NO network I/O and NO IPC (evaluated in-memory — INV-3b).
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
}

// newDecider builds the enforce-hook decision transport. There is exactly one
// (ADR-0006): the local bundle is evaluated IN-PROCESS — no daemon, no socket.
// It fails open on any fault (absent/unreadable bundle → cold-start fail-open),
// so an infra failure never blocks the developer (OD9 / INV-3b).
func newDecider() decision.Decider {
	return decision.NewInProcessDecider(decision.InProcessConfig{
		BundlePath: ResolveBundlePath(),
	})
}

// ── E6-S3: fail-open / fail-closed failure policy (port verbatim) ────────────
//
// The failure policy decides what the gate does when the decider could NOT
// deliver a real verdict (decision.Decision.FailOpen==true). It is the Go port
// of the reference SDK's _handle_api_error: fail-open → no verdict → proceed;
// fail-closed → synthesized HALT → the same apply cascade denies. Mirroring that
// shape keeps mapVerdict/applyDecision entirely policy-agnostic — a fail-closed
// deny travels the identical path as a real BLOCK.

// FailurePolicy is the per-org enforce failure posture (OD9). FailOpen is the
// zero value and the default: an OpenBox outage degrades to observe (proceed).
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure — the tool proceeds
	// (OD9 default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies on an evaluation failure (explicit per-org opt-in). An
	// OpenBox outage blocks work rather than letting it through ungoverned.
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

// applyFailurePolicy is the Go analog of the SDK's _handle_api_error, applied
// between OBTAIN (E6-S1) and APPLY (E6-S2). It touches a decision ONLY when the
// decider failed to deliver a real verdict (dec.FailOpen) AND the org opted into
// fail-closed: it then synthesizes a HALT (content-free reason) so the unchanged,
// policy-agnostic mapVerdict cascade denies via its normal HALT path.
//
// Every other case returns the decision UNCHANGED:
//   - fail-open (default): a fail-open decision stays VerdictUnknown → mapVerdict
//     emits nothing → proceed (byte-identical to observe).
//   - a REAL verdict (dec.FailOpen==false) under either policy: the failure policy
//     governs ONLY the evaluation-unavailable case, never a real ALLOW/CONSTRAIN/
//     BLOCK answer — a loaded-bundle decider's allow proceeds under fail-closed.
//
// It only ever converts a would-be PROCEED into a DENY, upholding tighten-only
// (E6-S2) and INV-3b (the block is still synchronous and pre-execution). The
// reachable-but-unbundled case (Source=fail-open:no-bundle → FailOpen) denies
// under fail-closed — closing the E6-S7 C6 hole; a deliberate deviation from the
// reference SDK, reconciled entirely upstream in decision.Decision.FailOpen.
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

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the mapper's tool classification (classifyTool) so the
// enforce gate and the observe event classify a tool identically. It carries the
// metadata axes a local policy matches on — tool name/kind, MCP server, file
// operation, permission mode, and (LOCAL-ONLY, never egressed) the shell command.
//
// Content (INV-2) is populated when localRedaction is true — i.e. Tier-1 secret
// detection (E6-S9, default ON) OR content capture (OD4). For the file class
// (apply_patch) the redactable body is the patch text, which Codex carries in
// tool_input["command"] (delta 3). Like the command axis it stays in-process and
// is NEVER egressed (the observe Mapper is untouched, still metadata-only unless
// content capture is on). With localRedaction off, Content stays nil and the
// request is byte-identical to the no-redaction path.
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

// logEnforceDecision emits ONE terse, secret-free (INV-1) and content-free (INV-2)
// diagnostic line for an obtained decision — verdict / source / fail_open / stale
// only, never the command, patch body, or reason free text. stderr only (never
// stdout — Codex parses hook stdout as output JSON).
func logEnforceDecision(logger *log.Logger, e *HookEvent, dec decision.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN"
	}
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t stale=%t policy=%s",
		capStr(e.ToolName), verdict, dec.Evaluation.WouldBlock(), orDash(dec.Source), dec.FailOpen, dec.Stale, policy)
}

// ── E6-S2/E6-S4: apply(verdict) onto Codex's PreToolUse output contract ──────

// Codex PreToolUse permissionDecision values (hooks/src/schema.rs). The enum also
// carries "ask", but the runtime output parser rejects it (delta 1) — so this
// adapter only ever emits deny (block) or allow (ONLY bundled with a redacting
// updatedInput — delta 4). A bare allow is structurally impossible here.
const (
	codexDecisionDeny  = "deny"
	codexDecisionAllow = "allow"
)

// preToolUseOutput is the Codex PreToolUse hook stdout contract. The output
// schema is additionalProperties:false at every level (binary-embedded
// pre-tool-use.command.output), so we emit EXACTLY the documented keys. An exit-0
// hook printing this JSON has its permissionDecision honored: deny blocks (Codex
// feeds the reason back to the model), allow+updatedInput rewrites tool_input
// before the tool runs. All strings are LOCAL (stdout → Codex on this machine, no
// egress) and content-free (INV-2).
type preToolUseOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input (E6-S4 plumbing, E6-S9
	// source). Codex takes it ONLY with permissionDecision:"allow" and re-parses
	// updated_input["command"] as the new patch text. RECONSTRUCTED from the
	// original tool_input with only the "command" field swapped (redactToolInput) —
	// never sourced whole from the decision. Content-bearing but LOCAL (never
	// egressed — INV-2). omitempty ⇒ absent on the deny path and when no redaction.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

// applyDecision maps an obtained decision onto a Codex PreToolUse hook output and
// writes it to stdout — the E6-S2 apply, extended by E6-S4. It returns the applied
// decision ("deny", "allow" for a redact-and-proceed, or "" on silent proceed) and
// whether anything was emitted.
//
// Two levers, in the SDK's own order:
//   - mapVerdict yields "deny" (HALT/BLOCK/guardrail-fail/REQUIRE_APPROVAL) → emit
//     permissionDecision:deny + a content-free reason.
//   - ELSE (the proceed path — CONSTRAIN/ALLOW/UNKNOWN) → apply input redaction
//     (E6-S4/E6-S9). On Codex a rewrite MUST ride permissionDecision:"allow" +
//     updatedInput (delta 1/4) — so when a non-empty redaction exists we emit
//     allow+updatedInput; otherwise we write NOTHING (silent proceed via Codex's
//     own flow, byte-identical to observe).
//
// TIGHTEN-ONLY: stdout carries only deny, OR allow+content-STRIPPING updatedInput
// (never a grant — delta 4). When nothing applies it writes NOTHING.
//
// It NEVER wedges the tool call: a nil stdout or any marshal/write fault degrades
// to "proceed" (fail-open, OD9). Enforcement can only ADD a deny/redaction.
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
		// Codex requires allow to carry a rewrite (delta 1/4). allow is emitted ONLY
		// here — bundled with a non-empty redacting updatedInput.
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

// applyInputRedaction turns a LOCAL redaction (E6-S9 secret detection) into the
// Codex updatedInput bytes, or nil to emit nothing. Invoked ONLY on the proceed
// path (no deny) — as the reference SDK applies _apply_input_redaction only after
// enforce_verdict returns without raising.
//
// nil (no rewrite) unless ALL hold: local redaction on; the Decision carries a
// non-empty RedactedContent.FileText; and reconstructing the ORIGINAL tool_input
// with ONLY the "command" field replaced yields a valid object DIFFERING from the
// original (a no-op / unparseable original is skipped, never rewritten to garbage).
//
// THE STRUCTURAL GUARANTEE (E6-S4/S9): the emitted object is the ORIGINAL
// tool_input with the single recognized content field swapped for the redacted
// body — every other field is carried over VERBATIM, never from the decision. A
// buggy/compromised detector can only change a content VALUE, never add/drop/alter
// a structural field. LOCAL: it travels stdout → Codex on this machine, never
// egressed (INV-2 — see decision.DecisionResponse.RedactedContent).
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

// contentFieldKeys are the tool_input keys that carry a redactable BODY, in the
// same precedence HookEvent.fileText() reads them. For Codex there is exactly one:
// "command" — apply_patch's PreToolUse tool_input is {"command":<patch text>} and
// updated_input is re-parsed via updated_hook_command → updated_input["command"]
// (grounded @ rust-v0.145.0 core ApplyPatchHandler + handlers/mod.rs). Bash also
// uses "command", but Bash is the SHELL class and never carries Content (its
// command is a match axis, not a redactable body), so it never reaches here.
var contentFieldKeys = []string{"command"}

// redactToolInput rebuilds a tool_input object with ONLY the recognized content
// field replaced by redactedBody, preserving every other field byte-for-byte. It
// returns nil when the original is not a JSON object or carries no recognized
// non-empty content field. This is where the content-only guarantee is enforced:
// structural fields are copied from the ORIGINAL as opaque json.RawMessage and are
// never sourced from the decision.
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

// jsonEqual reports whether two JSON documents are semantically equal, ignoring
// key order and insignificant whitespace. Either side unparseable ⇒ not-equal (so
// a redaction is applied rather than suppressed). Bounded re-parse (E6-S4 INFO-2).
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

// mapVerdict is the SDK enforce_verdict cascade (verdict_handler.py) ported to
// Codex decisions, in the SAME priority order HALT > BLOCK > guardrails >
// REQUIRE_APPROVAL > CONSTRAIN > ALLOW (OD-ENF-SCOPE). It returns the Codex
// decision and a content-free reason, or ("","") meaning "emit nothing — proceed".
//
//   - HALT / BLOCK → deny.
//   - A failed guardrail validation → deny, checked AFTER HALT/BLOCK but BEFORE
//     approval and INDEPENDENT of the verdict value (never silently swallowed).
//   - REQUIRE_APPROVAL → deny (OD-SL7-ASK, delta 2). Claude Code maps this to `ask`;
//     Codex rejects `ask` and a no-decision under approval_policy=never auto-runs
//     the tool ungoverned (probe P3), so REQUIRE_APPROVAL denies with a
//     content-free "requires approval" reason — strictly tighter.
//   - CONSTRAIN / ALLOW / UNKNOWN (fail-open) → nothing (proceed). On this proceed
//     path applyDecision then applies input redaction (E6-S4) when localRedaction
//     is on — mapVerdict itself never rewrites the input.
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

// approvalReason builds the LOCAL, content-free deny reason for a REQUIRE_APPROVAL
// verdict (OD-SL7-ASK, delta 2). Unlike Claude Code's `ask` (a native interactive
// prompt), Codex has no usable per-hook approval lever from PreToolUse output, so
// REQUIRE_APPROVAL is realized as a DENY carrying the policy-authored approval
// context (govReason) plus e.ApprovalID — a server correlation id (same class as
// policy_id, INV-2 safe), never tool content. Shown on this machine only.
func approvalReason(e client.Evaluation) string {
	msg := govReason(e, "this action requires human approval per OpenBox governance policy (denied — Codex has no interactive approval hook lever; re-run under an approved policy)")
	if e.ApprovalID != "" {
		msg += " (approval: " + e.ApprovalID + ")"
	}
	return msg
}

// govReason builds the LOCAL, content-free deny reason shown to the developer. It
// surfaces the POLICY-authored reason (the bundle/OPA rule's own text) and the
// policy id — text authored in the policy, not derived from tool content (INV-2).
// Shown on this machine only (stdout → Codex); never egressed.
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

// guardrailReason renders a guardrail-failure deny reason from the CATEGORY types
// only (e.g. "[pii,secrets]") — never the guardrail reason free text (INV-2).
func guardrailReason(g *client.GuardrailResult) string {
	return "OpenBox guardrails validation failed " + reasonTypes(g.Reasons)
}

// enforcementRecord is one line in the enforcement audit sink (E6-S2): the
// decision ACTUALLY APPLIED to a tool call. STRICTLY content-free (INV-1/INV-2):
// verdict/ids/flags plus the guardrail CATEGORY types only — never tool content,
// the policy reason free text, or the guardrail reason free text.
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
	// Record the redaction signal only when a rewrite was ACTUALLY applied to Codex
	// — i.e. the proceed path (applied=="allow") produced a non-nil reconstruction
	// — so the audit never over-reports a category-hit that did not reach
	// updatedInput (G_SEC INFO-2). redactToolInput is the same pure function
	// applyDecision used, so this stays consistent with what was emitted.
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

// ── enforce timeout clamps — adapter-owned, DERIVED from the installed Codex hook
//    timeout (OD-SL7-T2-TIMEOUT), NOT copied from Claude Code's magic numbers ──
//
// DERIVATION (probe P1, live @ codex-cli 0.145.0): when a PreToolUse hook exceeds
// its configured `timeout`, Codex KILLS it and FAILS OPEN — the tool runs (marker
// written; log "hook: PreToolUse Failed" then "exec … succeeded"; wall ≈ the
// timeout). That is the same correctness hazard as Claude Code's 5 s kill: for a
// fail-CLOSED org, a hook that overruns its timeout silently lets the call
// through. So our verdict MUST land BEFORE Codex kills the hook.
//
// The ceiling is therefore a function of the timeout WE INSTALL, not Codex's 600 s
// default: SL7-A's installer writes hotHookTimeoutSec on every PreToolUse handler
// (installer.go). We derive the whole-hook budget from that value minus a margin
// for the config reads + apply + audit that bracket the gate. If an org raises the
// installed hot-hook timeout, these clamps scale with it automatically (they read
// the same constant) — no second edit, no drift. Codex's 600 s default is the
// HEADROOM available (far more than Claude Code's 5 s), but per OD-SL7-T2-TIMEOUT
// the DEFAULT budgets stay conservative until a probe justifies more; more headroom
// is an optimization, not a requirement.

// installedHotHookTimeout is the per-hook `timeout` SL7-A's installer writes on the
// PreToolUse handler (installer.go hotHookTimeoutSec). The enforce budgets are
// derived from THIS, so raising it raises them in lock-step.
const installedHotHookTimeout = time.Duration(hotHookTimeoutSec) * time.Second

// hookBudgetMargin is the slack reserved under installedHotHookTimeout for the
// non-gate work in the hook (config reads, classify, apply, spool, audit) so our
// verdict is written well before Codex's kill. 1 s is generous for microsecond
// in-process work + one bounded stdout write.
const hookBudgetMargin = 1 * time.Second

// maxEnforceHookBudget caps the WHOLE enforce PreToolUse hook's wall clock (T1
// decider + a possible T2 /evaluate, run SEQUENTIALLY) so the per-tier budgets can
// never JOINTLY push the hook past Codex's kill (→ fail-open, which would defeat a
// fail-closed org). Derived: installed timeout − margin.
const maxEnforceHookBudget = installedHotHookTimeout - hookBudgetMargin

// maxEnforceTimeout clamps the configurable T1 decider budget. The in-process
// decider is microseconds (no network — ADR-0006), so this is a defensive bound,
// kept conservative and well under maxEnforceHookBudget.
const maxEnforceTimeout = 2 * time.Second
