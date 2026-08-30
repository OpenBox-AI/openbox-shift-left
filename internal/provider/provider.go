// Package provider is the shared SPI between the `openbox` CLI and the per-
// tool adapters (Claude Code, Codex, Cursor). It has two halves: Until an
// adapter is built, its slot is a Stub: Available()==false, Plan() only prints
// the manual config the user must apply, and Install() returns ErrNotBuilt so
// `init` exits non-zero for that provider. An Installer receives only non-
// secret install-time context (the DID, URLs and posture); it must never
// receive or embed a credential value.
package provider

import (
	"errors"
	"fmt"
)

// Name identifies a supported developer tool.
type Name string

const (
	ClaudeCode Name = "claude-code"
	Codex      Name = "codex"
	Cursor     Name = "cursor"
)

// ErrNotBuilt means the provider is recognized but its adapter (config writer)
// has not shipped yet.
var ErrNotBuilt = errors.New("provider adapter not built yet")

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
	// `openbox init` time (via --enforce and its granular siblings) into the dev
	// config, so the runtime hook reads them from dev.json and needs no runtime
	// environment variable.
	Enforce  *bool
	Tier2    *bool
	Findings *bool
}

// Installer writes one tool's native config, delegated from `init`.
type Installer interface {
	Name() Name
	// Available reports whether this provider's adapter (config writer) is built.
	Available() bool
	// Plan returns a human-readable description of the config that would be
	// written (used for --dry-run and for the manual-config message when the
	// adapter is not yet built).
	Plan(ref CredentialRef) string
	// Install applies the config, or returns ErrNotBuilt when !Available().
	Install(ref CredentialRef) error
}

// Supported lists the recognized provider names, sorted.
func Supported() []string {
	return []string{string(ClaudeCode), string(Codex), string(Cursor)}
}

// Stub is the Installer for a recognized provider whose adapter is not built
// yet.
type Stub struct {
	ProviderName Name
	Manual       func(ref CredentialRef) string
}

func (s Stub) Name() Name      { return s.ProviderName }
func (s Stub) Available() bool { return false }

// Plan renders the provider-specific manual-config guidance.
func (s Stub) Plan(ref CredentialRef) string {
	if s.Manual == nil {
		return fmt.Sprintf("provider %q adapter is not built yet; no manual config available.", s.ProviderName)
	}
	return s.Manual(ref)
}
func (s Stub) Install(ref CredentialRef) error { return ErrNotBuilt }
