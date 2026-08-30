package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const managedMarker = "managed-by: openbox-shift-left (STORY-SL-5)"

// RunPrepareCommitMsg is the fail-open entrypoint a hook binary calls. Returns
// the number of ids that were candidates for stamping (diagnostics only) and
// any error (to be logged, never to fail the commit).
func (g Git) RunPrepareCommitMsg(args []string, r SessionResolver, logf func(string, ...any)) (int, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(args) < 1 || args[0] == "" {
		logf("prepare-commit-msg: no message file argument; skipping")
		return 0, nil
	}
	msgFile := args[0]

	sessions := g.ResolveSessions(r)
	if err := g.StampMessageFile(msgFile, sessions); err != nil {
		return len(sessions), fmt.Errorf("stamp %s: %w", msgFile, err)
	}
	return len(sessions), nil
}

// ResolveSessions resolves the session id(s) for a commit in this repo: it
// determines the git worktree (best-effort; "" on error, fail-open) and hands
// it to the resolver's two-tier lookup (env override, then the worktree-scoped
// registry).
func (g Git) ResolveSessions(r SessionResolver) []string {
	worktree, _ := g.Worktree() // "" on error → resolver uses the override tier only
	return r.Resolve(worktree)
}

// Worktree returns the absolute top-level of the working tree for g.Dir.
func (g Git) Worktree() (string, error) {
	out, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HookConfig describes the command the installed hook script shells out to.
type HookConfig struct {
	Command string   // executable to run; "" => "openbox"
	Args    []string // fixed leading args; nil => ["hook", "git", "prepare-commit-msg"]
}

func (c HookConfig) command() string {
	if c.Command != "" {
		return c.Command
	}
	return "openbox"
}

func (c HookConfig) args() []string {
	if c.Args != nil {
		return c.Args
	}
	return []string{"hook", "git", "prepare-commit-msg"}
}

// InstallHook writes a `prepare-commit-msg` hook into hooksDir (typically
// `<repo>/.git/hooks`).
func InstallHook(hooksDir string, cfg HookConfig) error {
	if hooksDir == "" {
		return fmt.Errorf("install hook: empty hooks dir")
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("install hook: mkdir %s: %w", hooksDir, err)
	}
	return writeHookScript(hooksDir, "prepare-commit-msg", cfg)
}

// InstallPostCommitHook installs the `post-commit` hook, which runs after a
// commit exists and writes the two artifacts that need its sha: the non-
// authoritative notes mirror and the signed attestation (E8-S10). Installing
// it is additive; the same never-overwrite-a-foreign-hook rule applies.
func InstallPostCommitHook(hooksDir string, cfg HookConfig) error {
	if hooksDir == "" {
		return fmt.Errorf("install hook: empty hooks dir")
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("install hook: mkdir %s: %w", hooksDir, err)
	}
	post := cfg
	post.Args = []string{"hook", "git", "post-commit"}
	return writeHookScript(hooksDir, "post-commit", post)
}

// writeHookScript a developer's existing hook is theirs; silently replacing it
// would be a worse failure than not installing.
func writeHookScript(hooksDir, name string, cfg HookConfig) error {
	path := filepath.Join(hooksDir, name)

	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), managedMarker) {
			return fmt.Errorf("install hook: %s already exists and is not managed by openbox; not overwriting", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("install hook: stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(hookScript(cfg)), 0o755); err != nil {
		return fmt.Errorf("install hook: write %s: %w", path, err)
	}
	return nil
}

// HooksDir returns the hooks directory for the repo, honoring `core.hooksPath`
// if configured (else `<git-common-dir>/hooks`).
func (g Git) HooksDir() (string, error) {
	if out, err := g.run("config", "--get", "core.hooksPath"); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if filepath.IsAbs(p) {
				return p, nil
			}
			top, terr := g.run("rev-parse", "--show-toplevel")
			if terr != nil {
				return "", terr
			}
			return filepath.Join(strings.TrimSpace(string(top)), p), nil
		}
	}
	return g.HooksDirDefault()
}

// HooksDirDefault is `<git-common-dir>/hooks`, ignoring core.hooksPath. The
// opt-in ambient auto-install uses this (SL5-SEC-2): an implicit, session-
// start install must not follow a repo-controlled core.hooksPath, which a
// malicious repo could point outside the tree.
func (g Git) HooksDirDefault() (string, error) {
	out, err := g.run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		base := g.Dir
		if base == "" {
			base = "."
		}
		dir = filepath.Join(base, dir)
	}
	return filepath.Join(dir, "hooks"), nil
}

// hookScript every path exits 0; a non-zero exit from prepare-commit-msg
// aborts the commit, which observe-only must never do.
func hookScript(cfg HookConfig) string {
	var parts []string
	for _, a := range cfg.args() {
		parts = append(parts, shellQuote(a))
	}
	fixedArgs := strings.Join(parts, " ")

	return "#!/bin/sh\n" +
		"# OpenBox commit-trailer hook (STORY-SL-5). Stamps OpenBox-Session trailers\n" +
		"# so pushed commits can be bound to their session(s) server-side (SL-6).\n" +
		"# OBSERVE-ONLY: this hook MUST NEVER fail a commit; every path exits 0.\n" +
		"# " + managedMarker + "\n" +
		"if [ -z \"$OPENBOX_GIT_HOOK\" ]; then\n" +
		"  OPENBOX_GIT_HOOK=" + shellQuote(cfg.command()) + "\n" +
		"fi\n" +
		"if command -v \"$OPENBOX_GIT_HOOK\" >/dev/null 2>&1 || [ -x \"$OPENBOX_GIT_HOOK\" ]; then\n" +
		"  \"$OPENBOX_GIT_HOOK\" " + fixedArgs + " \"$@\" || true\n" +
		"fi\n" +
		"exit 0\n"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
