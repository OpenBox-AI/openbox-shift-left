package codex

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Capabilities returns the Codex adapter's capability profile, grounded in
// the v0.145.0 hooks addendum.
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/SessionEnd (hooks stable + ON by default >=0.145.0) → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over Bash/apply_patch/mcp__* (matcher \"*\"), paired by tool_use_id"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer stamped from the CODEX_THREAD_ID exec env (no liveness registry needed for agent-made commits)"},
		{Key: "tool.status", Supported: false, How: "NOT REPORTED — success is unknown, not assumed. Claude Code splits success and failure across two hooks (PostToolUse / PostToolUseFailure), which makes the outcome structural; Codex v0.145.0 has one PostToolUse and no failure hook, and its payload carries no exit code and no error flag — `tool_response` is bound by no field here (INV-2). Sending status:\"completed\" unconditionally would report SUCCESS 100% for a session whose calls failed, which is worse than the honest 0% it would replace, so the field is omitted and core's tool-success metric stays unpopulated for Codex. The upgrade path is a structural outcome field on the Codex hook payload, not a heuristic over tool_response"},
		{Key: "telemetry.tokens", Supported: true, How: "PER SESSION (ResolveFinops, default on): rollout-JSONL total_token_usage at SessionEnd → an llm_completion activity pair (activity_id <session>:usage:rollup) plus client.Tokens on SessionEnded, all four counts. NOT per turn: Codex's Stop hook exists in v0.145.0 and is deliberately unwired here — scope, not impossibility; the upgrade path is to wire Stop and take the per-turn delta from last_token_usage. Cost stays unreported — the Codex token path carries no cost field, and it is never derived from a pricing table"},
		{Key: "telemetry.model", Supported: true, How: "session-level model attribution from the rollout's turn_context.payload.model (last non-empty = the model in effect at session end); omitted rather than guessed when the rollout names none. No per-turn model attribution, for the same reason usage is per-session"},
		{Key: "verdict.apply", Supported: true, How: "enforce leg: every gated PreToolUse call is evaluated by /evaluate → permissionDecision:deny; ON by default; tighten-only; REQUIRE_APPROVAL→deny (Codex rejects 'ask')"},
		{Key: "enforce.rewrite", Supported: true, How: "local secret redaction → permissionDecision:allow + updatedInput on the proceed path (apply_patch tool_input[\"command\"]); allow rides only a redacting rewrite, never a grant; gated on content posture"},
	}
}
