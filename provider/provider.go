// Package provider is the shared install-time SPI between `openbox dev init`
// (STORY-SL-2) and the per-tool adapter installers (SL-4 Claude Code, SL-7
// Codex, SL-8 Cursor). It is the "install half" of the generic adapter seam
// (architecture §1b: register / emit / apply / capabilities).
//
// It lives in its OWN module — importable by both the `cli` module and every
// adapter module — so an adapter can implement the Installer interface without
// crossing the CLI's `internal/` boundary, and the CLI can register an adapter
// without depending on adapter internals leaking the other way. The concrete
// registry (which names map to which installers) is the CLI's composition root,
// NOT this module: putting it here would force this module to import the
// adapters, and the adapters import this module — an import cycle. See
// ADR-0001. This module therefore has zero dependencies.
//
// SL-2 owns identity + credentials; it does NOT own provider config *content*.
// Each adapter story registers an Installer that writes its tool's native
// config. Until an adapter is built, its slot is a Stub: Available()==false,
// Plan() only PRINTS the manual config the user must apply, and Install()
// returns ErrNotBuilt so `dev init` exits non-zero for that provider.
//
// INV-1: an Installer references the credential in the OS secret store
// (service + account coordinates + the non-secret DID); it MUST NOT receive or
// embed the secret value itself.
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
// has not shipped yet. `dev init` surfaces the manual config and exits non-zero.
var ErrNotBuilt = errors.New("provider adapter not built yet")

// ErrUnknown means the provider name is not recognized at all.
var ErrUnknown = errors.New("unknown provider")

// CredentialRef points an Installer at where the agent credentials live in the
// secret store. It carries the non-secret DID for convenience but never the
// API key or private key value (INV-1). Coordinate fields are the same values
// `openbox dev init` (SL-2) wrote into the OS secret store; BaseURL and
// ContentCapture are org-posture defaults the installer persists into the
// tool's non-secret dev config.
type CredentialRef struct {
	SecretService     string // keychain service namespace
	APIKeyAccount     string // account holding the obx_ key
	PrivateKeyAccount string // account holding the Ed25519 seed
	DID               string // did:aip:... (not secret)
	BaseURL           string // optional core base URL; empty ⇒ adapter default
	ContentCapture    bool   // org content posture (default false = metadata-only, INV-2)
	InstallGitHook    bool   // STORY-SL-5: persist the ambient commit-hook install preference
}

// Installer writes one tool's native config, delegated from `dev init`.
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
// `dev init --provider` accepts regardless of whether each adapter is built yet.
func Supported() []string {
	return []string{string(ClaudeCode), string(Codex), string(Cursor)}
}

// Stub is the Installer for a recognized provider whose adapter is not built
// yet. It is Available()==false and only describes the manual config. Adapter
// stories (SL-4/SL-7/SL-8) replace their Stub with a real installer in the CLI
// registry. The Manual func renders provider-specific manual-config guidance.
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
