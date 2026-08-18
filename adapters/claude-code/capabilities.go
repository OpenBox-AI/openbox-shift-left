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
		{Key: "telemetry.hook", Supported: true, How: "SessionStart/UserPromptSubmit/Pre+PostToolUse/PostToolUseFailure/Stop/SubagentStop/SubagentStart/PermissionDenied/StopFailure/SessionEnd → normalized events. The four failure/lifecycle hooks are new in ADR-0018 and are registered by the installer, so an EXISTING install does not fire them until `openbox init` is re-run; an older Claude Code that does not know the keys never invokes them and the events are simply absent (fail-open)"},
		{Key: "tool.events", Supported: true, How: "PreToolUse/PostToolUse over all tools incl. mcp__* (matcher \"*\"), plus PostToolUseFailure for the failure half — the two post-hooks are mutually exclusive per call, which is what makes the outcome structural"},
		{Key: "tool.status", Supported: true, How: "status completed|failed on every tool ActivityCompleted (ADR-0018), derived from WHICH hook fired — PostToolUse is documented and verified to fire only after a successful tool, PostToolUseFailure only after a failure. Never parsed from tool output; not content-gated, so it ships identically with content_capture off. metadata.is_interrupt (tri-state) separates a user cancellation from a real tool failure — both are `failed`, and an operator needs to tell them apart"},
		{Key: "commit.binding", Supported: true, How: "OpenBox-Session git trailer; provider-independent"},
		{Key: "telemetry.tokens", Supported: true, How: "PER TURN (ResolveFinops, default on): Stop/SubagentStop → llm_completion activity pair carrying model + 4 token counts (ADR-0014), plus the SessionEnd session rollup. Window sums, not per-model-call — hooks fire per turn. Transcript projection binds one egressing string (model), allowlist-enforced (INV-2); off the hot path. Cost never derived here"},
		{Key: "telemetry.model", Supported: true, How: "per-turn model attribution from the transcript's message.model (last non-empty in the window); omitted rather than guessed when a window names none. SessionStart's model field is separate and not guaranteed present"},
		{Key: "verdict.apply", Supported: true, How: "enforce leg: every gated PreToolUse call AND every UserPromptSubmit prompt is evaluated by /evaluate (ADR-0017, ADR-0020) → permissionDecision deny/ask on tools, decision:block on prompts; ON by default (ADR-0016); tighten-only, never emits allow as a grant; REQUIRE_APPROVAL is held for a real decision and denies/blocks if unanswered. A HALT the control plane returns additionally stops the session: continue:false now, and a local latch refuses every later prompt/tool call in that session (resume included). Prompt gating and the raised UserPromptSubmit timeout are installer-registered — an existing install needs `openbox init` re-run"},
		{Key: "enforce.rewrite", Supported: true, How: "local secret redaction → permissionDecision:allow + updatedInput on the proceed path (Write/Edit bodies); allow rides only a redacting rewrite; gated on content posture"},
	}
}
