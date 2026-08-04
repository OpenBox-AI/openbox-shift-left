// Package provider is the shared SPI between the `openbox` CLI and the per-tool
// adapters (Claude Code, Codex, Cursor). It has two halves:
//
//   - Installer — install time: what `openbox init` delegates to an adapter
//     to write that tool's native config.
//   - HookEngine — runtime: what the adapter does when its tool fires a hook,
//     plus the capability profile it declares.
//
// The package doc used to advertise "register / emit / apply / capabilities"
// while only Installer existed; emit and apply were per-adapter free functions
// the CLI reached through a hard-coded switch, and Capability was declared
// twice with no shared type. The interfaces here are now the ones the code
// actually has.
//
// It lives in its own module, importable by both the CLI module and every
// adapter module, so an adapter can implement the Installer interface without
// crossing the CLI's `internal/` boundary, and the CLI can register an adapter
// without adapter internals leaking the other way. The concrete registry
// (which names map to which installers) lives in the CLI's composition root,
// not here: putting it in this package would force it to import the adapters,
// while the adapters import this package — an import cycle. This module
// therefore has zero dependencies.
//
// `init` owns identity + credentials; it does not own provider config
// *content*. Each adapter registers an Installer that writes its tool's
// native config. Until an adapter is built, its slot is a Stub:
// Available()==false, Plan() only prints the manual config the user must
// apply, and Install() returns ErrNotBuilt so `init` exits non-zero for
// that provider.
//
// An Installer references the credential in the OS secret store (service +
// account coordinates + the non-secret DID); it must not receive or embed the
// secret value itself.
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

// CredentialRef points an Installer at where the agent credentials live in the
// secret store. It carries the non-secret DID for convenience but never the
// API key or private key value (INV-1). Coordinate fields are the same values
// `openbox init` (SL-2) wrote into the OS secret store; BaseURL and
// ContentCapture are org-posture defaults the installer persists into the
// tool's non-secret dev config.
type CredentialRef struct {
	SecretService     string // keychain service namespace
	APIKeyAccount     string // account holding the obx_ key
	PrivateKeyAccount string // account holding the Ed25519 seed
	DID               string // did:aip:... (not secret)
	BaseURL           string // optional core base URL; empty ⇒ adapter default
	ContentCapture    *bool  // org content posture; nil ⇒ the adapter default (content capture ON). Set to &false to pin metadata-only.
	InstallGitHook    bool   // persist the ambient commit-hook install preference

	// AgentID is the backend PolicyEntity subject — the agent id `openbox dev
	// sync` and the session-start staleness check read to fetch this agent's
	// current policy. Non-secret, persisted to dev.json.
	AgentID string
	// BackendURL is the openbox-backend control-plane base (distinct from
	// BaseURL, the core data-plane base). Persisted so `dev sync` / staleness
	// can reach the policy read endpoint without re-supplying
	// OPENBOX_BACKEND_URL. Non-secret.
	BackendURL string

	// LocalHooksDir is the opt-in LOCAL-TESTING hook scope (`init
	// --local-hooks <project-dir>`): the adapter additionally merges its
	// hook block into <dir>/.claude/settings.local.json so ONLY sessions in
	// that project are governed. Empty (the default) keeps the production
	// posture: hooks activate via the plugin / managed or global settings,
	// never a project file. Non-secret.
	LocalHooksDir string

	// Enforce / Tier2 / Findings persist the enforce-mode posture chosen at
	// `openbox init` time (via --enforce and its granular siblings) into
	// the dev config, so the runtime hook reads them from dev.json and needs
	// no runtime environment variable. Enforcement stays opt-in: a first
	// install with none of them set is observe-only.
	//
	// All three are *bool because nil has to mean "this run did not say",
	// distinct from "this run said false". Enforce used to be a plain bool,
	// which made a plain `init` indistinguishable from `init` asking
	// for observe — so re-running it to repair hooks silently dropped a
	// developer out of enforce mode. nil now preserves what is already on
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
