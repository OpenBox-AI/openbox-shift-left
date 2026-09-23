package claudecode

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// CredentialRef is the install-time seam's credential coordinate type.
type CredentialRef = providerspi.CredentialRef

// Installer writes the Claude Code plugin bundle + the non-secret dev config,
// delegated from `openbox init` (the provider seam).
type Installer struct {
	PluginDir  string // where the bundle is materialized (default: userPluginDir())
	ConfigPath string // where the dev config is written (default: DefaultConfigPath())
	// SettingsPath is the hook file to register in (default: UserSettingsPath()).
	// An override, like the two above, so a test can assert the write without
	// depending on the machine's real home.
	SettingsPath string
	// EngineBinary, when set, is the path to the unified `openbox` engine to copy
	// into the bundle's bin/openbox (the hooks invoke
	// ${CLAUDE_PLUGIN_ROOT}/bin/openbox).
	EngineBinary string
	// HomeDir overrides the home directory the showThinkingSummaries
	// prior-value restore record resolves under (default: homeDir(), the
	// real OS home). A test seam, like the three paths above.
	HomeDir string
}

// Name is the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.ClaudeCode }

// Available reports that the Claude Code adapter is built.
func (Installer) Available() bool { return true }

// Install materializes the plugin bundle and writes the dev config.
func (i Installer) Install(ref CredentialRef) error {
	if ref.AgentID == "" {
		return fmt.Errorf("claude-code install: CredentialRef.AgentID is required")
	}
	release, err := i.acquireInstallLock()
	if err != nil {
		return err
	}
	defer release()

	if err := i.materializeBundle(); err != nil {
		return err
	}
	if err := i.placeEngineBinary(); err != nil {
		return err
	}
	if err := i.writeConfig(ref); err != nil {
		return err
	}
	// User-wide, always. An install governs every session on this machine
	// rather than one directory, so there is no scope to resolve and no project
	// file to write. Both of the following stay inside the install lock.
	engine := filepath.Join(i.pluginDir(), "bin", "openbox")
	if err := writeHooks(i.settingsPath(), engine); err != nil {
		return err
	}
	// Claude Code only sends a non-empty reasoning summary when this is true;
	// otherwise a governed transcript's thinking blocks arrive empty. Forced
	// unconditionally (no opt-out), with the prior value recorded so
	// `openbox uninstall` can put it back. A failure here is an install
	// failure, like writeHooks above, and stays inside the same lock.
	if err := writeThinkingSummaries(i.settingsPath(), i.homeDir()); err != nil {
		return err
	}
	// And an entry left in the current project's own file is now a second
	// registration of the same gate. Sweeping it is part of installing, not
	// cleanup: where it names a different engine path the tool will not
	// de-duplicate the two and every governed call is stored twice.
	//
	// A sweep failure must not fail the install: the user-wide hooks are
	// already in place and governing, so refusing here would leave a machine
	// governed by a command that reported failure. It is reported instead.
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openbox: could not resolve the working directory, so this project's "+
			"own settings file was not swept: %v\n", err)
		return nil
	}
	removed, err := SweepProjectHooks(wd)
	if err != nil {
		if strings.Contains(err.Error(), "not valid JSON") {
			// It was left untouched, and while it stays malformed the tool applies
			// NO hooks from it -- so the loss is the developer's own project
			// hooks, not a doubled OpenBox registration.
			fmt.Fprintf(os.Stderr, "openbox: %s could not be parsed, so it was left alone: %v\n"+
				"  While it stays malformed the tool applies no hooks from that file at all, including "+
				"any of your own. The user-wide install above is unaffected.\n",
				ProjectSettingsPath(wd), err)
			return nil
		}
		fmt.Fprintf(os.Stderr, "openbox: %s could not be cleaned: %v\n"+
			"  If it holds an OpenBox registration at a different engine path, every governed tool "+
			"call in this project is recorded twice until it is removed by hand.\n",
			ProjectSettingsPath(wd), err)
		return nil
	}
	i.reportSweep(wd, removed)
	return nil
}

// settingsPath is where this install registers its hooks.
func (i Installer) settingsPath() string {
	if i.SettingsPath != "" {
		return i.SettingsPath
	}
	return UserSettingsPath()
}

// homeDir is where this install resolves the showThinkingSummaries
// prior-value restore record under. When HomeDir is unset it falls back to
// SettingsPath's own grandparent -- UserSettingsPath is
// homeDir()/.claude/settings.json by construction (userhooks.go), so a test
// that pins SettingsPath without also pinning HomeDir still gets an isolated
// directory instead of silently resolving against the real OS home. Only
// when neither is set (the real installer, via internal/cli/providers) does
// this fall through to the real home.
func (i Installer) homeDir() string {
	if i.HomeDir != "" {
		return i.HomeDir
	}
	if i.SettingsPath != "" {
		return filepath.Dir(filepath.Dir(i.SettingsPath))
	}
	return homeDir()
}

func (i Installer) reportSweep(projectDir string, removed []string) {
	if len(removed) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "openbox: removed %d superseded OpenBox hook registration(s) from %s\n"+
		"  events: %s\n"+
		"  This install governs every session on this machine, so a project-level copy would fire a "+
		"second time and store every governed tool call twice.\n",
		len(removed), ProjectSettingsPath(projectDir), strings.Join(removed, ", "))
}

// acquireInstallLock past a few dozen writers the queue drains slower than it
// fills, every arrival makes it worse, and the processes never exit; thousands
// accumulated that way and took a machine down. So this refuses rather than
// queues, and TryLock keeps that true where a blocking acquire would rebuild
// the queue with kernel-grade reliability.
//
// Liveness is the kernel's now, not the filesystem clock's: an advisory lock is
// released when its holder dies, which deletes the staleness window and every
// bug in it -- the full-minute hold after a crash, the NTP step, 1s mtime
// granularity against a 60s comparison, the Stat/Chtimes reclaim racing.
func (i Installer) acquireInstallLock() (release func(), err error) {
	dir := i.pluginDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, fmt.Errorf("claude-code install: plugin dir: %w", err)
	}
	lock := filepath.Join(dir, ".install.lock")

	fl := flock.New(lock)
	held, lockErr := fl.TryLock()
	switch {
	case lockErr != nil:
		return func() {}, nil // cannot lock here; do not block a legitimate install
	case !held:
		return func() {}, fmt.Errorf(
			"claude-code install: another `openbox init` is already installing into %s "+
				"(%s is held). Wait for it to finish and re-run. Nothing needs deleting by hand: "+
				"the lock is released when that process exits, however it exits",
			dir, lock)
	}
	// The lock file stays, per gofrs' own contract. Removing it reopens a
	// by-name hole: unlinking the name while a second init has the inode open
	// lets that flock succeed on an unreachable inode while a third creates a
	// fresh file and locks that -- two holders. Unlocking first only moves it.
	return func() { _ = fl.Unlock() }, nil
}

// placeEngineBinary the copy is atomic (temp + rename) so a re-init never
// leaves a half-written engine.
func (i Installer) placeEngineBinary() error {
	if i.EngineBinary == "" {
		return nil
	}
	binDir := filepath.Join(i.pluginDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("claude-code install: bin dir: %w", err)
	}
	dst := filepath.Join(binDir, "openbox")

	sweepStaleEngineTemps(binDir)

	same, err := sameContents(i.EngineBinary, dst)
	if err != nil {
		return fmt.Errorf("claude-code install: compare engine: %w", err)
	}
	if same {
		if err := os.Chmod(dst, 0o755); err != nil {
			return fmt.Errorf("claude-code install: engine mode: %w", err)
		}
		return nil
	}

	src, err := os.Open(i.EngineBinary)
	if err != nil {
		return fmt.Errorf("claude-code install: open engine binary: %w", err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(binDir, ".openbox-*.tmp")
	if err != nil {
		return fmt.Errorf("claude-code install: temp engine: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("claude-code install: copy engine: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("claude-code install: commit engine: %w", err)
	}
	return nil
}

// engineTempAge the copy it belongs to takes well under a second, so an hour
// cannot reach a live one while still reclaiming promptly.
const engineTempAge = time.Hour

// sweepStaleEngineTemps what defer cannot survive is the process being killed,
// and a killed init leaves a multi-megabyte partial copy behind with nothing
// that ever reclaims it.
func sweepStaleEngineTemps(binDir string) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, ".openbox-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < engineTempAge {
			continue
		}
		_ = os.Remove(filepath.Join(binDir, name))
	}
}

// sameContents equal sizes fall through to a full comparison rather than
// trusting mtime, which a copy rewrites and so can never indicate sameness
// here.
func sameContents(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ai.Mode().IsRegular() || !bi.Mode().IsRegular() || ai.Size() != bi.Size() {
		return false, nil
	}
	sumA, err := fileSum(a)
	if err != nil {
		return false, err
	}
	sumB, err := fileSum(b)
	if err != nil {
		return false, err
	}
	return sumA == sumB, nil
}

func fileSum(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// materializeBundle creates the directory that holds the engine copy, and
// nothing else.
//
// It used to unpack an embedded hooks.json and plugin manifest as well. Those
// made this directory a loadable Claude Code plugin carrying its own copy of
// all eleven handlers — and a plugin's handlers are separate from settings and
// do not de-duplicate against them, so anything that loaded it would have
// doubled every event against the registrations the install writes. The
// directory's one remaining job is hosting bin/openbox, which those
// registrations point at.
func (i Installer) materializeBundle() error {
	dir := filepath.Join(i.pluginDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("claude-code install: create %s: %w", dir, err)
	}
	return nil
}

func (i Installer) writeConfig(ref CredentialRef) error {
	if err := devconfig.WriteConfig(i.configPath(), providerspi.ConfigUpdate(ref)); err != nil {
		return fmt.Errorf("claude-code install: %w", err)
	}
	return nil
}

func (i Installer) pluginDir() string {
	if i.PluginDir != "" {
		return i.PluginDir
	}
	return userPluginDir()
}

func (i Installer) configPath() string {
	if i.ConfigPath != "" {
		return i.ConfigPath
	}
	if p, err := devconfig.DevConfigWritePath(); err == nil {
		return p
	}
	return DefaultConfigPath()
}

func userPluginDir() string {
	return filepath.Join(homeDir(), ".claude", "plugins", "openbox-observe")
}
