package claudecode

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	fmt.Fprintf(&b, "      PreToolUse, PostToolUse, SessionEnd → `bin/openbox hook claude-code <event>`; async/best-effort)\n")
	if i.EngineBinary != "" {
		fmt.Fprintf(&b, "  - Place the openbox engine → %s\n", filepath.Join(i.pluginDir(), "bin", "openbox"))
	} else {
		fmt.Fprintf(&b, "  - (packaging places the openbox engine into bin/openbox)\n")
	}
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      base_url=%s\n", devconfig.BaseURLLabel(ref.BaseURL))
	fmt.Fprintf(&b, "      secret_service=%q api_key_account=%q private_key_account=%q\n",
		ref.SecretService, ref.APIKeyAccount, ref.PrivateKeyAccount)
	fmt.Fprintf(&b, "      content_capture=%s (default ON as of 2026-07-15; set false to restore metadata-only)\n", contentCaptureLabel(ref.ContentCapture))
	fmt.Fprintf(&b, "  - Credentials stay in the OS secret store; the hook reads them at runtime (INV-1).\n")
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
	if ref.LocalHooksDir != "" {
		fmt.Fprintf(&b, "\nLOCAL-TESTING hook scope (--local-hooks, opt-in):\n")
		fmt.Fprintf(&b, "  - Merge the five hook entries into %s\n", filepath.Join(ref.LocalHooksDir, ".claude", "settings.local.json"))
		fmt.Fprintf(&b, "    so ONLY sessions in that project are governed (production = managed/global, never project files).\n")
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
	if ref.LocalHooksDir != "" {
		engine := filepath.Join(i.pluginDir(), "bin", "openbox")
		if err := writeLocalHooks(ref.LocalHooksDir, engine); err != nil {
			return err
		}
	}
	return nil
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
func (i Installer) placeEngineBinary() error {
	if i.EngineBinary == "" {
		return nil
	}
	src, err := os.Open(i.EngineBinary)
	if err != nil {
		return fmt.Errorf("claude-code install: open engine binary: %w", err)
	}
	defer src.Close()

	binDir := filepath.Join(i.pluginDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("claude-code install: bin dir: %w", err)
	}
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
	if err := os.Rename(tmpName, filepath.Join(binDir, "openbox")); err != nil {
		return fmt.Errorf("claude-code install: commit engine: %w", err)
	}
	return nil
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
