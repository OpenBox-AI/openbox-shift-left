// Package provider is the shared SPI between the `openbox` CLI and the per-tool
// adapters (Claude Code, Codex, Cursor). It has two halves:
//
// - Installer — install time: what `openbox init` delegates to an adapter to
// write that tool's native config. - HookEngine — runtime: what the adapter does
// when its tool fires a hook, plus the capability profile it declares.
//
// The package doc used to advertise "register / emit / apply / capabilities"
// while only Installer existed; emit and apply were per-adapter free functions
// the CLI reached through a hard-coded switch, and Capability was declared twice
// with no shared type. The interfaces here are now the ones the code actually
// has.
//
// It lives in its own module, importable by both the CLI module and every
// adapter module, so an adapter can implement the Installer interface without
// crossing the CLI's `internal/` boundary, and the CLI can register an adapter
// without adapter internals leaking the other way. The concrete registry (which
// names map to which installers) lives in the CLI's composition root, not here:
// putting it in this package would force it to import the adapters, while the
// adapters import this package — an import cycle. This module therefore has zero
// dependencies.
//
// `init` owns setup — hooks at a chosen scope, plus posture. It does NOT own
// credentials (that is `openbox auth`) and it does not own provider config
// *content*. Each adapter registers an Installer that writes its tool's native
// config. Until an adapter is built, its slot is a Stub: Available()==false,
// Plan() only prints the manual config the user must apply, and Install()
// returns ErrNotBuilt so `init` exits non-zero for that provider.
//
// An Installer receives only non-secret install-time context (the DID, URLs and
// posture); it must never receive or embed a credential value. Since that
// decision it does not even receive a credential ADDRESS: credentials live in
// ~/.openbox/.env, written by `openbox auth`, and are resolved at read time.
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
// has not shipped yet. `init` surfaces the manual config and exits non-zero.
var ErrNotBuilt = errors.New("provider adapter not built yet")

// ErrUnknown means the provider name is not recognized at all.
var ErrUnknown = errors.New("unknown provider")

// CredentialRef is the non-secret install-time context an Installer needs: which
// identity this machine governs as, and the org posture to persist into the
// tool's dev config. It never carries a credential value (INV-1).
//
// It no longer carries secret-store coordinates. Before that decision it named a
// keychain service and two account paths, because the credential lived in the OS
// store and only its address was safe to pass around. Credentials now resolve
// from ~/.openbox/.env at read time, so there is no address to hand an installer
// — and `openbox auth`, not `openbox init`, is what writes them.
type CredentialRef struct {
	DID            string // did:aip:... (not secret)
	BaseURL        string // optional core base URL; empty ⇒ adapter default
	ContentCapture *bool  // org content posture; nil ⇒ the adapter default (content capture ON). Set to &false to pin metadata-only.
	InstallGitHook bool   // persist the ambient commit-hook install preference

	// AgentID is the backend PolicyEntity subject — the agent id `openbox dev
	// sync` and the session-start staleness check read to fetch this agent's
	// current policy. Non-secret, persisted to dev.json.
	AgentID string
	// BackendURL is the openbox-backend control-plane base (distinct from
	// BaseURL, the core data-plane base). Persisted so `dev sync` / staleness
	// can reach the policy read endpoint without re-supplying
	// OPENBOX_BACKEND_URL. Non-secret.
	BackendURL string

	// ProjectDir selects PROJECT hook scope, which is what `openbox init` does
	// by default : the adapter merges its hook block into
	// <dir>/.claude/settings.local.json, so sessions in that project are
	// governed and sessions anywhere else are not.
	//
	// Empty means GLOBAL scope: the bundle is still installed and posture is
	// still written, but activation waits on a managed-settings deployment the
	// CLI cannot perform itself. Scope selects activation, not location — the
	// bundle, the engine binary and the config are written either way.
	//
	// It was LocalHooksDir, described as an opt-in for local testing, back when
	// project scope was the exception and the flag text said "never set this in
	// production". That decision inverted that, so the name and the comment had
	// to stop describing the default as an escape hatch. Non-secret.
	ProjectDir string

	// Enforce / Tier2 / Findings persist the enforce-mode posture chosen at
	// `openbox init` time (via --enforce and its granular siblings) into the
	// dev config, so the runtime hook reads them from dev.json and needs no
	// runtime environment variable. Enforce now DEFAULTS ON, so a first install
	// with none of these set enforces; `--enforce=false` opts out and the
	// opt-out persists.
	//
	// All three are *bool because nil has to mean "this run did not say",
	// distinct from "this run said false". nil preserves what is already on
	// disk; only an explicit false turns enforcement off.
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
	// adapter is not yet built). It performs no writes and prints no secret.
	Plan(ref CredentialRef) string
	// Install applies the config, or returns ErrNotBuilt when !Available().
	Install(ref CredentialRef) error
}

// Supported lists the recognized provider names, sorted. These are the names
// `init --provider` accepts regardless of whether each adapter is built yet.
func Supported() []string {
	return []string{string(ClaudeCode), string(Codex), string(Cursor)}
}

// Stub is the Installer for a recognized provider whose adapter is not built
// yet. It is Available()==false and only describes the manual config. A real
// adapter replaces its Stub with a real installer in the CLI registry. The
// Manual func renders provider-specific manual-config guidance.
type Stub struct {
	ProviderName Name
	Manual       func(ref CredentialRef) string
}

func (s Stub) Name() Name      { return s.ProviderName }
func (s Stub) Available() bool { return false }

// Plan renders the provider-specific manual-config guidance. A Stub built
// without a Manual func falls back to a generic message rather than panicking,
// so a future adapter author who forgets to set it gets a usable --dry-run
// instead of a nil-func crash on the observe path.
func (s Stub) Plan(ref CredentialRef) string {
	if s.Manual == nil {
		return fmt.Sprintf("provider %q adapter is not built yet; no manual config available.", s.ProviderName)
	}
	return s.Manual(ref)
}
func (s Stub) Install(ref CredentialRef) error { return ErrNotBuilt }
