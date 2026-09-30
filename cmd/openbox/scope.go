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

// credentialPlan is what requireCredentials decided: reuse the tool's own
// identity, or go and get one. It is a decision, not a state, which is why it
// is returned rather than re-derived -- asking the same question twice is how
// the register branch ends up running against a store that was present after
// all. That exact disagreement happened once between this gate and devinit's,
// so the predicate here and the one in devinit.register must stay the same
// question.
type credentialPlan struct {
	reuse bool
}

// requireCredentials it must not half-install: a bundle installed against no
// identity produces hooks that fire, fail to resolve credentials, and fail
// open silently; an install that looks finished and governs nothing.
//
// It no longer refuses for a missing organization token. Adopting an existing
// agent needs no fleet authority -- it is four values pasted by hand -- and
// gating it behind the token locked out the shape this restores: a developer
// holding their own agent's key while the org key lives with an administrator.
// The token is required by the register branch, where it is actually used, and
// that refusal names both routes to one.
func (a *app) requireCredentials() (credentialPlan, int) {
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		return credentialPlan{}, a.errorf("%v", err)
	}
	kv, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return credentialPlan{}, a.errorf("%v", err)
	}
	// A store carrying a legacy marker beside v3 keys is refused by the
	// resolver and re-registered by devinit, so it must not read as reusable
	// here either, or the register branch runs with no registrar wired.
	if ls, lerr := devconfig.LegacyStoreFor(devconfig.BoundProvider()); lerr == nil && ls.Legacy {
		kv = nil
	}
	return credentialPlan{reuse: a.credentialsPresent(kv)}, exitOK
}

// requireControlToken is the register branch's own gate, and the only refusal
// `init` has left.
func (a *app) requireControlToken() (token, source string, code int) {
	envPath, _ := devconfig.EnvFilePath()
	token, source = devconfig.ResolveControlTokenWithSource()
	if token == "" {
		return "", "", a.errorf(
			"no agent identity for this tool, and no organization credential to register one with.\n"+
				"  Nothing was installed.\n"+
				"  Run `openbox auth` (it stores the token), or export %s, then re-run.\n"+
				"  Already have this tool's agent? Re-run with a terminal and answer yes when it\n"+
				"  offers to adopt one; that needs no organization credential.\n"+
				"  Expected %s and %s in %s, or as environment variables.",
			devconfig.EnvControlToken, devconfig.EnvAPIKeyDirect, devconfig.EnvWorkloadPrivateKey, envPath)
	}
	if problem := controlTokenProblem(token); problem != "" {
		return "", "", a.errorf("%s\n  Nothing was installed.\n  That token came from %s.", problem, source)
	}
	return token, source, exitOK
}

// printGovernedScope states what this install governs, in the terms a reader
// needs to act on: which sessions, which file changed, what was swept, and --
// when the machine blocks hooks -- that nothing is governed despite a
// successful install.
func (a *app) printGovernedScope(o devinit.Options) {
	if o.Provider == "codex" {
		fmt.Fprintf(a.stdout, "\nGoverned: EVERY CODEX SESSION on this machine (user-wide hooks)\n")
		a.printPosture(o)
		a.row("one step", "run /hooks inside Codex and TRUST the new OpenBox hooks; until")
		a.row("", "trusted they do not run")
		a.printHookBlockNotice()
		return
	}
	if o.Provider == "muse" {
		fmt.Fprintf(a.stdout, "\nGoverned: EVERY MUSE SESSION on this machine (user-wide hooks)\n")
		a.row("hooks", "%s", providers.MuseSettingsPath())
		a.printPosture(o)
		// Muse reads settings.json at session start; whether an edit reaches a
		// running session is undocumented, so promise only the safe claim.
		a.row("restart", "open Muse sessions, so they load the new hooks")
		return
	}

	fmt.Fprintf(a.stdout, "\nGoverned: EVERY SESSION on this machine, in any directory\n")
	a.row("hooks", "%s", providers.ClaudeUserSettingsPath())
	// Not "the next session": the tool's own file watcher picks up direct edits
	// to hooks in a settings file, so governance starts at once, including in
	// sessions that are already running. Promising a restart would undersell it.
	a.row("", "live IMMEDIATELY: nothing to restart, and sessions already running")
	a.row("", "are governed too")
	a.printPosture(o)
	// The lanes are the half that a restart DOES gate: their env keys are read
	// once, at session start.
	a.row("restart", "open sessions, so their model calls reach the lanes above")
	// Named whether or not anything was there: "only partly cleaned" is only
	// actionable if the reader can see which file this run actually looked at.
	if wd, err := os.Getwd(); err == nil {
		project := providers.ClaudeProjectSettingsPath(wd)
		if fileExists(project) {
			a.row("checked", "%s", project)
			a.row("", "for a superseded project-level copy of the same gate")
		}
	}
	a.printHookBlockNotice()
}

// printPosture is what this install left switched on. One row each: the
// reasons they are the right defaults are `openbox doctor`'s, and the two
// environment variables that opt out are in this command's own --help.
func (a *app) printPosture(o devinit.Options) {
	a.row("posture", "mode: ENFORCE (always on), fail-closed; commit trailers ON")
	if o.Provider == "claude-code" {
		a.row("summaries", "ON; showThinkingSummaries, restored by `openbox uninstall`")
	}
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
		fmt.Fprintf(a.stderr, "Registers this tool's agent, then installs its hooks, the model-call lanes it\n")
		fmt.Fprintf(a.stderr, "supports, and posture. Run `openbox auth` first to connect the organization.\n")
		fmt.Fprintf(a.stderr, "Run this once per tool: each carries its own agent identity, in its own\n")
		fmt.Fprintf(a.stderr, "~/.openbox/<tool>/ store. A tool that already has one is reused offline.\n\n")
		if f := fs.Lookup("provider"); f != nil {
			fmt.Fprintf(a.stderr, "  -%s\n        %s\n", f.Name, f.Usage)
		}
		fmt.Fprintf(a.stderr, "\nThere are no other flags. One install governs every session on this machine,\n")
		fmt.Fprintf(a.stderr, "always enforcing, with every lane the provider supports and commit trailers on.\n")
		fmt.Fprintf(a.stderr, "The one posture that remains per-machine is an environment variable, not a flag:\n")
		fmt.Fprintf(a.stderr, "  OPENBOX_INSTALL_GIT_HOOK=false   do not touch any repo's .git/hooks\n")
		fmt.Fprintf(a.stderr, "Removal is `openbox uninstall`. The organization connection is `openbox auth`.\n")
	}
}

// credentialsPresent reports an API key plus a signing seed, from the
// environment or from an already-parsed .env. `init`'s gate and `uninstall`'s
// check ask the same question, so they ask it in one place: an alias added
// here reaches both.
//
// The environment half is devconfig's, not the app's getenv seam, and that is
// load-bearing rather than an oversight. Credential resolution at runtime goes
// through devconfig and reads the real environment; a gate that asked a
// different source could answer "present" where the resolver answers "absent",
// and it did -- a machine provisioned through exported variables passed this
// check, reached registration with no registrar wired, and was told it had hit
// a defect in the binary. The seam stays for what it is good at: reporting
// which variables are set, where a test wants to drive the message without
// touching the process environment.
func (a *app) credentialsPresent(kv map[string]string) bool {
	if devconfig.EnvIdentityPresent() {
		return true
	}
	// A seed under the documented legacy name or a deprecated alias no longer
	// satisfies this: it identifies a store devinit.register treats as legacy
	// (ErrLegacyStore), not a complete v3 identity this gate should reuse.
	return kv[devconfig.EnvAPIKeyDirect] != "" && kv[devconfig.EnvWorkloadPrivateKey] != ""
}
