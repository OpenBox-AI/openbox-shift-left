package codex

import (
	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

// Capabilities returns the Codex adapter's capability profile, grounded in
// the v0.145.0 hooks addendum.
func Capabilities() []providerspi.Capability {
	return []providerspi.Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox init`; provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/SessionEnd (hooks stable + ON by default >=0.145.0) → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over Bash/apply_patch/mcp__* (matcher \"*\"), paired by tool_use_id"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer stamped from the CODEX_THREAD_ID exec env (no liveness registry needed for agent-made commits)"},
		{Key: "telemetry.tokens", Supported: true, How: "opt-in (ResolveFinops, default off) rollout-JSONL total_token_usage extraction at SessionEnd → client.Tokens on SessionEnded; numbers-only (INV-2), off the hot path. Cost stays unreported — the Codex token path carries no cost field"},
		{Key: "verdict.apply", Supported: true, How: "enforce leg: PreToolUse in-process decision → permissionDecision:deny (opt-in, default observe); tighten-only; REQUIRE_APPROVAL→deny (Codex rejects 'ask')"},
		{Key: "enforce.rewrite", Supported: true, How: "Tier-1 secret redaction → permissionDecision:allow + updatedInput on the proceed path (apply_patch tool_input[\"command\"]); allow rides only a redacting rewrite, never a grant; gated on content posture"},
	}
}
