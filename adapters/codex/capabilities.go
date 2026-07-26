package codex

// Capability is one entry in a provider's declared capability profile
// (architecture §1b). OpenBox core is written against the normalized event
// contract and never assumes a capability; an adapter DECLARES what it supports
// so a per-session coverage tier can be derived and displayed.
type Capability struct {
	Key       string // stable capability id (§1b table)
	Supported bool
	How       string // one-line note on the mechanism / caveat
}

// Capabilities returns the Codex adapter's capability profile, grounded in spike
// S5's v0.145.0 addendum and architecture §1b's Codex column. verdict.apply and
// enforce.rewrite flip true with the SL7-B enforce leg; telemetry.tokens stays
// false until the rollout-JSONL finops follow-up.
func Capabilities() []Capability {
	return []Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox dev init` (STORY-SL-2); provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/SessionEnd (hooks stable + ON by default >=0.145.0) → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over Bash/apply_patch/mcp__* (matcher \"*\"), paired by tool_use_id"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer stamped from the CODEX_THREAD_ID exec env (no liveness registry needed for agent-made commits)"},
		{Key: "telemetry.tokens", Supported: false, How: "hooks expose no usage; rollout-JSONL token_count extraction is the noted finops follow-up (SL-16 parity)"},
		{Key: "verdict.apply", Supported: true, How: "STORY-SL7-B enforce leg: PreToolUse in-process decision → permissionDecision:deny (opt-in, default observe); tighten-only; REQUIRE_APPROVAL→deny (Codex rejects 'ask', OD-SL7-ASK)"},
		{Key: "enforce.rewrite", Supported: true, How: "Tier-1 secret redaction → permissionDecision:allow + updatedInput on the proceed path (apply_patch tool_input[\"command\"]); allow rides ONLY a redacting rewrite, never a grant (OD-SL7-ALLOW-REWRITE); gated on content posture"},
	}
}
