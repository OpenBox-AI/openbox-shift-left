package git

import (
	"os"
)

// RunHook is the shared fail-open git-hook engine (od17): the single
// implementation behind both `openbox hook git <sub>` (the unified engine) and
// the legacy standalone `openbox-git-hook` alias. Safety (the git analog of
// the adapters' INV-3): it never aborts a commit.
func RunHook(args []string, installArgs []string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
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
			logf("%v", err) // logged only; the caller still exits 0
		}
	case "post-commit":
		sessions := g.ResolveSessions(resolver)
		if err := g.WriteNoteMirror("HEAD", sessions); err != nil {
			logf("note mirror skipped: %v", err)
		}
		writeAttestation(g, sessions, logf)
	case "install":
		runInstall(g, installArgs, logf)
	default:
		logf("unknown git hook subcommand %q", sub)
	}
}

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
	if err := InstallPostCommitHook(hooksDir, cfg); err != nil {
		logf("post-commit hook not installed (trailer still works): %v", err)
	}
	if err := InstallHook(hooksDir, cfg); err != nil {
		logf("install: %v", err)
		return
	}
	logf("installed prepare-commit-msg hook in %s", hooksDir)
}
