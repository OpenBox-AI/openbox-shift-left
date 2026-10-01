// Package providers is the CLI's composition root for the install-time SPI: it
// binds each recognized provider name (from the shared `provider` module) to a
// concrete Installer.
package providers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	claudecode "github.com/openbox-ai/openbox-shift-left/internal/adapters/claude-code"
	codex "github.com/openbox-ai/openbox-shift-left/internal/adapters/codex"
	muse "github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Engine returns the runtime hook engine for a provider name, or ErrUnknown.
func Engine(name string) (provider.HookEngine, error) {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		return claudecode.Engine{}, nil
	case provider.Codex:
		return codex.Engine{}, nil
	case provider.Muse:
		return muse.Engine{}, nil
	default:
		return nil, unknownProvider(name)
	}
}

// LocalHookAudit is the provider-neutral shape of a project's hook
// registration, re-declared here so command code can read it without importing
// an adapter (TestOnlyTheRegistryImportsAdapters).
type LocalHookAudit struct {
	SettingsPath       string
	Present            bool
	Engines            []string
	DuplicateEvents    []string
	ShortTimeoutEvents []string
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
		SettingsPath:       a.SettingsPath,
		Present:            a.Present,
		Engines:            a.Engines,
		DuplicateEvents:    a.DuplicateEvents,
		ShortTimeoutEvents: a.ShortTimeoutEvents,
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
	case provider.Muse:
		inst := muse.Installer{Runner: MuseRunner}
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
	case provider.Muse:
		return muse.RemoveHooks(settingsPath)
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
	// RemovedFile is true when the settings file was one `init` created and
	// held nothing else, so the restore deleted it. It can be set with
	// Recorded false.
	RemovedFile bool
}

// RestoreProviderSettings puts back whatever a bare settings key held before
// `openbox init` forced it: Claude Code's showThinkingSummaries, and Muse's
// `telemetry` object (deleted again when there was none). Codex has no
// equivalent key, so it is a no-op, not an error: uninstall
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
	case provider.Muse:
		r, err := muse.RestoreTelemetry(settingsPath, homeDir)
		return SettingsRestoreResult{
			Recorded: r.Recorded,
			Drifted:  r.Drifted,
			Current:  r.Current,
			Present:  r.Present,
			Value:    r.Value,

			RemovedFile: r.RemovedFile,
		}, err
	case provider.Codex:
		// Codex forces no bare settings key at install time.
		return SettingsRestoreResult{}, nil
	default:
		return SettingsRestoreResult{}, unknownProvider(name)
	}
}

// MuseTelemetryKey names the Muse settings key RestoreProviderSettings
// restores, so command output can name it without importing the adapter.
const MuseTelemetryKey = muse.TelemetryKey

// MusePriorSettingsPath is where the Muse adapter records `telemetry`'s value
// from before `openbox init` pointed it at the receiver, so uninstall can find
// and purge it after restoring.
func MusePriorSettingsPath(homeDir string) string { return muse.PriorSettingsPath(homeDir) }

// WriteMuseTelemetry points Muse's own telemetry export at endpoint (the
// receiver's base URL), recording the prior value first. replaced is the
// developer's previous value when one was displaced. Refuses rather than
// overwrites a value the developer changed after OpenBox set it.
func WriteMuseTelemetry(settingsPath, homeDir, endpoint string) (replaced string, err error) {
	return muse.WriteTelemetry(settingsPath, homeDir, endpoint)
}

// CheckMuseTelemetry is WriteMuseTelemetry's refusal logic with nothing written,
// for an installer to run before it touches anything shared.
func CheckMuseTelemetry(settingsPath, homeDir, endpoint string) error {
	return muse.CheckTelemetry(settingsPath, homeDir, endpoint)
}

// HasOwnedMuseTelemetry reports whether Muse's settings still carry the
// telemetry value OpenBox set.
func HasOwnedMuseTelemetry(settingsPath, homeDir string) bool {
	return muse.HasOwnedTelemetry(settingsPath, homeDir)
}

// ClaudePriorSettingsPath is where the Claude Code adapter records a
// settings key's value from before `openbox init` forced it, so uninstall
// can find and purge it (after restoring) without importing the adapter.
func ClaudePriorSettingsPath(homeDir string) string { return claudecode.PriorSettingsPath(homeDir) }

// CodexHooksPath is where the Codex adapter keeps its hook file, so an
// uninstall can look where the install wrote without importing the adapter.
func CodexHooksPath() string { return codex.DefaultHooksPath() }

// MuseRunner runs every `muse` subprocess the install and doctor start: the
// version gate, the hook load probe, the config checks. A test replaces it, so
// none of them reaches a real binary.
var MuseRunner muse.Runner = muse.ExecRunner

// MuseRunResult, MuseSettingsAudit and MuseVersionCheck are the Muse adapter's
// result shapes, re-declared so command code can read them without importing
// the adapter.
type (
	MuseRunResult     = muse.RunResult
	MuseSettingsAudit = muse.SettingsAudit
	MuseVersionCheck  = muse.VersionCheck
)

// The states a MuseVersionCheck reports.
const (
	MuseVersionSupported  = muse.VersionSupported
	MuseVersionNotOnPath  = muse.VersionNotOnPath
	MuseVersionUnreadable = muse.VersionUnreadable
	MuseVersionTooOld     = muse.VersionTooOld
	MuseVersionUntested   = muse.VersionUntested
)

// ErrMuseNotOnPath is what MuseRunner returns when there is no muse binary.
var ErrMuseNotOnPath = muse.ErrNotOnPath

// MuseVersionRange names the tested Muse versions for a doctor row.
func MuseVersionRange() string {
	return ">= " + muse.MinVersion.String() + ", tested below " + muse.TestedBelow.String()
}

// CheckMuseVersion asks the installed muse for its version through MuseRunner.
func CheckMuseVersion() MuseVersionCheck { return muse.CheckVersion(MuseRunner) }

// AuditMuseSettings reads Muse's settings file under Muse's rules and audits its
// OpenBox handlers against the --home this machine's hooks need.
func AuditMuseSettings(path string) (MuseSettingsAudit, error) {
	return muse.AuditSettings(path, muse.BakedHome())
}

// MuseEvidenceSummary is what the session-log reconciler left in the local trace.
type MuseEvidenceSummary = muse.EvidenceSummary

// SummarizeMuseEvidence counts the reconciler's findings in the trace at dir
// over the last week, without importing the adapter.
func SummarizeMuseEvidence(traceDir string, now time.Time) (MuseEvidenceSummary, error) {
	return muse.SummarizeEvidence(traceDir, now)
}

// MuseSettingsPath is Muse's user-wide settings file, where an install
// registers its hooks, so uninstall and doctor can look there without
// importing the adapter.
func MuseSettingsPath() string { return muse.SettingsPath() }

// SpoolDirFor is the spool directory the named provider's adapter writes to,
// reusing that adapter's own DefaultSpoolDir (which honours OPENBOX_SPOOL_DIR)
// rather than re-deriving the path here. It exists so init/adopt can discard
// a tool's spool when a legacy identity is replaced: the caller already
// validated the provider name via Lookup, so an unrecognized name answers ""
// rather than an error.
func SpoolDirFor(name string) string {
	switch provider.Name(name) {
	case provider.ClaudeCode:
		return claudecode.DefaultSpoolDir()
	case provider.Codex:
		return codex.DefaultSpoolDir()
	case provider.Muse:
		return muse.DefaultSpoolDir()
	default:
		return ""
	}
}

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
	for _, dir := range []string{claudecode.DefaultSpoolDir(), codex.DefaultSpoolDir(), muse.DefaultSpoolDir()} {
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
	case provider.Muse:
		return muse.HookInvocationMarkers()
	default:
		return nil
	}
}

// unknownProvider is the one rendering of the unsupported-provider refusal;
// every entry point below returns it, so a new provider name reaches all four.
func unknownProvider(name string) error {
	return fmt.Errorf("%w: %q (supported: %s)", provider.ErrUnknown, name, strings.Join(provider.Supported(), ", "))
}
