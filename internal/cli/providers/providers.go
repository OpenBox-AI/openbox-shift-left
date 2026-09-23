// Package providers is the CLI's composition root for the install-time SPI: it
// binds each recognized provider name (from the shared `provider` module) to a
// concrete Installer.
package providers

import (
	"fmt"
	"os"
	"path/filepath"
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
		return nil, unknownProvider(name)
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

// AuditHooks reports which OpenBox engines one settings file registers. The
// path is a parameter because doctor has two levels to report: the user-wide
// file an install writes, and the current project's own file, where a
// superseded entry may survive. Codex has always replaced by argv shape, so it
// has no equivalent state to report and is deliberately absent here rather
// than silently returning empty.
func AuditHooks(settingsPath string) (LocalHookAudit, error) {
	a, err := claudecode.AuditHooks(settingsPath)
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
	default:
		return nil, unknownProvider(name)
	}
}

// RemoveProviderHooks takes every OpenBox registration out of one provider's
// hook file and reports what it removed as "event engine" lines. The
// settings-file path is a parameter rather than derived, because the same
// removal serves the user-scope file, a project's settings.local.json and the
// plugin bundle's own document.
//
// An absent file is success — uninstall walks every surface unconditionally
// rather than branching on a detected provider — but an unknown provider name
// is not: a typo must not read as "nothing was installed for it".
func RemoveProviderHooks(name, settingsPath string) ([]string, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		return claudecode.RemoveLocalHooks(settingsPath)
	case provider.Codex:
		return codex.RemoveHooks(settingsPath)
	default:
		return nil, unknownProvider(name)
	}
}

// ClaudeThinkingSummariesKey names the Claude Code settings key
// RestoreProviderSettings restores, so command output can name it without
// importing the adapter and without risking a print label that drifts from
// the key the adapter actually writes.
const ClaudeThinkingSummariesKey = claudecode.ThinkingSummariesKey

// SettingsRestoreResult is the provider-neutral shape of what
// RestoreProviderSettings did, re-declared here (like LocalHookAudit above)
// so command code can read it without importing an adapter.
type SettingsRestoreResult struct {
	// Recorded is false when the provider holds no restore record: `init`
	// never forced anything here, or an earlier `uninstall` already cleaned
	// up. The ordinary case, and not a failure.
	Recorded bool
	// Drifted is true when the value changed since `init` set it; Current is
	// its raw JSON form now ("<absent>" if the key itself is gone). Nothing
	// was touched.
	Drifted bool
	Current string
	// Present is the recorded prior value's own shape: true means the key was
	// restored to Value; false means it was absent before `init` and is now
	// deleted.
	Present bool
	Value   string
}

// RestoreProviderSettings puts back whatever a bare settings key held before
// `openbox init` forced it -- Claude Code's showThinkingSummaries today.
// Codex has no equivalent key, so it is a no-op, not an error: uninstall
// walks every surface unconditionally rather than branching on a detected
// provider. An unknown provider name still errors, mirroring
// RemoveProviderHooks's contract: a typo must not read as "nothing was
// installed for it".
func RestoreProviderSettings(name, settingsPath, homeDir string) (SettingsRestoreResult, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		r, err := claudecode.RestoreThinkingSummaries(settingsPath, homeDir)
		return SettingsRestoreResult{
			Recorded: r.Recorded,
			Drifted:  r.Drifted,
			Current:  r.Current,
			Present:  r.Present,
			Value:    r.Value,
		}, err
	case provider.Codex:
		return SettingsRestoreResult{}, nil
	default:
		return SettingsRestoreResult{}, unknownProvider(name)
	}
}

// ClaudePriorSettingsPath is where the Claude Code adapter records a
// settings key's value from before `openbox init` forced it, so uninstall
// can find and purge it (after restoring) without importing the adapter.
func ClaudePriorSettingsPath(homeDir string) string { return claudecode.PriorSettingsPath(homeDir) }

// CodexHooksPath is where the Codex adapter keeps its hook file, so an
// uninstall can look where the install wrote without importing the adapter.
func CodexHooksPath() string { return codex.DefaultHooksPath() }

// CodexSpoolDir is where the Codex adapter spools events before flush, so
// doctor can report its backlog without importing the adapter.
func CodexSpoolDir() string { return codex.DefaultSpoolDir() }

// OwnedSpoolDirs is every spool directory the adapters write to, de-duplicated
// by resolved path. It exists so a purge cannot miss one: cmd/openbox already
// hardcodes "cc-spool" three times for its own lane spools, and a fourth copy
// would let a renamed spool survive a full uninstall with undelivered governed
// tool calls still in it.
//
// De-duplication is not hygiene. OPENBOX_SPOOL_DIR overrides the whole path
// rather than the subdirectory, so with it set every adapter resolves to the
// same directory and a caller taking this list at face value would report
// deleting it once per provider.
func OwnedSpoolDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, dir := range []string{claudecode.DefaultSpoolDir(), codex.DefaultSpoolDir()} {
		if dir == "" {
			continue
		}
		resolved := filepath.Clean(dir)
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		dirs = append(dirs, resolved)
	}
	return dirs
}

// CodexConfigTOMLPath is where the Codex adapter writes its owned [otel]
// block, so `openbox init`/`doctor`/`uninstall` can look where an install
// wrote (or would write) it without importing the adapter.
func CodexConfigTOMLPath() string { return codex.ConfigTOMLPath() }

// WriteCodexOtel merges an OpenBox-owned [otel] block into Codex's
// config.toml, pointing its OTLP/HTTP exporter at endpoint. Ownership-aware:
// refuses rather than overwrites a foreign [otel] block.
func WriteCodexOtel(path, endpoint string) error { return codex.WriteOtel(path, endpoint) }

// RemoveCodexOtel removes only an OpenBox-owned [otel] block from Codex's
// config.toml, reporting whether it actually removed anything. A foreign
// block (or none at all) is left untouched.
func RemoveCodexOtel(path string) (bool, error) { return codex.RemoveOtel(path) }

// HasOwnedCodexOtel reports whether config.toml carries an OpenBox-owned
// [otel] block, for doctor/uninstall's inventory to check without writing.
func HasOwnedCodexOtel(path string) bool { return codex.HasOwnedOtel(path) }

// ClaudePluginDir is where the Claude Code adapter materializes its plugin
// bundle, so an uninstall can delete it without importing the adapter.
func ClaudePluginDir() string { return claudecode.DefaultPluginDir() }

// ClaudeUserSettingsPath is the user-wide hook file an install registers in,
// and ClaudeProjectSettingsPath is a project's own. Both are exposed so the
// command layer can name and audit them without importing the adapter.
func ClaudeUserSettingsPath() string { return claudecode.UserSettingsPath() }

// ClaudeProjectSettingsPath is a project's own hook file.
func ClaudeProjectSettingsPath(projectDir string) string {
	return claudecode.ProjectSettingsPath(projectDir)
}

// HookMarkers are the substrings that identify an OpenBox registration in one
// provider's hook file. An uninstall needs them to tell "this file exists"
// from "this file carries something of ours": the settings files belong to the
// developer and survive the removal, so their presence is not ownership, and a
// second uninstall on a clean machine has to be able to report nothing to do.
func HookMarkers(name string) []string {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		return claudecode.HookInvocationMarkers()
	case provider.Codex:
		return codex.HookInvocationMarkers()
	default:
		return nil
	}
}

// unknownProvider is the one rendering of the unsupported-provider refusal;
// every entry point below returns it, so a new provider name reaches all four.
func unknownProvider(name string) error {
	return fmt.Errorf("%w: %q (supported: %s)", provider.ErrUnknown, name, strings.Join(provider.Supported(), ", "))
}
