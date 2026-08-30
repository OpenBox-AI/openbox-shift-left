package codex

import (
	"context"
	"encoding/json"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// Enforcement — the synchronous pre-execution gate for the Codex adapter, the port of the
// shipped Claude Code enforce cascade.
//
// This is the provider-shaped edge of the shared enforce stack: obtain a local decision, apply
// the per-org failure policy, then apply the verdict onto Codex's PreToolUse hook output
// contract. The middle — decision.InProcessDecider (that decision, no socket/daemon, fail-open
// on every fault), the native policy evaluator and the local detection secret detector — is
// consumed unchanged from decision/. This file adds nothing to decision/; it only maps its
// Decision onto Codex's wire shape.
//
// INV-3b (the carve-out to "observe never blocks"): an enforce PreToolUse hook may block, but
// only pre-execution, hard-bounded, and fail-open by default. The T1 decision takes no network
// I/O — it evaluates the synced local bundle in-process (microseconds). With enforce off
// (ResolveEnforce false) none of this runs and the observe path is byte-identical.
//
// ── Codex-shaped deltas from the Claude Code port (each grounded @ rust-v0.145.0 + the
// binary-embedded per-event JSON Schemas):
//
// 1. permissionDecision literals. Codex's PreToolUse output enum is allow|deny|ask (schema.rs
// PreToolUsePermissionDecisionWire), but the runtime output parser
// (hooks/src/engine/output_parser.rs unsupported_pre_tool_use_hook_specific_output) rejects: -
// "ask"                              → "unsupported permissionDecision:ask" - "allow" without
// updatedInput       → "unsupported permissionDecision:allow" - updatedInput without "allow"
// → "updatedInput without permissionDecision:allow" A rejected output marks the hook Failed and
// the decision is discarded → the tool proceeds (a failed/timed-out PreToolUse hook fails
// open). So the only usable levers are: deny+reason (block), and allow+updatedInput
// (redact-and-proceed). This is the inverse of Claude Code, which emits updatedInput alone on
// the proceed path.
//
// 2. REQUIRE_APPROVAL → deny. Claude Code maps it to `ask`; Codex rejects `ask` (delta 1) and a
// no-decision fallthrough under approval_policy=never auto-runs the tool ungoverned. No
// approval-policy mode could be proven to surface a native prompt within the harness (codex
// exec is non-interactive), so every REQUIRE_APPROVAL quadrant emits a content-free deny
// strictly tighter, never a silent proceed.
//
// 3. Redaction content field is "command" (delta from CC's content/new_string). apply_patch's
// PreToolUse tool_input is {"command":<raw patch text>} (core registry
// ApplyPatchHandler.pre_tool_use_payload) and updatedInput is re-parsed via
// updated_hook_command → tool_input["command"] as a string. So the redactable file body rides
// "command" and the rewrite swaps "command".
//
// 4. allow+updatedInput does not loosen (tighten-only preserved). Codex resolves competing
// hooks by "any deny wins" (hook_runtime should_block = any) and updated_input is taken only
// when not blocked; an allow from us cannot override another hook's deny, and there is no
// "approve/bypass approval" hook lever (PreToolUseHookResult is Continue{updated_input} |
// Blocked) — so allow+updatedInput is "proceed via Codex's own approval/sandbox flow, with
// redacted input", never a grant. We emit "allow" only bundled with a non-empty redacting
// updatedInput; a bare allow is structurally impossible here.
//
// Exit code is always 0 (we speak JSON, not Codex's exit-2 block signal), so a non-blocking
// verdict is byte-identical to observe mode.

// EnforceDecision is the PreToolUse enforce gate: it synchronously obtains
// a governance decision from the in-process decider for the tool about to
// run. It never errors and never blocks — the decider fails open
// (VerdictUnknown/allow) on any fault. It reads no secret (identity is
// the DID only — INV-1) and takes no network I/O and no IPC (evaluated
// in-memory — INV-3b).
func EnforceDecision(ctx context.Context, cl decision.Decider, id Identity, e *HookEvent, localRedaction bool) decision.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e, localRedaction))
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

// buildDecisionRequest assembles the local decision request from a
// PreToolUse payload, reusing the mapper's tool classification
// (classifyTool) so the enforce gate and the observe event classify a
// tool identically. It carries the metadata axes a local policy matches
// on — tool name/kind, MCP server, file operation, permission mode, and
// (local-only, never egressed) the shell command.
//
// Content (INV-2) is populated when localRedaction is true — i.e. local detection
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
		attrs["command"] = hookflow.CapCommand(e.command())
	}

	req := decision.DecisionRequest{
		SessionID:    e.SessionID,
		DeveloperDID: id.DeveloperDID,
		EventType:    client.EventToolCall, // the pre-execution gate is a ToolCall decision
		Tool:         tool,
		Attributes:   hookflow.CompactAny(attrs),
	}

	// Content is GATED on localRedaction and LOCAL-only (INV-2). Only the file BODY
	// (apply_patch patch text) is carried. A body over hookflow.MaxRedactBody is NOT sent —
	// the tool proceeds unredacted (fail-open) rather than be truncated (a
	// truncated reconstruction would corrupt the patch). Left nil for a non-file
	// tool, an empty body, or an oversized body.
	if localRedaction && isFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= hookflow.MaxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
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

// ── apply(verdict) onto Codex's PreToolUse output contract ───────────────

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

// ── Enforce timeout clamps ────────────────────────────────────────────
//
// The whole-hook budget is derived by the engine from this adapter's
// declared ceiling (HookCeilings in enforce_tier2.go → hookflow.EnforceBudget),
// so the margin arithmetic lives in one place for both providers rather
// than being copied per adapter.
//
// Why the ceiling is what it is, since the evidence belongs with the
// number: when a PreToolUse hook exceeds its configured `timeout`, Codex
// kills it and fails open — the tool runs (marker written; log "hook:
// PreToolUse Failed" then "exec … succeeded"; wall ≈ the timeout). For a
// fail-closed org that is a silently ungoverned call, so our verdict must
// land first. The ceiling is therefore a function of the timeout WE
// install, not Codex's 600s default: raise the installed timeout and the
// budget scales with it, because both read the same constant. That 600s
// default is headroom available rather than headroom taken — the defaults
// stay conservative until there is a reason for more.

// maxEnforceTimeout bounds the local decision step. It is microseconds of
// in-process work, so this is a defensive bound kept well under the
// whole-hook budget.
const maxEnforceTimeout = 2 * time.Second
