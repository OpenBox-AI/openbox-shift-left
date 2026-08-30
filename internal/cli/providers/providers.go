// Package providers is the CLI's composition root for the install-time SPI: it
// binds each recognized provider name (from the shared `provider` module) to a
// concrete Installer. This is the one place that imports both the shared SPI
// and the adapter modules; keeping it in `cli` (not in the shared module) is
// what breaks the would-be import cycle between the SPI and its adapters.
package providers

import (
	"fmt"
	"os"
	"strings"

	claudecode "github.com/openbox-ai/openbox-shift-left/internal/adapters/claude-code"
	codex "github.com/openbox-ai/openbox-shift-left/internal/adapters/codex"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Engine returns the runtime hook engine for a provider name, or ErrUnknown.
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

// LocalHookAudit is the provider-neutral shape of a project's hook
// registration, re-declared here so command code can read it without importing
// an adapter (TestOnlyTheRegistryImportsAdapters).
type LocalHookAudit struct {
	SettingsPath    string
	Present         bool
	Engines         []string
	DuplicateEvents []string
}

// AuditProjectHooks reports which OpenBox engines a project's provider-local
// hook config registers. Codex has always replaced by argv shape, so it has no
// equivalent state to report and is deliberately absent here rather than
// silently returning empty.
func AuditProjectHooks(projectDir string) (LocalHookAudit, error) {
	a, err := claudecode.AuditLocalHooks(projectDir)
	return LocalHookAudit{
		SettingsPath:    a.SettingsPath,
		Present:         a.Present,
		Engines:         a.Engines,
		DuplicateEvents: a.DuplicateEvents,
	}, err
}

// Lookup returns the Installer for a provider name, or ErrUnknown.
func Lookup(name string) (provider.Installer, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		inst := claudecode.Installer{} // real installer (default install paths)
		if exe, err := os.Executable(); err == nil {
			inst.EngineBinary = exe
		}
		return inst, nil
	case provider.Codex:
		inst := codex.Installer{} // real installer (default install paths)
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
