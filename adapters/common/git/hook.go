package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// managedMarker tags a hook script this package wrote, so re-install is
// idempotent (we overwrite our own) but a foreign, hand-written hook is never
// clobbered silently.
const managedMarker = "managed-by: openbox-shift-left (STORY-SL-5)"

// PrepareCommitMsg is the body of the `prepare-commit-msg` hook. git invokes the
// hook as: <hook> <msgFile> [<source> [<sha>]]. It stamps the in-scope
// session(s) onto msgFile.
//
// SAFETY: it returns an error ONLY for the caller to LOG — the caller (the hook
// entrypoint) must still exit 0 (see RunPrepareCommitMsg / cmd). A stamping
// failure must never abort the developer's commit.
//
// It does not special-case the commit source: `--amend` (source "commit")
// and rebase squash (source "squash") re-fire the hook, and
// addIfDifferent keeps those idempotent/additive. Merge nodes (source
// "merge") get the current session too, harmlessly — the git action
// attributes the reachable originals, not the merge node, so an extra
// merge-node line changes nothing downstream.
func (g Git) PrepareCommitMsg(msgFile string, sessions []string) error {
	return g.StampMessageFile(msgFile, sessions)
}

// RunPrepareCommitMsg is the fail-open entrypoint a hook binary calls. It
// resolves the in-scope sessions (env + file), stamps them, and reports any
// error via logf — but it is the CALLER's job to always exit 0. args are the
// raw hook arguments git passes (args[0] is the message file).
//
// Returns the number of ids that were candidates for stamping (diagnostics
// only) and any error (to be logged, never to fail the commit).
func (g Git) RunPrepareCommitMsg(args []string, r SessionResolver, logf func(string, ...any)) (int, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(args) < 1 || args[0] == "" {
		logf("prepare-commit-msg: no message file argument; skipping")
		return 0, nil
	}
	msgFile := args[0]

	// Resolve the in-scope session(s). Even when there are none (a manual/human
	// commit), we still call StampMessageFile: it harvests any OpenBox-Session
	// lines a squash left mid-body and heals them into the trailing block, so a
	// human squashing agent commits does not drop their attribution. With no
	// session in scope AND nothing to harvest, StampMessageFile is a no-op —
	// the commit stays unattributed (the git action records the reason),
	// never guessed.
	sessions := g.ResolveSessions(r)
	if err := g.StampMessageFile(msgFile, sessions); err != nil {
		return len(sessions), fmt.Errorf("stamp %s: %w", msgFile, err)
	}
	return len(sessions), nil
}

// ResolveSessions resolves the session id(s) for a commit in this repo: it
// determines the git worktree (best-effort — "" on error, fail-open) and hands
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
// Command + Args are prepended to git's hook arguments. Defaults target the
// standalone cmd/openbox-git-hook binary; the CLI wiring (OD17: single `openbox`
// engine) points these at `openbox hook git prepare-commit-msg` instead, without
// this package needing to change.
type HookConfig struct {
	Command string   // executable to run; "" => "openbox-git-hook"
	Args    []string // fixed leading args; nil => ["prepare-commit-msg"]
}

func (c HookConfig) command() string {
	if c.Command != "" {
		return c.Command
	}
	return "openbox-git-hook"
}

func (c HookConfig) args() []string {
	if c.Args != nil {
		return c.Args
	}
	return []string{"prepare-commit-msg"}
}

// InstallHook writes a `prepare-commit-msg` hook into hooksDir (typically
// `<repo>/.git/hooks`). It is idempotent when the existing hook is one we wrote
// (identified by managedMarker); it refuses to overwrite a FOREIGN hook and
// returns an error so the caller can decide (chain it, or ask the user) rather
// than destroying someone's existing hook.
func InstallHook(hooksDir string, cfg HookConfig) error {
	if hooksDir == "" {
		return fmt.Errorf("install hook: empty hooks dir")
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("install hook: mkdir %s: %w", hooksDir, err)
	}
	path := filepath.Join(hooksDir, "prepare-commit-msg")

	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), managedMarker) {
			return fmt.Errorf("install hook: %s already exists and is not managed by openbox; not overwriting", path)
		}
		// ours — fall through and overwrite (idempotent re-install)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("install hook: stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(hookScript(cfg)), 0o755); err != nil {
		return fmt.Errorf("install hook: write %s: %w", path, err)
	}
	return nil
}

// HooksDir returns the hooks directory for the repo, honoring `core.hooksPath`
// if configured (else `<git-common-dir>/hooks`). Use this for an EXPLICIT,
// user-invoked install (the user's own hooksPath is intended).
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

// HooksDirDefault is `<git-common-dir>/hooks`, IGNORING core.hooksPath. The
// opt-in AMBIENT auto-install uses this (SL5-SEC-2): an implicit, session-start
// install must not follow a repo-controlled core.hooksPath, which a malicious
// repo could point outside the tree. Explicit `install` honors it via HooksDir.
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

// hookScript renders the POSIX-sh hook. Every path exits 0 — a non-zero exit
// from prepare-commit-msg ABORTS the commit, which observe-only must never do.
// It runs the configured command only if it is resolvable, so a missing binary
// (uninstalled engine) degrades to a no-op commit rather than a failed one. The
// user can point OPENBOX_GIT_HOOK at a different binary to override.
func hookScript(cfg HookConfig) string {
	// Build a shell-safe, space-joined prefix of fixed args.
	var parts []string
	for _, a := range cfg.args() {
		parts = append(parts, shellQuote(a))
	}
	fixedArgs := strings.Join(parts, " ")

	// The default command is set with a plain assignment (NOT a "${VAR:-default}"
	// expansion): inside double quotes the single quotes of a quoted default are
	// kept literally, which would corrupt a path. A normal assignment performs
	// quote removal, so paths with spaces/quotes survive.
	return "#!/bin/sh\n" +
		"# OpenBox commit-trailer hook (STORY-SL-5). Stamps OpenBox-Session trailers\n" +
		"# so pushed commits can be bound to their session(s) server-side (SL-6).\n" +
		"# OBSERVE-ONLY: this hook MUST NEVER fail a commit — every path exits 0.\n" +
		"# " + managedMarker + "\n" +
		"if [ -z \"$OPENBOX_GIT_HOOK\" ]; then\n" +
		"  OPENBOX_GIT_HOOK=" + shellQuote(cfg.command()) + "\n" +
		"fi\n" +
		"if command -v \"$OPENBOX_GIT_HOOK\" >/dev/null 2>&1 || [ -x \"$OPENBOX_GIT_HOOK\" ]; then\n" +
		"  \"$OPENBOX_GIT_HOOK\" " + fixedArgs + " \"$@\" || true\n" +
		"fi\n" +
		"exit 0\n"
}

// shellQuote single-quotes a string for POSIX sh (handles embedded quotes).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
