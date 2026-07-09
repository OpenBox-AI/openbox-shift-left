package claudecode

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

// pluginFS is the Claude Code plugin bundle shipped with this adapter (the
// manifest + hook wiring). `all:` includes the dotted .claude-plugin directory,
// which go:embed skips by default. The engine binary (bin/openbox-cc-hook) is
// NOT embedded — it is built per-platform and placed into the bundle's bin/ at
// package time (see README "Packaging").
//
//go:embed all:plugin
var pluginFS embed.FS

// CredentialRef points the installer at where `openbox dev init` (STORY-SL-2)
// stored the agent credentials. INV-1: it carries the non-secret DID + secret
// store COORDINATES only — never the obx_ key or Ed25519 seed value.
type CredentialRef struct {
	SecretService     string
	APIKeyAccount     string
	PrivateKeyAccount string
	DID               string
	BaseURL           string // optional; defaults to the core base
	ContentCapture    bool   // org content posture (default false = metadata-only)
	InstallGitHook    bool   // STORY-SL-5: ambient prepare-commit-msg install (default false)
}

// Installer writes the Claude Code plugin bundle + the non-secret dev config,
// delegated from `openbox dev init` (the SL-2 provider seam). It replaces the
// SL-2 "not built yet" stub for claude-code. Zero-value fields default to the
// standard install locations; tests set them to temp dirs.
type Installer struct {
	PluginDir  string // where the bundle is materialized (default: userPluginDir())
	ConfigPath string // where the dev config is written (default: DefaultConfigPath())
}

// Name is the provider this installer serves.
func (Installer) Name() string { return provider }

// Available reports that the Claude Code adapter is built (unlike the SL-2 stub).
func (Installer) Available() bool { return true }

// Plan describes what Install would write, without writing anything (--dry-run
// and the onboarding summary). It never prints a secret value (INV-1).
func (i Installer) Plan(ref CredentialRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OpenBox Claude Code plugin (observe-only, STORY-SL-4):\n")
	fmt.Fprintf(&b, "  - Materialize plugin bundle → %s\n", i.pluginDir())
	fmt.Fprintf(&b, "      .claude-plugin/plugin.json + hooks/hooks.json (SessionStart, UserPromptSubmit,\n")
	fmt.Fprintf(&b, "      PreToolUse, PostToolUse, SessionEnd → bin/openbox-cc-hook; async/best-effort)\n")
	fmt.Fprintf(&b, "  - Write dev config (non-secret coordinates) → %s\n", i.configPath())
	fmt.Fprintf(&b, "      developer_did=%s\n", ref.DID)
	fmt.Fprintf(&b, "      secret_service=%q api_key_account=%q private_key_account=%q\n",
		ref.SecretService, ref.APIKeyAccount, ref.PrivateKeyAccount)
	fmt.Fprintf(&b, "      content_capture=%t (default false = metadata-only, INV-2)\n", ref.ContentCapture)
	fmt.Fprintf(&b, "  - Credentials stay in the OS secret store; the hook reads them at runtime (INV-1).\n")
	fmt.Fprintf(&b, "\nCommit-trailer stamping (STORY-SL-5, session→commit binding):\n")
	fmt.Fprintf(&b, "  - The plugin bundles bin/openbox-git-hook and maintains a per-session liveness\n")
	fmt.Fprintf(&b, "    registry (%s) so a git commit is attributed to the session that made it —\n", obgit.DefaultSessionDir())
	fmt.Fprintf(&b, "    parallel-safe across concurrent sessions (worktree-scoped, INV-2 metadata-only).\n")
	fmt.Fprintf(&b, "  - Ambient install of the prepare-commit-msg hook is %s (it modifies a repo's\n", onOff(ref.InstallGitHook))
	fmt.Fprintf(&b, "    .git/hooks). Enable at onboarding with `openbox dev init --install-git-hook`\n")
	fmt.Fprintf(&b, "    (persisted to dev config); OPENBOX_INSTALL_GIT_HOOK overrides either way; or install\n")
	fmt.Fprintf(&b, "    per repo with `openbox-git-hook install`. Idempotent; never overwrites a foreign hook.\n")
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
	return i.writeConfig(ref)
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
	cfg := DevConfig{
		BaseURL:           ref.BaseURL,
		DID:               ref.DID,
		SecretService:     ref.SecretService,
		APIKeyAccount:     ref.APIKeyAccount,
		PrivateKeyAccount: ref.PrivateKeyAccount,
		ContentCapture:    ref.ContentCapture,
		InstallGitHook:    ref.InstallGitHook,
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := i.configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("claude-code install: config dir: %w", err)
	}
	// 0600: coordinates are not secret, but keep them owner-only anyway.
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("claude-code install: write config: %w", err)
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

func userPluginDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "plugins", "openbox-observe")
}
