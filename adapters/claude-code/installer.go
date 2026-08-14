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

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
	providerspi "github.com/openbox-ai/openbox-shift-left/provider"
)

// pluginFS is the Claude Code plugin bundle shipped with this adapter (the
// manifest + hook wiring). `all:` includes the dotted .claude-plugin
// directory, which go:embed skips by default. The engine binary
// (bin/openbox) is not embedded — `init` copies the running engine
// into the bundle's bin/ when Installer.EngineBinary is set;
// packaging/marketplace builds place it per-platform otherwise (see
// README "Packaging").
//
//go:embed all:plugin
var pluginFS embed.FS

// CredentialRef is the install-time seam's credential coordinate type. The
// adapter doesn't define its own; it's lifted into the shared provider
// module so `cli` and this adapter agree on one interface (INV-1: it
// carries the non-secret DID + secret-store coordinates only — never the
// obx_ key or Ed25519 seed value).
type CredentialRef = providerspi.CredentialRef

// Installer writes the Claude Code plugin bundle + the non-secret dev
// config, delegated from `openbox init` (the provider seam). It
// implements provider.Installer and replaces the "not built yet" stub for
// claude-code. Zero-value fields default to the standard install
// locations; tests set them to temp dirs.
type Installer struct {
	PluginDir  string // where the bundle is materialized (default: userPluginDir())
	ConfigPath string // where the dev config is written (default: DefaultConfigPath())
	// EngineBinary, when set, is the path to the unified `openbox` engine
	// to copy into the bundle's bin/openbox (the hooks invoke
	// ${CLAUDE_PLUGIN_ROOT}/bin/openbox). `openbox init` sets it to
	// its own executable; empty ⇒ packaging places the binary and Install
	// skips the copy.
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
	fmt.Fprintf(&b, "    the hook reads them at runtime (ADR-0015). This command cannot read or write a secret.\n")
	fmt.Fprintf(&b, "\nCommit-trailer stamping (STORY-SL-5, session→commit binding):\n")
	fmt.Fprintf(&b, "  - The session hook maintains a per-session liveness registry (%s) so a git\n", obgit.DefaultSessionDir())
	fmt.Fprintf(&b, "    commit is attributed to the session that made it — parallel-safe across concurrent\n")
	fmt.Fprintf(&b, "    sessions (worktree-scoped, INV-2 metadata-only).\n")
	fmt.Fprintf(&b, "  - The prepare-commit-msg hook runs `bin/openbox hook git prepare-commit-msg` (the same\n")
	fmt.Fprintf(&b, "    unified engine — STORY-SL4-WIRE-2; no separate git-hook binary).\n")
	fmt.Fprintf(&b, "  - Ambient install of that hook is %s (it modifies a repo's .git/hooks). Enable at\n", onOff(ref.InstallGitHook))
	fmt.Fprintf(&b, "    onboarding with `openbox init --install-git-hook` (persisted to dev config);\n")
	fmt.Fprintf(&b, "    OPENBOX_INSTALL_GIT_HOOK overrides either way; or install per repo with\n")
	fmt.Fprintf(&b, "    `openbox hook git install`. Idempotent; never overwrites a foreign hook.\n")
	if ref.ProjectDir != "" {
		fmt.Fprintf(&b, "\nPROJECT hook scope (--scope local, the default):\n")
		fmt.Fprintf(&b, "  - Merge the hook entries into %s\n", filepath.Join(ref.ProjectDir, ".claude", "settings.local.json"))
		fmt.Fprintf(&b, "    so sessions in THAT project are governed and sessions elsewhere are not (ADR-0016).\n")
	} else {
		fmt.Fprintf(&b, "\nGLOBAL hook scope (--scope global):\n")
		fmt.Fprintf(&b, "  - Touch no project file. Activation awaits the managed-settings step below,\n")
		fmt.Fprintf(&b, "    which this command cannot perform, so nothing is governed until it lands.\n")
	}
	fmt.Fprintf(&b, "\nOrg-wide force-enable (managed settings; VERIFIED, not activated for the pilot — NFR-5):\n")
	fmt.Fprintf(&b, "  add to the managed settings.json: {\"enabledPlugins\": [\"openbox-observe\"]}\n")
	return b.String()
}

// Install materializes the plugin bundle and writes the dev config. It is
// idempotent — re-running overwrites with identical content. It does NOT modify
// global/managed Claude Code settings (the force-enable step is verified-only
// for the Phase-1 opt-in pilot; Plan prints the snippet).
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
	// Opt-in LOCAL-TESTING scope: additionally activate the hooks for ONE
	// project via its .claude/settings.local.json (see localhooks.go).
	// Production posture (empty LocalHooksDir) never touches project files.
	if ref.ProjectDir != "" {
		engine := filepath.Join(i.pluginDir(), "bin", "openbox")
		if err := writeLocalHooks(ref.ProjectDir, engine); err != nil {
			return err
		}
	}
	return nil
}

// installLockStale is how long an install lock may go untouched before a later
// run treats it as abandoned. An install writes a few small files plus one
// engine copy — well under a second — so a minute is far beyond any live run
// while still self-healing after a kill, which is the failure that leaves one
// behind.
const installLockStale = time.Minute

// acquireInstallLock serializes installs against one plugin bundle, returning
// the release function.
//
// It FAILS FAST rather than queueing, and that is the whole point. Concurrent
// installs each create a temp file, write ~10MB and rename, all in one
// directory; APFS serializes those on a kernel lock for the directory. Past a
// few dozen writers the queue drains slower than it fills, every arrival makes
// it worse, and the processes never exit — thousands of them accumulated that
// way and took a machine down. A blocking lock would have produced the same
// pile-up with the waiting moved into userspace. Refusing is what bounds it: at
// most one install touches the bundle, and a second caller learns why
// immediately instead of hanging.
//
// The claim protocol mirrors the realtime flush lock (hookflow/realtime.go):
// O_EXCL create is the atomic happy path, and on EEXIST the mtime decides
// between a live install (refuse) and a stale claim from a killed one (take
// over). Best-effort by design — if the lock cannot be created at all we
// proceed rather than block a legitimate install on a filesystem quirk, since
// the guard exists to bound a pathological case, not to gate the normal one.
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
			// Vanished mid-race: the holder just finished. Treat the bundle as
			// free rather than inventing a failure.
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

// placeEngineBinary copies the unified `openbox` engine into the bundle's
// bin/openbox (0755) so the hooks — which invoke
// ${CLAUDE_PLUGIN_ROOT}/bin/openbox — resolve to it. No-op when
// EngineBinary is empty (the packaging/marketplace path places the
// per-platform binary instead). The copy is atomic (temp + rename) so a
// re-init never leaves a half-written engine.
//
// When EngineBinary is set, a copy failure is returned (fatal to Install)
// rather than swallowed: a bundle whose hooks point at a missing
// bin/openbox is a silently broken install, so we fail loudly. The caller
// decides whether to set EngineBinary at all (cli resolves it best-effort
// from os.Executable()).
//
// Re-running `init` is a supported, encouraged operation — new hook keys only
// register on a re-init — so the common case is that the engine already in
// place is the one being installed. That case now costs a read and no write:
// the copy is skipped when the bytes already match. It used to rewrite the
// whole ~10MB binary every time, which is how a repeated init turned into
// gigabytes of churn in a directory Claude Code executes out of.
func (i Installer) placeEngineBinary() error {
	if i.EngineBinary == "" {
		return nil
	}
	binDir := filepath.Join(i.pluginDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("claude-code install: bin dir: %w", err)
	}
	dst := filepath.Join(binDir, "openbox")

	// Reclaim residue from earlier runs BEFORE deciding whether to copy, so a
	// no-op re-init still cleans up (see sweepStaleEngineTemps for why a
	// killed run leaves any).
	sweepStaleEngineTemps(binDir)

	// Already the engine being installed: leave it alone. Still assert the mode,
	// because the hooks invoke this path directly and a non-executable engine
	// fails every hook rather than the install.
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

// engineTempAge is how long a .openbox-*.tmp must have gone untouched before
// the sweep treats it as residue. The copy it belongs to takes well under a
// second, so an hour cannot reach a live one while still reclaiming promptly.
const engineTempAge = time.Hour

// sweepStaleEngineTemps deletes abandoned engine temp files from binDir.
//
// placeEngineBinary removes its own temp on every ordinary path, including
// every error, via defer. What defer cannot survive is the process being
// killed, and a killed init leaves a multi-megabyte partial copy behind with
// nothing that ever reclaims it. Enough interrupted runs and the residue
// outgrows everything else in the plugin, inside the directory Claude Code
// scans for the engine.
//
// Only files older than engineTempAge are touched, because a concurrent init
// may be mid-copy into one right now and deleting it would break that run's
// rename. Best-effort throughout: this is housekeeping, and failing to tidy
// must never fail an install.
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

// sameContents reports whether a and b are byte-identical. A missing b is
// simply "not the same" — the first install has nothing to compare against.
//
// Size is checked first so the common mismatch (a rebuilt engine) costs two
// stats. Equal sizes fall through to a full comparison rather than trusting
// mtime, which a copy rewrites and so can never indicate sameness here.
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

// writeConfig persists the shared non-secret dev config. The merge policy —
// what a re-init carries forward versus overwrites — lives in devconfig so it
// is identical for every provider (ADR-0006 posture included).
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
	// The WRITE target, deliberately — not DefaultConfigPath(), which is
	// read-resolved and prefers an existing LEGACY file over a not-yet-created new
	// one (devconfig.resolveConfigPath). Writing through the read path would let an
	// install land in the pre-ADR-0015 directory whenever migration had not yet
	// created the new file — and migration is explicitly non-fatal, so that is
	// reachable, not theoretical. It happens to work today only because
	// migrateLegacyConfig usually runs first; relying on that ordering is what this
	// avoids.
	if p, err := devconfig.DevConfigWritePath(); err == nil {
		return p
	}
	return DefaultConfigPath()
}

// userPluginDir is the default local install location for the plugin bundle
// (~/.claude/plugins/openbox-observe). Marketplace/managed installs use their
// own path; this is the CLI-driven local install target.
func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF by default"
}

// contentCaptureLabel renders the *bool org content posture for the install
// preview: nil ⇒ the adapter default (ON as of 2026-07-15), else the pinned value.
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
