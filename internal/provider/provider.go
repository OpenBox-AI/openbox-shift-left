// Package provider is the shared SPI between the `openbox` CLI and the per-
// tool adapters (Claude Code, Codex). Every recognized name has a built
// adapter: there is no not-built state, no stub, and no predicate that could
// only ever answer one way. An Installer receives only non-secret install-time
// context (the DID, URLs and posture); it must never receive or embed a
// credential value.
//   - Installer; install time: what `openbox init` delegates to an adapter to
package provider

import "errors"

// Name identifies a supported developer tool.
type Name string

const (
	ClaudeCode Name = "claude-code"
	Codex      Name = "codex"
)

// ErrUnknown means the provider name is not recognized at all.
var ErrUnknown = errors.New("unknown provider")

// CredentialRef is the non-secret install-time context an Installer needs:
// which identity this machine governs as, and the org posture to persist into
// the tool's dev config. It never carries a credential value (INV-1).
type CredentialRef struct {
	DID            string // did:aip:... (not secret)
	BaseURL        string // optional core base URL; empty ⇒ adapter default
	ContentCapture *bool  // org content posture; nil ⇒ the adapter default (content capture ON). Set to &false to pin metadata-only.
	InstallGitHook bool   // persist the ambient commit-hook install preference

	// AgentID is the backend PolicyEntity subject; the agent id `openbox dev
	// sync` and the session-start staleness check read to fetch this agent's
	// current policy.
	AgentID string
	// BackendURL is the openbox-backend control-plane base (distinct from
	// BaseURL, the core data-plane base).
	BackendURL string

	// ProjectDir selects project hook scope, which is what `openbox init` does by
	// default : the adapter merges its hook block into
	// <dir>/.claude/settings.local.json, so sessions in that project are governed
	// and sessions anywhere else are not.
	ProjectDir string

	// Enforce / Tier2 / Findings persist the enforce-mode posture chosen at
	// `openbox init` time into the dev
	// config, so the runtime hook reads them from dev.json and needs no runtime
	// environment variable.
	Enforce  *bool
	Tier2    *bool
	Findings *bool
}

// Installer writes one tool's native config, delegated from `init`.
type Installer interface {
	Name() Name
	// Install applies the config.
	Install(ref CredentialRef) error
}

// Supported lists the recognized provider names, sorted.
func Supported() []string {
	return []string{string(ClaudeCode), string(Codex)}
}
