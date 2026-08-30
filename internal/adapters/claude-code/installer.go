package claudecode

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

//go:embed all:plugin
var pluginFS embed.FS

// CredentialRef is the install-time seam's credential coordinate type.
type CredentialRef = providerspi.CredentialRef

// Installer writes the Claude Code plugin bundle + the non-secret dev config,
// delegated from `openbox init` (the provider seam).
type Installer struct {
	PluginDir  string // where the bundle is materialized (default: userPluginDir())
	ConfigPath string // where the dev config is written (default: DefaultConfigPath())
	// EngineBinary, when set, is the path to the unified `openbox` engine to copy
	// into the bundle's bin/openbox (the hooks invoke
	// ${CLAUDE_PLUGIN_ROOT}/bin/openbox).
	EngineBinary string
}

// Name is the provider this installer serves.
func (Installer) Name() providerspi.Name { return providerspi.ClaudeCode }

// Available reports that the Claude Code adapter is built.
func (Installer) Available() bool { return true }

// Plan describes what Install would write, without writing anything (--dry-run
// and the onboarding summary). It never prints a secret value (INV-1).
func (i Installer) Plan(ref CredentialRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenBox Claude Code plugin (observe-only, STORY-SL-4):\n")
	fmt.Fprintf(&b, "  - Materialize plugin bundle → %s\n", i.pluginDir())
	fmt.Fprintf(&b, "      .claude-plugin/plugin.json + hooks/hooks.json (SessionStart, UserPromptSubmit,\n")
	fmt.Fprintf(&b, "      PreToolUse, PostToolUse, Stop, SubagentStop, SessionEnd →\n")
	fmt.Fprintf(&b, "      `bin/openbox hook claude-code <event>`; async/best-effort)\n")
	if i.EngineBinary != "" {
		fmt.Fprintf(&b, "  - Place the openbox engine → %s\n", filepath.Join(i.pluginDir(), "bin", "openbox"))
	} else {
		fmt.Fprintf(&b, "  - (packaging places the openbox engine into bin/openbox)\n")
	}
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      base_url=%s\n", devconfig.BaseURLLabel(ref.BaseURL))
	fmt.Fprintf(&b, "      content_capture=%s (default ON as of 2026-07-15; set false to restore metadata-only)\n", contentCaptureLabel(ref.ContentCapture))
	fmt.Fprintf(&b, "  - Credentials are NOT touched here: `openbox auth` wrote them to ~/.openbox/.env and\n")
	fmt.Fprintf(&b, " the hook reads them at runtime. This command cannot read or write a secret.\n")
	fmt.Fprintf(&b, "\nCommit-trailer stamping (STORY-SL-5, session→commit binding):\n")
	fmt.Fprintf(&b, "  - The session hook maintains a per-session liveness registry (%s) so a git\n", obgit.DefaultSessionDir())
	fmt.Fprintf(&b, "    commit is attributed to the session that made it; parallel-safe across concurrent\n")
	fmt.Fprintf(&b, "    sessions (worktree-scoped, INV-2 metadata-only).\n")
	fmt.Fprintf(&b, "  - The prepare-commit-msg hook runs `bin/openbox hook git prepare-commit-msg` (the same\n")
	fmt.Fprintf(&b, "    unified engine; STORY-SL4-WIRE-2; no separate git-hook binary).\n")
	fmt.Fprintf(&b, "  - Ambient install of that hook is %s (it modifies a repo's .git/hooks). Enable at\n", onOff(ref.InstallGitHook))
	fmt.Fprintf(&b, "    onboarding with `openbox init --install-git-hook` (persisted to dev config);\n")
	fmt.Fprintf(&b, "    OPENBOX_INSTALL_GIT_HOOK overrides either way; or install per repo with\n")
	fmt.Fprintf(&b, "    `openbox hook git install`. Idempotent; never overwrites a foreign hook.\n")
	if ref.ProjectDir != "" {
		fmt.Fprintf(&b, "\nPROJECT hook scope (--scope local, the default):\n")
		fmt.Fprintf(&b, "  - Merge the hook entries into %s\n", filepath.Join(ref.ProjectDir, ".claude", "settings.local.json"))
		fmt.Fprintf(&b, " so sessions in THAT project are governed and sessions elsewhere are not.\n")
	} else {
		fmt.Fprintf(&b, "\nGLOBAL hook scope (--scope global):\n")
		fmt.Fprintf(&b, "  - Touch no project file. Activation awaits the managed-settings step below,\n")
		fmt.Fprintf(&b, "    which this command cannot perform, so nothing is governed until it lands.\n")
	}
	fmt.Fprintf(&b, "\nOrg-wide force-enable (managed settings; VERIFIED, not activated for the pilot; NFR-5):\n")
	fmt.Fprintf(&b, "  add to the managed settings.json: {\"enabledPlugins\": [\"openbox-observe\"]}\n")
	return b.String()
}

// Install materializes the plugin bundle and writes the dev config.
func (i Installer) Install(ref CredentialRef) error {
	if ref.DID == "" {
		return fmt.Errorf("claude-code install: CredentialRef.DID is required")
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
	// Production posture (empty LocalHooksDir) never touches project files.
	if ref.ProjectDir != "" {
		engine := filepath.Join(i.pluginDir(), "bin", "openbox")
		if err := writeLocalHooks(ref.ProjectDir, engine); err != nil {
			return err
		}
	}
	return nil
}

const installLockStale = time.Minute

// acquireInstallLock past a few dozen writers the queue drains slower than it
// fills, every arrival makes it worse, and the processes never exit; thousands
// of them accumulated that way and took a machine down.
func (i Installer) acquireInstallLock() (release func(), err error) {
	dir := i.pluginDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}, fmt.Errorf("claude-code install: plugin dir: %w", err)
	}
	lock := filepath.Join(dir, ".install.lock")

	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Close()
	case os.IsExist(err):
		info, statErr := os.Stat(lock)
		if statErr != nil {
			break
		}
		if time.Since(info.ModTime()) < installLockStale {
			return func() {}, fmt.Errorf(
				"claude-code install: another `openbox init` is already installing into %s "+
					"(lock held since %s). Wait for it to finish and re-run; if no other init is "+
					"running, delete %s",
				dir, info.ModTime().Format(time.RFC3339), lock)
		}
		now := time.Now()
		if chErr := os.Chtimes(lock, now, now); chErr != nil {
			return func() {}, fmt.Errorf("claude-code install: cannot reclaim stale lock %s: %w", lock, chErr)
		}
	default:
		return func() {}, nil // cannot lock here; do not block a legitimate install
	}
	return func() { _ = os.Remove(lock) }, nil
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

func (i Installer) materializeBundle() error {
	dst := i.pluginDir()
	return fs.WalkDir(pluginFS, "plugin", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("plugin", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := pluginFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
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

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF by default"
}

func contentCaptureLabel(b *bool) string {
	switch {
	case b == nil:
		return "on (default)"
	case *b:
		return "on"
	default:
		return "off"
	}
}

func userPluginDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "plugins", "openbox-observe")
}
