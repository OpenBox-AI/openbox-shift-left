// Package providers is the CLI's composition root for the install-time SPI: it
// binds each recognized provider name (from the shared `provider` module) to a
// concrete Installer. This is the one place that imports both the shared SPI and
// the adapter modules; keeping it in `cli` (not in the shared module) is what
// breaks the would-be import cycle between the SPI and its adapters (ADR-0001).
//
// SL4-WIRE-1: claude-code is now a REAL installer (claudecode.Installer) that
// materializes the plugin bundle + non-secret dev config, replacing the SL-2
// stub. Codex/Cursor stay stubs until SL-7/SL-8 build their adapters.
package providers

import (
	"fmt"
	"strings"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	"github.com/openbox-ai/openbox-shift-left/provider"
)

// registry maps a provider name to its Installer. Built adapters register a real
// installer; unbuilt ones a provider.Stub (Available()==false).
var registry = map[provider.Name]provider.Installer{
	provider.ClaudeCode: claudecode.Installer{}, // SL-4 real installer (default install paths)
	provider.Codex:      provider.Stub{ProviderName: provider.Codex, Manual: codexManual},
	provider.Cursor:     provider.Stub{ProviderName: provider.Cursor, Manual: cursorManual},
}

// Lookup returns the Installer for a provider name, or ErrUnknown.
func Lookup(name string) (provider.Installer, error) {
	if inst, ok := registry[provider.Name(name)]; ok {
		return inst, nil
	}
	return nil, fmt.Errorf("%w: %q (supported: %s)", provider.ErrUnknown, name, strings.Join(provider.Supported(), ", "))
}

func codexManual(ref provider.CredentialRef) string {
	return fmt.Sprintf(`Codex adapter (STORY-SL-7) is not built yet.
Manual config until the bundle ships:
  - Lay down requirements.toml / MDM-managed hooks that invoke 'openbox'.
  - Credentials come from the OS secret store (service %q, DID %s), never inline.`,
		ref.SecretService, ref.DID)
}

func cursorManual(ref provider.CredentialRef) string {
	return fmt.Sprintf(`Cursor adapter (STORY-SL-8) is not built yet.
Manual config until the bundle ships:
  - Add hooks.json / Team hooks over beforeSubmitPrompt, beforeMCPExecution,
    afterFileEdit that invoke 'openbox' (note: Cursor hooks fail-open).
  - Credentials come from the OS secret store (service %q, DID %s), never inline.`,
		ref.SecretService, ref.DID)
}
