package codex

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Capabilities returns the Codex adapter's capability profile.
//
// Floor: codex-cli >= 0.145.0. Every claim below that rests on live behaviour
// was measured against 0.150.0-alpha.8 and cites the probe record at
// plans/260917-0225-codex-parity-with-claude-code/probes/probe-record.md by
// probe id. A note with no test name, no file:line and no probe id is a claim
// nothing verifies, and does not belong here.
//
// The dashboard renders "not reported by this tool" as a first-class state, so a
// declared gap costs nothing; a synthesized value costs the audit trail its
// standing as evidence. Every gap below is therefore stated as scope, never as
// impossibility.
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},

		{Key: "telemetry.hook", Supported: true, How: "eleven events registered in hooks.json (installer.go hookedEvents): SessionStart, UserPromptSubmit, PreToolUse, PermissionRequest, PostToolUse, Stop, SubagentStart, SubagentStop, PreCompact, PostCompact, SessionEnd -> normalized events. Hooks are stable and ON by default (`codex features list` reports hooks/stable/true, probe P0.1), but they are HASH-TRUSTED: an untrusted hooks.json is silently inert and fires nothing (P0.1). Codex ignores unknown event names rather than rejecting the file, so adding an event needs no version guard (P0.9). Breadth parity with Claude Code is not achievable and is not claimed: Codex documents twelve hook events, Claude Code registers thirty-two"},

		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over Bash/apply_patch/mcp__* (matcher \"*\"), paired by tool_use_id (mapper.go mapTool; held by TestWire_ToolEventsAreActivityPairs). A tool blocked before it ran emits the started row only; that asymmetry is real and is never closed by fabricating a completion"},

		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer stamped from the CODEX_THREAD_ID exec env, which Codex injects into every exec (internal/adapters/common/git; hookrun.go reads obgit.EnvCodexThreadID). No liveness registry is needed for agent-made commits"},

		{Key: "tool.status", Supported: false, How: "NOT REPORTED; success is unknown, not assumed. Claude Code splits success and failure across two hooks (PostToolUse / PostToolUseFailure), which makes the outcome structural; Codex has one PostToolUse and no failure hook, and its payload carries no exit code and no error flag; `tool_response` is bound by no field here (INV-2). The vendor additionally documents PostToolUse as running after commands that exit non-zero, so success and failure are not distinguishable on this surface at all. Sending status:\"completed\" unconditionally would report SUCCESS 100% for a session whose calls failed, which is worse than the honest 0% it would replace, so the field is omitted and core's tool-success metric stays unpopulated for Codex. The upgrade path is a structural outcome field on the Codex hook payload, not a heuristic over tool_response. Verified against the payload shape the binary actually delivers (probe P0.6 / probes/raw/hook-contract-from-binary.md): post-tool-use.command.input declares tool_response and no status or exit-code field, and hookevent.go binds none"},

		{Key: "telemetry.tokens", Supported: true, How: "PER TURN (ResolveFinops, default on): Stop fires once per turn -- measured, 3 turns => 3 firings sharing one session_id with distinct turn_id (probe P0.7) -- and each emits an llm_completion activity pair under activity_id <session>:turn:N, or <session>:agent:<id>:turn:N for a subagent. The number is a DELTA between cumulative rollout snapshots, not a read of one: total_token_usage is cumulative and monotone, verified over a real 93-snapshot rollout where delta(total) equalled last_token_usage on every transition (P0.7b). The cursor advances only after both halves spool, making it exactly-once across repeated Stops. Cache counts are SUB-counts of input_tokens on Codex, unlike Claude Code's additive siblings, and are reported in their own fields rather than added in. The SessionEnd rollup (<session>:usage:rollup, PER SESSION) still ships, but ONLY for a session that emitted zero turns -- a crash, a kill, or a surface where the Stop hook never ran -- so the same tokens are never counted twice. Held by TestTurn_ThreeStopsEmitThreePairs, TestWire_TurnPairSharesOneActivityID and TestRollupIsGatedOnZeroTurns. Cost stays unreported; the Codex token path carries no cost field, and it is never derived from a pricing table"},

		{Key: "telemetry.model", Supported: true, How: "PER TURN: each turn pair carries the last non-empty turn_context.payload.model inside ITS window, never carried across windows and never back-filled from the session's start -- attributing a window's tokens to a model that may not have spent them is a fabricated number, the same class of error as deriving a cost. Omitted rather than guessed when a window names none (usage.go readTurnUsage; held by TestTurn_ThreeStopsEmitThreePairs). The SessionEnd rollup keeps session-level attribution for the zero-turn case"},

		{Key: "verdict.apply", Supported: true, How: "three gate classes, all decided by /evaluate, ON by default, tighten-only. (1) PreToolUse -> permissionDecision:deny + a non-empty reason. (2) UserPromptSubmit -> decision:block, and a session-terminating HALT additionally renders continue:false + stopReason; measured, Codex logs `UserPromptSubmit Blocked` versus `UserPromptSubmit Stopped` for the two shapes (probe P0.5). (3) PermissionRequest -> hookSpecificOutput.decision.behavior:deny only; it fires between PreToolUse and PostToolUse when a call needs escalated permission (P0.6). The first HALT writes a session-halt latch and every later gated call in that session replays it locally with ZERO evaluate round trips (TestPromptGate_HaltLatchesAndReplaysWithoutARoundTrip); a latch that will not parse still halts. REQUIRE_APPROVAL is held for a real decision and denies if unanswered -- the same hold Claude Code uses, not a Codex-specific mapping; Codex's parser additionally has no 'ask' verb to render. LIMITS, which any enforcement claim here is bounded by: the vendor's own guidance is to treat tool hooks as a useful guardrail and not a complete enforcement boundary; hosted tools never take the hook path; write_stdin does not re-run PreToolUse; a user-scope and a project-scope registration both fire rather than the higher one replacing the lower; and hook governance is defeated entirely by disabling the hooks feature or by bypassing hook trust, which only managed hooks plus allow_managed_hooks_only resists"},

		{Key: "enforce.rewrite", Supported: true, How: "local secret redaction -> permissionDecision:allow + updatedInput on the proceed path (tool_input[\"command\"]); allow rides only a redacting rewrite, never a grant; gated on content posture. Verified live end-to-end on 0.150.0-alpha.8: the rewritten command is what ran, and PostToolUse reported the rewritten tool_input (probe P0.11). Two bounds this depends on, both measured rather than assumed: a BARE allow (no updatedInput) is rejected by Codex and fails the call OPEN, and updatedInput's `command` must stay a JSON string or the call likewise fails open to the ORIGINAL input. On PermissionRequest there is no rewrite at all -- updatedInput, updatedPermissions and interrupt are reserved there and documented to fail closed -- so that surface is deny-or-nothing and never emits `allow`, which would skip the human approval prompt outright"},
	}
}
