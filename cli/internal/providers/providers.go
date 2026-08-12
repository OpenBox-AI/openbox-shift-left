// Package providers is the CLI's composition root for the install-time SPI:
// it binds each recognized provider name (from the shared `provider`
// module) to a concrete Installer. This is the one place that imports both
// the shared SPI and the adapter modules; keeping it in `cli` (not in the
// shared module) is what breaks the would-be import cycle between the SPI
// and its adapters (ADR-0001).
//
// claude-code and codex are real installers (hooks.json/dev.json, no
// bundle); Cursor stays a stub until it ships.
package providers

import (
	"fmt"
	"os"
	"strings"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	codex "github.com/openbox-ai/openbox-shift-left/adapters/codex"
	"github.com/openbox-ai/openbox-shift-left/provider"
)

// Engine returns the runtime hook engine for a provider name, or ErrUnknown.
//
// This is the other half of what Lookup does for install time. The CLI used to
// reach the adapters through a hard-coded switch for hook dispatch, so adding a
// provider meant editing the command wiring as well as registering it here;
// now the registry is the single place that knows which providers exist.
func Engine(name string) (provider.HookEngine, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		return claudecode.Engine{}, nil
	case provider.Codex:
		return codex.Engine{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (supported: %s)", provider.ErrUnknown, name, strings.Join(provider.Supported(), ", "))
	}
}

// Lookup returns the Installer for a provider name, or ErrUnknown. Built
// adapters return a real installer; unbuilt ones a provider.Stub
// (Available()==false).
func Lookup(name string) (provider.Installer, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		inst := claudecode.Installer{} // real installer (default install paths)
		// Place this running `openbox` engine into the bundle's bin/openbox
		// so the plugin's hooks resolve to it. Best-effort resolution: if
		// os.Executable() is unavailable we leave EngineBinary empty and
		// Install skips the copy (packaging supplies the binary). Once
		// EngineBinary is set, a copy failure surfaces as an install error
		// rather than leaving a bundle with no engine — a loud failure
		// beats a silently broken install.
		if exe, err := os.Executable(); err == nil {
			inst.EngineBinary = exe
		}
		return inst, nil
	case provider.Codex:
		inst := codex.Installer{} // real installer (default install paths)
		// Same EngineBinary discipline as claude-code, with a different
		// mechanic: Codex has no plugin bundle to copy into, so the
		// absolute path of this running engine is baked directly into each
		// hooks.json command. If os.Executable() is unavailable the
		// installer falls back to `openbox` on PATH (hookCommand).
		if exe, err := os.Executable(); err == nil {
			inst.EngineBinary = exe
		}
		return inst, nil
	case provider.Cursor:
		return provider.Stub{ProviderName: provider.Cursor, Manual: cursorManual}, nil
	default:
		return nil, fmt.Errorf("%w: %q (supported: %s)", provider.ErrUnknown, name, strings.Join(provider.Supported(), ", "))
	}
}

func cursorManual(ref provider.CredentialRef) string {
	return fmt.Sprintf(`Cursor adapter is not built yet.
Manual config until the bundle ships:
  - Add hooks.json / Team hooks over beforeSubmitPrompt, beforeMCPExecution,
    afterFileEdit that invoke 'openbox' (note: Cursor hooks fail-open).
  - Credentials come from ~/.openbox/.env (written by 'openbox auth'), never
    inline. This install governs DID %s.`, ref.DID)
}
