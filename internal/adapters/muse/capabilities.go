package muse

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Capabilities returns the Muse adapter's capability profile.
//
// Every claim below rests on Muse's published hook documentation (Muse Code SDK
// hook-events reference and the 1.4.0 changelog) and on scrubbed captures of a
// Muse 1.4.1 session (testdata/README.md); what those captures did not show is
// listed in README.md and the notes say so where it matters. A gap is
// stated as scope, never as impossibility: the dashboard renders "not reported
// by this tool" as a first-class state, and a synthesized value costs the audit
// trail its standing as evidence.
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},

		{Key: "telemetry.hook", Supported: true, How: "sixteen events map to contract types (hookevent.go): SessionStart, UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse, PostToolUseFailure, SubagentStart, SubagentStop, StopFailure, SessionEnd, PreLLMCall, PostLLMCall, PreCompact, PostCompact, Notification and Stop (a subagent runs under its own session id and is folded into the session that spawned it, found through the parent journal's link record: its start is a SubagentStarted there, its stop ends nothing, and every row is tagged agent_id and agent_type; a child with no link stays a session of its own. A session with no known run, or one SessionEnd already sealed as on `muse resume`, is opened before its first event). Stop reports the turn: a TurnStarted/TurnCompleted pair whose reply is the payload's last_assistant_message and whose thinking is the reasoning summaries read from the session journal, both content-gated and carrying no tokens (turn.go); it also runs the session-log reconciler. PreCompact, PostCompact and Notification are unpaired signals that ride an open run and never open one (a PreCompact for an internal session is dropped with a trace finding); trigger is Muse's own soft|hard. PostToolBatch never fires on Muse 1.4.2 and Interrupt is refused unless async, so neither is installed. Payload shapes were read off Muse 1.4.1 and 1.4.2 captures; anything not captured there is unverified (see README.md)"},

		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse/PostToolUseFailure over every tool, MCP included (installed with no matcher; mcp__<server>__<tool> -> ToolMCP), paired by tool_use_id (mapper.go mapTool). A tool blocked before it ran emits the started row only, and that asymmetry is never closed by fabricating a completion. Whether an MCP call fires PreToolUse is documented by implication only; a payload over 256 KiB is never delivered to any hook"},

		{Key: "tool.status", Supported: true, How: "structural: PostToolUse is completed and PostToolUseFailure is failed, two distinct hooks, so the outcome is never inferred from a response body"},

		{Key: "verdict.apply", Supported: true, How: "four gate classes, all decided by /evaluate, tighten-only: PreToolUse (permissionDecision:deny), UserPromptSubmit (decision:block), PermissionRequest (behavior:deny, a record and never the gate) and PreLLMCall (decision:block). HALT and REQUIRE_APPROVAL both render the same plain refusal; `continue` and `allow` are never written on any event. A real HALT verdict writes a run-keyed latch and later gated calls of that run, model calls included, replay it with no round trip. A delivery failure denies only its own call. Muse fails OPEN on an answer it does not accept, so the output contract is closed per event and a crash on a gated event exits non-zero (FaultExitCode) for Muse's onFailure successor. LIMITS: hooks run outside Muse's sandbox and approval layer, so --yolo does not bypass the gate, but hooks are a guardrail and not a tamper-proof boundary: a user can edit settings.json, and a malformed hooks file silently drops every handler from that source"},

		{Key: "model_call_gate", Supported: true, How: "every PreLLMCall is evaluated through /evaluate before the request is sent, as a model_call_gate activity of its own (activity id <session>:llmgate:<request_id>.<attempt>); PostLLMCall closes it with metadata only. It is a gate, not a record of the call: it carries no usage, no turn and no body, and a call with no PostLLMCall leaves the started row only (llmgate.go)"},

		{Key: "model_call.record", Supported: false, How: "NOT REPORTED by this adapter: PostLLMCall's usage and traceparent go to the local trace only (usage.go), never onto a DevEvent, because a gate is not a model-call record. Muse's model calls are recorded by the telemetry lane instead (internal/cli/telemetryemit MuseFieldMap, an :otel: row), outside this adapter; Muse has no proxy lane"},

		{Key: "telemetry.tokens", Supported: false, How: "NOT REPORTED by this adapter; usage is taken from no hook payload onto the wire, and PostLLMCall's usage stays in the local trace. Token counts reach core on the telemetry lane's :otel: model-call row, from Muse's own export"},

		{Key: "enforce.rewrite", Supported: true, How: "local secret redaction of a write/edit body -> updatedInput alone on the proceed path (never paired with an allow, never on PermissionRequest); gated on content posture. Unverified on a Muse binary: if Muse rejects updatedInput without a permissionDecision the answer is discarded and the call runs with its original input"},

		{Key: "commit.binding", Supported: false, How: "NOT REPORTED; Muse's shell-tool environment is undocumented, so no session marker reaches a commit hook and no commit is attributed to a Muse session"},
	}
}
