package git

import (
	"os"
)

// RunHook is the shared fail-open git-hook engine (OD17): the single
// implementation behind both `openbox hook git <sub>` (the unified
// engine) and the legacy standalone `openbox-git-hook` alias.
//
// Safety (the git analog of the adapters' INV-3): it never aborts a
// commit. Every failure — bad args, git error, unreadable message file,
// even a panic — is logged via logf and swallowed; nothing goes to
// stdout. The caller still owns the exit code and must exit 0.
//
// args[0] is the subcommand (prepare-commit-msg | post-commit | install); the
// rest are that subcommand's args (git passes the message file to
// prepare-commit-msg). installArgs is the fixed argument prefix baked into an
// installed hook so it re-invokes THIS engine — ["hook","git","prepare-commit-msg"]
// under `openbox`, or ["prepare-commit-msg"] under the openbox-git-hook alias.
func RunHook(args []string, installArgs []string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// A panic must never escape into the caller's exit path (never abort a commit).
	defer func() {
		if r := recover(); r != nil {
			logf("recovered: %v", r)
		}
	}()

	if len(args) == 0 {
		logf("usage: <engine> hook git <prepare-commit-msg|post-commit|install> [args...]")
		return
	}
	sub, rest := args[0], args[1:]

	g := Git{}                    // ambient git, current repo
	resolver := SessionResolver{} // env override + worktree-scoped registry

	switch sub {
	case "prepare-commit-msg":
		if _, err := g.RunPrepareCommitMsg(rest, resolver, logf); err != nil {
			logf("%v", err) // logged only — the caller still exits 0
		}
	case "post-commit":
		sessions := g.ResolveSessions(resolver)
		// Optional, non-authoritative local notes mirror.
		if err := g.WriteNoteMirror("HEAD", sessions); err != nil {
			logf("note mirror skipped: %v", err)
		}
		// Signed attestation (E8-S10): upgrades the trailer from a claim anyone
		// could write to a statement the session keyholder signed. Best-effort
		// and entirely optional — the commit already happened, and a machine
		// with no credentials simply leaves the lineage at "inferred".
		writeAttestation(g, sessions, logf)
	case "install":
		runInstall(g, installArgs, logf)
	default:
		logf("unknown git hook subcommand %q", sub)
	}
}

// runInstall writes the prepare-commit-msg hook into the current repo's
// hooks dir, pointing it back at this engine (os.Executable()) with the
// installArgs prefix. Convenience for local/dev use; the CLI and the
// ambient SessionStart install are the production paths.
func runInstall(g Git, installArgs []string, logf func(string, ...any)) {
	hooksDir, err := g.HooksDir()
	if err != nil {
		logf("install: locating hooks dir: %v", err)
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "openbox"
	}
	cfg := HookConfig{Command: self, Args: installArgs}
	// The post-commit hook carries the notes mirror and the signed attestation
	// (E8-S10). Additive and best-effort: the trailer works without it, so a
	// failure here is logged rather than failing the install.
	if err := InstallPostCommitHook(hooksDir, cfg); err != nil {
		logf("post-commit hook not installed (trailer still works): %v", err)
	}
	if err := InstallHook(hooksDir, cfg); err != nil {
		logf("install: %v", err)
		return
	}
	logf("installed prepare-commit-msg hook in %s", hooksDir)
}
