package main

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
)

// scope.go — which sessions an install actually governs.
//
// Scope selects ACTIVATION, not location. `Install` unconditionally materializes
// the plugin bundle, places the engine binary and writes posture; scope decides
// only whether the project settings merge happens on top. So "local vs global"
// is not "install here vs there".

const (
	// scopeLocal governs the current directory only, by merging the hook block
	// into <dir>/.claude/settings.local.json. It is the DEFAULT because it is the
	// only scope the CLI can finish by itself.
	scopeLocal = "local"
	// scopeGlobal installs the bundle and prints the managed-settings snippet it
	// cannot apply. Activation is an administrator's action.
	scopeGlobal = "global"
)

// resolveScope turns the --scope flag into a scope, per provider.
//
// Codex has no project scope and pretending otherwise would be the bad kind of
// default: its hooks live at $CODEX_HOME/hooks.json or ~/.codex/hooks.json, and
// the repo-level .codex/hooks.json is a location its installer deliberately does
// not touch. So `--scope local --provider codex` ERRORS, and an unspecified scope
// resolves to global while SAYING SO. Silently governing every Codex session on
// the machine when the user asked for one project is worse than an error — it
// over-delivers governance without consent.
func (a *app) resolveScope(scope, providerName string) (string, int) {
	switch scope {
	case "", scopeLocal, scopeGlobal:
	default:
		return "", a.errorf("--scope %q is not valid (want %q or %q)", scope, scopeLocal, scopeGlobal)
	}

	if providerName == "codex" {
		if scope == scopeLocal {
			return "", a.errorf("--scope local is not available for codex.\n" +
				"  Codex reads its hooks from $CODEX_HOME/hooks.json (or ~/.codex/hooks.json), which is\n" +
				"  user-wide. A repo-level .codex/hooks.json is an alternative location this installer\n" +
				"  deliberately does not touch, so a project-scoped Codex install cannot be honoured.\n" +
				"  Run without --scope to install user-wide, or use claude-code for project scope.")
		}
		if scope == "" {
			// Never infer silently: say which scope was resolved and what it means.
			fmt.Fprintf(a.stdout, "note: codex hooks are user-wide, so this install uses GLOBAL scope —\n"+
				"      every Codex session on this machine is governed, not just this directory.\n")
		}
		return scopeGlobal, exitOK
	}

	if scope == "" {
		return scopeLocal, exitOK
	}
	return scope, exitOK
}

// flagPassed reports whether a flag was explicitly given on the command line.
//
// It exists for one case that a value cannot answer: `--enforce` defaults to
// true, so its VALUE alone cannot distinguish "the user asked for enforce" from
// "the user said nothing". That distinction is what makes `--enforce --no-enforce`
// an error rather than a silent win for one of them.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// requireCredentials refuses to install when this machine has no credentials.
//
// `init` performs no registration and writes no credential, so absent credentials
// are a precondition failure rather than something to fix inline. It must not
// half-install: a bundle installed against no identity produces hooks that fire,
// fail to resolve credentials, and fail open silently — an install that looks
// finished and governs nothing.
func (a *app) requireCredentials() int {
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	kv, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return a.errorf("%v", err)
	}
	haveKey := a.getenv(devconfig.EnvAPIKeyDirect) != "" || kv[devconfig.EnvAPIKeyDirect] != ""
	havePrivateKey := a.getenv(devconfig.EnvAgentPrivateKey) != "" || kv[devconfig.EnvAgentPrivateKey] != ""
	for _, alias := range []string{"OPENBOX_ED25519_SEED", "OPENBOX_SEED"} {
		if a.getenv(alias) != "" || kv[alias] != "" {
			havePrivateKey = true
		}
	}
	if haveKey && havePrivateKey {
		return exitOK
	}
	return a.errorf("no credentials on this machine — run `openbox auth` first.\n"+
		"  `init` installs hooks and writes posture; it never registers an agent or writes a\n"+
		" credential. Nothing was installed.\n"+
		"  Expected %s and %s in %s, or as environment variables.",
		devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey, envPath)
}

// printGovernedScope closes `init` by saying which sessions this install actually
// governs.
//
// This string is the one place a user learns the truth about coverage, and it is
// pinned by a test for that reason. Installing a plugin is not activating it:
// global scope writes a correct config, reports success, and gates nothing until
// an administrator deploys managed settings. Silence there is the worst available
// outcome — the install looks finished, no session is governed, and the first
// evidence is an empty dashboard that reads as a broken product rather than an
// unfinished rollout.
func (a *app) printGovernedScope(o devinit.Options, resolvedScope string) {
	if o.Provider == "cursor" {
		// No adapter yet, so there is no scope to report; the manual-config path
		// already said so.
		return
	}

	if resolvedScope == scopeLocal && o.ProjectDir != "" {
		settings := filepath.Join(o.ProjectDir, ".claude", "settings.local.json")
		fmt.Fprintf(a.stdout, "\nGoverned: THIS PROJECT ONLY — %s\n", o.ProjectDir)
		fmt.Fprintf(a.stdout, "  Hooks were merged into %s, so the next session started here is governed.\n", settings)
		fmt.Fprintf(a.stdout, "  Sessions started in ANY OTHER directory are not governed and produce no events,\n")
		fmt.Fprintf(a.stdout, " so absence of events is not evidence of absence of work.\n")
		fmt.Fprintf(a.stdout, "  Run `openbox init` in each project you want governed, or `--scope global` for a fleet.\n")
		fmt.Fprintf(a.stdout, "  That settings file is per-developer and git-ignored by convention — do not commit it,\n")
		fmt.Fprintf(a.stdout, "  or your engine path lands on the whole team.\n")
		return
	}

	if o.Provider == "codex" {
		fmt.Fprintf(a.stdout, "\nGoverned: EVERY CODEX SESSION on this machine (user-wide hooks).\n")
		fmt.Fprintf(a.stdout, "  One more step inside Codex: run /hooks and TRUST the new OpenBox hooks —\n")
		fmt.Fprintf(a.stdout, "  until trusted they do not run.\n")
		return
	}

	fmt.Fprintf(a.stdout, "\nGoverned: NOTHING YET — activation is pending.\n")
	fmt.Fprintf(a.stdout, "  The bundle, engine and posture are installed, but global scope activates through\n")
	fmt.Fprintf(a.stdout, "  managed settings, which is an administrator's deployment and not something this\n")
	fmt.Fprintf(a.stdout, "  command can perform. Until that lands, no session is governed.\n")
	fmt.Fprintf(a.stdout, "  Add to the managed settings.json:  {\"enabledPlugins\": [\"openbox-observe\"]}\n")
	fmt.Fprintf(a.stdout, "  See `openbox managed install` and deployments/managed/.\n")
	fmt.Fprintf(a.stdout, "  For one project instead, re-run with --scope local.\n")
}

// initUsage prints only the flags `init` actually has.
//
// The moved and removed flags are still DECLARED on the flagset — that is what
// lets passing one produce a clear error instead of "flag provided but not
// defined" — but advertising all eighteen would defeat the point of cutting the
// surface from seventeen to seven. So they are named in one line as a pointer,
// not listed as options.
func (a *app) initUsage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(a.stderr, "Usage: openbox init --provider <claude-code|codex|cursor> [flags]\n\n")
		fmt.Fprintf(a.stderr, "Installs the tool's hooks and writes posture. Run `openbox auth` first —\n")
		fmt.Fprintf(a.stderr, "this command never reads, writes or prompts for a credential.\n\n")
		// The gateway flags are listed rather than left to the "not listed" pointer:
		// --gateway changes what a machine sends model traffic to, which is the
		// largest-blast-radius thing this command can do, and an undiscoverable
		// flag for it would be a support problem rather than a tidy help screen.
		// The lane flags are listed for the same reason as the gateway's: they
		// change what a machine sends model traffic to, and --full additionally
		// puts a CA on the developer's machine. Those are the
		// largest-blast-radius things this command does, and an undiscoverable
		// flag for one of them is a support problem rather than a tidy help
		// screen.
		for _, name := range []string{
			"provider", "scope", "enforce", "no-enforce", "install-git-hook",
			"full", "remove-all",
			"gateway", "remove-gateway", "gateway-addr", "gateway-upstream", "gateway-verbose",
			"telemetry", "remove-telemetry", "telemetry-addr",
			"transport", "remove-transport", "transport-addr",
			"lane-verbose", "force-restore",
			"role", "dry-run",
		} {
			f := fs.Lookup(name)
			if f == nil {
				continue
			}
			fmt.Fprintf(a.stderr, "  -%s\n        %s\n", f.Name, f.Usage)
		}
		fmt.Fprintf(a.stderr, "\nMoved to `openbox auth`: --org --agent-name --icon --description --base-url\n")
		fmt.Fprintf(a.stderr, "  --backend-url --force. Passing one here fails with a pointer rather than\n")
		fmt.Fprintf(a.stderr, "  being ignored.\n")
		fmt.Fprintf(a.stderr, "Removed: --secret-backend --client-id --managed-enable. Deprecated: --local-hooks\n")
		fmt.Fprintf(a.stderr, "  (use --scope local).\n")
	}
}
