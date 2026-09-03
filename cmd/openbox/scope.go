package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// flagPassed reports whether a flag was explicitly given on the command line.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// requireCredentials it must not half-install: a bundle installed against no
// identity produces hooks that fire, fail to resolve credentials, and fail
// open silently; an install that looks finished and governs nothing.
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
	return a.errorf("no credentials on this machine; run `openbox auth` first.\n"+
		"  `init` installs hooks and writes posture; it never registers an agent or writes a\n"+
		" credential. Nothing was installed.\n"+
		"  Expected %s and %s in %s, or as environment variables.",
		devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey, envPath)
}

// printGovernedScope states what this install governs, in the terms a reader
// needs to act on: which sessions, which file changed, what was swept, and --
// when the machine blocks hooks -- that nothing is governed despite a
// successful install.
func (a *app) printGovernedScope(o devinit.Options) {
	if o.Provider == "cursor" {
		return
	}

	if o.Provider == "codex" {
		fmt.Fprintf(a.stdout, "\nGoverned: EVERY CODEX SESSION on this machine (user-wide hooks).\n")
		fmt.Fprintf(a.stdout, "  One more step inside Codex: run /hooks and TRUST the new OpenBox hooks -\n")
		fmt.Fprintf(a.stdout, "  until trusted they do not run.\n")
		a.printHookBlockNotice()
		return
	}

	fmt.Fprintf(a.stdout, "\nGoverned: EVERY SESSION on this machine, in any directory.\n")
	fmt.Fprintf(a.stdout, "  Hooks were merged into %s.\n", providers.ClaudeUserSettingsPath())
	// Not "the next session": the tool's own file watcher picks up direct edits
	// to hooks in a settings file, so governance starts at once, including in
	// sessions that are already running. Promising a restart would undersell it.
	fmt.Fprintf(a.stdout, "  This takes effect IMMEDIATELY: the tool watches that file, so sessions already\n")
	fmt.Fprintf(a.stdout, "  running are governed too. There is nothing to restart.\n")
	fmt.Fprintf(a.stdout, "  Absence of events is therefore evidence about the work, not about the scope.\n")
	// Named whether or not anything was there: "only partly cleaned" is only
	// actionable if the reader can see which file this run actually looked at.
	if wd, err := os.Getwd(); err == nil {
		fmt.Fprintf(a.stdout, "  Swept any superseded OpenBox entry from %s;\n", providers.ClaudeProjectSettingsPath(wd))
		fmt.Fprintf(a.stdout, "  a project-level copy would register the same gate a second time.\n")
	}
	a.printHookBlockNotice()
}

// printHookBlockNotice is the one case where a successful install governs
// nothing: an org-managed machine can disable user-level hooks outright. Saying
// so here matters more than in doctor, because this is the moment somebody
// believes the machine is now governed.
func (a *app) printHookBlockNotice() {
	state := resolveHookBlock()
	if !state.blocked {
		return
	}
	fmt.Fprintf(a.stdout, "\n  BUT NOTHING IS GOVERNED BY THIS INSTALL: %s\n", state.summary)
	for _, line := range state.detail {
		fmt.Fprintf(a.stdout, "    %s\n", line)
	}
	fmt.Fprintf(a.stdout, "    The hooks, engine and posture are in place; the tool will not run them.\n")
}

func (a *app) initUsage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(a.stderr, "Usage: openbox init --provider <claude-code|codex|cursor> [flags]\n\n")
		fmt.Fprintf(a.stderr, "Installs the tool's hooks and writes posture. Run `openbox auth` first -\n")
		fmt.Fprintf(a.stderr, "this command never reads, writes or prompts for a credential.\n\n")
		for _, name := range []string{
			"provider", "enforce", "no-enforce", "install-git-hook",
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
		fmt.Fprintf(a.stderr, "Removed: --secret-backend --client-id --managed-enable --scope --local-hooks.\n")
		fmt.Fprintf(a.stderr, "  One install governs every session on this machine; there is no scope to pick.\n")
	}
}
