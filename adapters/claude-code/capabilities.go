package claudecode

// Capability is one entry in a provider's declared capability profile
// (architecture §1b). OpenBox core is written against the normalized event
// contract and never assumes a capability; an adapter DECLARES what it supports
// so a per-session coverage tier can be derived and displayed (no false sense of
// coverage).
type Capability struct {
	Key       string // stable capability id (§1b table)
	Supported bool
	How       string // one-line note on the mechanism / Phase-1 caveat
}

// Capabilities returns the Claude Code adapter's Phase-1 capability profile,
// grounded in spike S1 and architecture §1b. Two capabilities are
// provider-independent and always available (identity.register via agent/create,
// commit.binding via the git trailer). Phase-2 enforcement (verdict.apply) is
// declared-but-not-active in observe mode (D7/INV-3).
func Capabilities() []Capability {
	return []Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox dev init` (STORY-SL-2); provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/SessionEnd → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over all tools incl. mcp__* (matcher \"*\")"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer (STORY-SL-5); provider-independent"},
		{Key: "telemetry.tokens", Supported: false, How: "Claude Code hooks do not expose token/cost usage (Phase-2: parse transcript, gated)"},
		{Key: "verdict.apply", Supported: false, How: "Phase-1 observe treats every verdict as allow; never blocks (D7/INV-3)"},
	}
}
