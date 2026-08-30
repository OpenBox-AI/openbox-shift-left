package claudecode

import (
	"context"
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
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

// buildDecisionRequest assembles the local decision request from a
// PreToolUse payload, reusing the Mapper's tool classification
// (classifyTool / filePath) so the enforce gate and the observe event
// classify a tool identically. It carries the metadata axes a local
// policy matches on — tool name/kind, MCP server, file path/operation,
// permission mode, and (local-only, never egressed) the shell command.
//
// Content (INV-2) is populated when localRedaction is true — i.e. local detection
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
	// is carried (the redactable tool input the secret detector scans). A body over
	// hookflow.MaxRedactBody is NOT sent — the tool proceeds unredacted (fail-open) rather
	// than be truncated (a truncated reconstruction would corrupt the write). Left
	// nil for a non-file tool, an empty body, or an oversized body.
	if localRedaction && isFileSemantic(sem) {
		if body := e.fileText(); body != "" && len(body) <= hookflow.MaxRedactBody {
			req.Content = &client.Content{FileText: body}
		}
	}
	return req
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

// preToolUseOutput is the Claude Code PreToolUse hook stdout contract: an
// exit-0 hook that prints this JSON has its permissionDecision honored —
// `deny` blocks the tool call (Claude sees the reason), `ask` shows the
// user a permission prompt. permissionDecisionReason is shown locally
// (stdout → Claude Code on the same machine, no egress) and carries the
// policy-authored reason, never the tool command/file/output content
// (INV-2).
type preToolUseOutput struct {
	// Continue/StopReason are Claude Code's session-stop lever, common to every
	// hook and documented to take precedence over any per-event decision. They
	// are emitted ONLY for a session-halting HALT (hookflow.DecisionHalt):
	// `continue:false` ends the turn immediately and StopReason is shown to the
	// user. A *bool because omitempty drops a plain false — the same trap
	// devconfig.Enforce already documents — and `continue:true` must never be
	// emitted (it could read as a grant; tighten-only).
	Continue           *bool              `json:"continue,omitempty"`
	StopReason         string             `json:"stopReason,omitempty"`
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
