package claudecode

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

// Capabilities returns the Claude Code adapter's capability profile. Two
// capabilities are provider-independent and always available
// (identity.register via agent/create, commit.binding via the git trailer).
//
// "Supported" means the adapter implements the mechanism, not that it is
// active in a given session: both verdict.apply and telemetry.tokens are
// opt-in and default off, so an unconfigured session still observes only
// (INV-3). Keep this profile in step with contracts/dev-event/COVERAGE.md —
// a profile that undersells shipped capability is as misleading as one that
// oversells it (report SL-07).
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/Stop/SubagentStop/SessionEnd → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over all tools incl. mcp__* (matcher \"*\")"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer; provider-independent"},
		{Key: "telemetry.tokens", Supported: true, How: "PER TURN (ResolveFinops, default on): Stop/SubagentStop → llm_completion activity pair carrying model + 4 token counts (ADR-0014), plus the SessionEnd session rollup. Window sums, not per-model-call — hooks fire per turn. Transcript projection binds one egressing string (model), allowlist-enforced (INV-2); off the hot path. Cost never derived here"},
		{Key: "telemetry.model", Supported: true, How: "per-turn model attribution from the transcript's message.model (last non-empty in the window); omitted rather than guessed when a window names none. SessionStart's model field is separate and not guaranteed present"},
		{Key: "verdict.apply", Supported: true, How: "enforce leg: PreToolUse in-process decision → permissionDecision deny/ask (opt-in, default observe); tighten-only, never emits allow as a grant; REQUIRE_APPROVAL→ask (CC has an HITL prompt)"},
		{Key: "enforce.rewrite", Supported: true, How: "Tier-1 secret redaction → permissionDecision:allow + updatedInput on the proceed path (Write/Edit bodies); allow rides only a redacting rewrite; gated on content posture"},
	}
}
