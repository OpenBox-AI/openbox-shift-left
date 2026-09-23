// Package sessionkey names, per governed provider and per lane, where that
// provider's session identity carrier lives on the wire. It is the one place
// that knowledge is written down, so a lane's own emitter or mapper reads a
// header or an attribute name through here rather than repeating it by hand
// -- and so a later caller (the cross-lane HALT decorator) can ask the same
// question a lane's own emitter already answers, without duplicating the
// table.
//
// Scope: hooks, OTel and the proxy lane's per-provider carrier. No
// `chat:<surface>:<uuid>` namespace and no `pac_activated` election input --
// both belong to a chat session key, out of scope here.
//
// This package never mints a session key. Every Resolve-shaped function
// returns ok=false for an absent or unusable carrier; the caller's existing
// skip-and-count behavior is unchanged (a headerless call to an API host
// stays skipped and counted).
package sessionkey

// Provider names a governed tool. The string values match
// internal/provider's Name constants (and hostTable's own keys in
// internal/transport) so a caller already holding one of those can convert
// with a plain string cast; this package does not import internal/provider
// itself; the value is a name comparison, not a capability lookup, and every
// guarded subtree (internal/transport, internal/gateway) is one string cast
// away from it without a new import.
type Provider string

const (
	// ClaudeCode is Anthropic's own CLI/IDE tool.
	ClaudeCode Provider = "claude-code"
	// Codex is OpenAI's CLI tool.
	Codex Provider = "codex"
)

// Lane identifies which channel a carrier is read from. The three lanes this
// package resolves for; a chat lane is not modeled here.
type Lane int

const (
	// LaneHooks is the tool's own hook payloads (PreToolUse, SessionStart, ...).
	LaneHooks Lane = iota
	// LaneOTel is the tool's OpenTelemetry export, received by `openbox telemetry`.
	LaneOTel
	// LaneProxy is a relayed model call intercepted by `openbox transport`.
	LaneProxy
)

func (l Lane) String() string {
	switch l {
	case LaneHooks:
		return "hooks"
	case LaneOTel:
		return "otel"
	case LaneProxy:
		return "proxy"
	default:
		return "unknown"
	}
}

// HooksKey is the hook payload field every provider's hook carries its
// session identity on. Both providers key hooks on session_id: a Codex root
// session's hook session_id equals its own thread id by construction
// (codex-rs core/src/session/session.rs:910-920, pinned by
// core/tests/suite/hooks.rs:4647); a non-root (subagent) hook keys on the
// PARENT session's id instead, a distinction the adapter -- not this
// constant -- must already account for.
const HooksKey = "session_id"

// defaultOTelAttr is what every OTel exporter before Codex's arm existed
// read: Claude Code's own `session.id` resource attribute.
const defaultOTelAttr = "session.id"

// codexOTelAttr is what Codex's own OTel export carries instead: Codex has
// no `session.id` attribute at all, only `conversation.id`.
const codexOTelAttr = "conversation.id"

// OTelAttr returns the OTel attribute a provider's turn exports its session
// identity under: "session.id" for Claude Code (and any provider this table
// does not otherwise name -- the default every caller had before this
// function existed), "conversation.id" for Codex.
func OTelAttr(p Provider) string {
	if p == Codex {
		return codexOTelAttr
	}
	return defaultOTelAttr
}
