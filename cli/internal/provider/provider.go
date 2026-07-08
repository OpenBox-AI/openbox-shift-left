// Package provider is the seam between `openbox dev init` (STORY-SL-2) and the
// per-tool adapter installers (STORY-SL-4 Claude Code, SL-7 Codex, SL-8 Cursor).
//
// SL-2 owns identity + credentials; it does NOT own provider config *content*.
// Each adapter story registers an Installer that writes its tool's native
// config. Under the delivery model (OD18/OD19) the Claude Code installer will
// install a native plugin bundling bin/openbox + hooks; Codex/Cursor lay down a
// config + managed-hooks bundle. Until an adapter is built, its Installer is
// Available()==false and only PRINTS the manual config the user must apply, and
// `dev init` exits non-zero for that provider (per the SL-2 acceptance criteria).
//
// INV-1: an Installer references the credential in the OS secret store
// (service + account); it MUST NOT receive or embed the secret value itself.
package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
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
// API key or private key value (INV-1).
type CredentialRef struct {
	SecretService     string // keychain service namespace
	APIKeyAccount     string // account holding the obx_ key
	PrivateKeyAccount string // account holding the Ed25519 seed
	DID               string // did:aip:... (not secret)
}

// Installer writes one tool's native config, delegated from `dev init`.
type Installer interface {
	Name() Name
	// Available reports whether this provider's adapter (config writer) is built.
	Available() bool
	// Plan returns a human-readable description of the config that would be
	// written (used for --dry-run and for the manual-config message when the
	// adapter is not yet built). It performs no writes.
	Plan(ref CredentialRef) string
	// Install applies the config, or returns ErrNotBuilt when !Available().
	Install(ref CredentialRef) error
}

var registry = map[Name]Installer{
	ClaudeCode: stub{name: ClaudeCode, manual: claudeCodeManual},
	Codex:      stub{name: Codex, manual: codexManual},
	Cursor:     stub{name: Cursor, manual: cursorManual},
}

// Lookup returns the Installer for a provider name, or ErrUnknown.
func Lookup(name string) (Installer, error) {
	if inst, ok := registry[Name(name)]; ok {
		return inst, nil
	}
	return nil, fmt.Errorf("%w: %q (supported: %s)", ErrUnknown, name, strings.Join(Supported(), ", "))
}

// Supported lists the recognized provider names, sorted.
func Supported() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}

// stub is a placeholder Installer for a provider whose adapter isn't built yet.
// It is Available()==false and only describes the manual config. Adapter stories
// (SL-4/SL-7/SL-8) replace these entries with real installers.
type stub struct {
	name   Name
	manual func(ref CredentialRef) string
}

func (s stub) Name() Name        { return s.name }
func (s stub) Available() bool   { return false }
func (s stub) Plan(ref CredentialRef) string { return s.manual(ref) }
func (s stub) Install(ref CredentialRef) error { return ErrNotBuilt }

func claudeCodeManual(ref CredentialRef) string {
	return fmt.Sprintf(`Claude Code adapter (STORY-SL-4) is not built yet.
Manual config until the plugin ships (OD18/OD19 — plugin bundles bin/openbox + hooks):
  - Install the OpenBox Claude Code plugin (marketplace) or add hooks in
    ~/.claude/settings.json: SessionStart, UserPromptSubmit, PreToolUse,
    PostToolUse, SessionEnd -> shell out to 'openbox' (observe-only, async).
  - The hooks read credentials from the OS secret store, not from config:
      service = %q
      api key account     = %q
      private key account = %q
      developer DID        = %s`,
		ref.SecretService, ref.APIKeyAccount, ref.PrivateKeyAccount, ref.DID)
}

func codexManual(ref CredentialRef) string {
	return fmt.Sprintf(`Codex adapter (STORY-SL-7) is not built yet.
Manual config until the bundle ships:
  - Lay down requirements.toml / MDM-managed hooks that invoke 'openbox'.
  - Credentials come from the OS secret store (service %q, DID %s), never inline.`,
		ref.SecretService, ref.DID)
}

func cursorManual(ref CredentialRef) string {
	return fmt.Sprintf(`Cursor adapter (STORY-SL-8) is not built yet.
Manual config until the bundle ships:
  - Add hooks.json / Team hooks over beforeSubmitPrompt, beforeMCPExecution,
    afterFileEdit that invoke 'openbox' (note: Cursor hooks fail-open).
  - Credentials come from the OS secret store (service %q, DID %s), never inline.`,
		ref.SecretService, ref.DID)
}
