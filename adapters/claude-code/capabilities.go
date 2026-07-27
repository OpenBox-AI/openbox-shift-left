package claudecode

// Capability is one entry in a provider's declared capability profile.
// OpenBox core is written against the normalized event contract and never
// assumes a capability; an adapter declares what it supports so a
// per-session coverage tier can be derived and displayed (no false sense
// of coverage).
type Capability struct {
	Key       string // stable capability id
	Supported bool
	How       string // one-line note on the mechanism / caveat
}

// Capabilities returns the Claude Code adapter's capability profile. Two
// capabilities are provider-independent and always available
// (identity.register via agent/create, commit.binding via the git
// trailer). verdict.apply is declared-but-not-active here: the observe
// path always treats every verdict as allow (INV-3); the separate enforce
// mode is opt-in and out of this profile's scope.
func Capabilities() []Capability {
	return []Capability{
		{Key: "identity.register", Supported: true, How: "agent/create via `openbox dev init`; provider-independent"},
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/SessionEnd → normalized events"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over all tools incl. mcp__* (matcher \"*\")"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer; provider-independent"},
		{Key: "telemetry.tokens", Supported: false, How: "Claude Code hooks do not expose token/cost usage (parsed from the transcript, gated)"},
		{Key: "verdict.apply", Supported: false, How: "observe mode treats every verdict as allow; never blocks (INV-3)"},
	}
}
