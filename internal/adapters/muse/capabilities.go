package muse

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Capabilities returns the Muse adapter's capability profile.
//
// Every claim below rests on Muse's published hook documentation (Muse Code SDK
// hook-events reference, verified by Meta on 1.3.0, and the 1.4.0 changelog). No
// Muse binary was available when this adapter was written, so none of it is
// measured on a live session, and the notes say so where it matters. A gap is
// stated as scope, never as impossibility: the dashboard renders "not reported
// by this tool" as a first-class state, and a synthesized value costs the audit
// trail its standing as evidence.
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},

		{Key: "telemetry.hook", Supported: true, How: "twelve events map to contract types (hookevent.go): SessionStart, UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse, PostToolUseFailure, SubagentStart, SubagentStop, StopFailure, SessionEnd, PreLLMCall and PostLLMCall (a subagent runs under its own session id, so its start and stop are that session's SessionStarted and SessionEnded, and a session with no known run, or one SessionEnd already sealed as on `muse resume`, is opened before its first event); Stop runs only the session-log reconciler and reports nothing. PreCompact, PostCompact, Notification, PostToolBatch and Interrupt have no contract type and are never installed; Interrupt in particular never fabricates a completion. Payload shapes were read off Muse 1.4.1 captures; anything not captured there is unverified (see README.md)"},

		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse/PostToolUseFailure over every tool, MCP included (installed with no matcher; mcp__<server>__<tool> -> ToolMCP), paired by tool_use_id (mapper.go mapTool). A tool blocked before it ran emits the started row only, and that asymmetry is never closed by fabricating a completion. Whether an MCP call fires PreToolUse is documented by implication only; a payload over 256 KiB is never delivered to any hook"},

		{Key: "tool.status", Supported: true, How: "structural: PostToolUse is completed and PostToolUseFailure is failed, two distinct hooks, so the outcome is never inferred from a response body"},

		{Key: "verdict.apply", Supported: true, How: "four gate classes, all decided by /evaluate, tighten-only: PreToolUse (permissionDecision:deny), UserPromptSubmit (decision:block), PermissionRequest (behavior:deny, a record and never the gate) and PreLLMCall (decision:block). HALT and REQUIRE_APPROVAL both render the same plain refusal; `continue` and `allow` are never written on any event. A real HALT verdict writes a run-keyed latch and later gated calls of that run, model calls included, replay it with no round trip. A delivery failure denies only its own call. Muse fails OPEN on an answer it does not accept, so the output contract is closed per event and a crash on a gated event exits non-zero (FaultExitCode) for Muse's onFailure successor. LIMITS: hooks run outside Muse's sandbox and approval layer, so --yolo does not bypass the gate, but hooks are a guardrail and not a tamper-proof boundary: a user can edit settings.json, and a malformed hooks file silently drops every handler from that source"},

		{Key: "model_call_gate", Supported: true, How: "every PreLLMCall is evaluated through /evaluate before the request is sent, as a model_call_gate activity of its own (activity id <session>:llmgate:<request_id>.<attempt>); PostLLMCall closes it with metadata only. It is a gate, not a record of the call: it carries no usage, no turn and no body, and a call with no PostLLMCall leaves the started row only (llmgate.go)"},

		{Key: "model_call.record", Supported: false, How: "NOT REPORTED by this adapter: PostLLMCall's usage and traceparent go to the local trace only (usage.go), never onto a DevEvent, because a gate is not a model-call record. Muse's own model-call recording belongs to a proxy or telemetry lane, and neither is built for Muse"},

		{Key: "telemetry.tokens", Supported: false, How: "NOT REPORTED; usage is taken from no hook payload onto the wire. PostLLMCall's usage stays in the local trace, and Muse's transcript format is undocumented"},

		{Key: "enforce.rewrite", Supported: true, How: "local secret redaction of a write/edit body -> updatedInput alone on the proceed path (never paired with an allow, never on PermissionRequest); gated on content posture. Unverified on a Muse binary: if Muse rejects updatedInput without a permissionDecision the answer is discarded and the call runs with its original input"},

		{Key: "commit.binding", Supported: false, How: "NOT REPORTED; Muse's shell-tool environment is undocumented, so no session marker reaches a commit hook and no commit is attributed to a Muse session"},
	}
}
