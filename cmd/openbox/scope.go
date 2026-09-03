package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

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
		project := providers.ClaudeProjectSettingsPath(wd)
		if fileExists(project) {
			fmt.Fprintf(a.stdout, "  Checked %s for a superseded OpenBox entry;\n", project)
			fmt.Fprintf(a.stdout, "  a project-level copy would register the same gate a second time. Anything\n")
			fmt.Fprintf(a.stdout, "  removed, or any reason it could not be, is reported above.\n")
		}
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
	// A mandated fleet is the shape the lock exists for, and OpenBox's own
	// managed template is exactly that: it declares these hooks itself. Shouting
	// "nothing is governed" there would be false and would send somebody looking
	// for a gap that is not there.
	if state.governedElsewhere {
		fmt.Fprintf(a.stdout, "\n  NOTE; this machine is governed by managed policy: %s\n", state.summary)
		for _, line := range state.detail {
			fmt.Fprintf(a.stdout, "    %s\n", line)
		}
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
		fmt.Fprintf(a.stderr, "Usage: openbox init --provider <%s>\n\n",
			strings.Join(provider.Supported(), "|"))
		fmt.Fprintf(a.stderr, "Installs the tool's hooks, the model-call lanes it supports, and posture.\n")
		fmt.Fprintf(a.stderr, "Run `openbox auth` first; this command never reads, writes or prompts for a\n")
		fmt.Fprintf(a.stderr, "credential.\n\n")
		if f := fs.Lookup("provider"); f != nil {
			fmt.Fprintf(a.stderr, "  -%s\n        %s\n", f.Name, f.Usage)
		}
		fmt.Fprintf(a.stderr, "\nThere are no other flags. One install governs every session on this machine,\n")
		fmt.Fprintf(a.stderr, "enforcing, with every lane the provider supports and commit trailers on. The\n")
		fmt.Fprintf(a.stderr, "two postures that remain per-machine are environment variables, not flags:\n")
		fmt.Fprintf(a.stderr, "  OPENBOX_ENFORCE=false            observe only, for this run\n")
		fmt.Fprintf(a.stderr, "  OPENBOX_INSTALL_GIT_HOOK=false   do not touch any repo's .git/hooks\n")
		fmt.Fprintf(a.stderr, "Removal is `openbox uninstall`. Credentials are `openbox auth`.\n")
	}
}
