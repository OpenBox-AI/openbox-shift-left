package main

import (
	"fmt"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// `openbox auth` connects this machine to an organization, and nothing else.
// It registers no agent and writes no agent credential: identity is per tool
// now, so `openbox init --provider <tool>` mints that tool's agent. What is
// left here is the part every tool shares -- two URLs and the org control
// token -- and it is written only to the org-level files.

type authFields struct {
	backendURL   string
	baseURL      string
	controlToken string
}

func (a *app) runAuth(args []string) int {
	fs := a.newFlagSet("openbox auth")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	// No flags at all. Every one of the thirteen was a way to do this without a
	// terminal, and each was also a way to get it wrong: a secret in argv, a
	// confirmation skipped, a credential file written somewhere the hooks do not
	// read. Provisioning without a terminal is still possible and better served
	// by the environment or the files themselves, which already outrank
	// anything this command writes.
	if fs.NArg() > 0 {
		return a.errorf("`openbox auth` takes no arguments or flags; it prompts. Got %q.\n%s",
			fs.Arg(0), prompt.NonInteractiveHelp)
	}

	a.migrateLegacyConfig()

	// Explicitly the org file, not EnvFilePath(): this command is the one that
	// writes a credential every tool shares, and nothing it writes may land in
	// a per-tool store.
	envPath, err := devconfig.OrgEnvFilePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	existing, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return a.errorf("%v", err)
	}
	cfg, _ := devconfig.Load(devconfig.DefaultConfigPath())

	// Prefilled from what is on the machine, so a re-run to fix one URL keeps
	// the token. The token itself prefills only as "there is one"; Secret never
	// echoes a stored value.
	f := authFields{
		backendURL: firstNonEmptyStr(cfg.BackendURL, devconfig.DefaultBackendURL),
		baseURL:    firstNonEmptyStr(cfg.BaseURL, devconfig.DefaultBaseURL),
	}
	f.controlToken = existing[devconfig.EnvControlToken]

	p, err := a.newPrompt()
	if err != nil {
		return a.errorf("%v", err)
	}
	if f, err = collectAuthFields(p, f); err != nil {
		return a.errorf("%v", err)
	}

	// Caught here rather than at `init` time, so a bad paste is reported while
	// the person who pasted it is still looking at the terminal. An empty
	// answer is allowed: with hosted defaults `auth` must stay skippable, and
	// `init` refuses later and names both routes.
	if problem := controlTokenProblem(f.controlToken); problem != "" {
		return a.errorf("%s", problem)
	}
	// Both URLs are chosen here, so this is where the mismatch is visible. One
	// default and one override sends events to the hosted core and surfaces
	// later as a 401 with nothing naming a URL.
	if selfHostedWithoutDataPlane(f.backendURL, f.baseURL) {
		fmt.Fprintf(a.stderr,
			"warning: the backend is %s but the core URL is the hosted default (%s).\n"+
				"         If OpenBox is self-hosted, set the core URL to your own openbox-core.\n",
			f.backendURL, devconfig.DefaultBaseURL)
	}

	if code := a.writeSecrets(envPath, f); code != exitOK {
		return code
	}
	if code := a.writeCoordinates(f); code != exitOK {
		return code
	}
	a.warnShadowedByEnv(envPath)
	a.printAuthNextSteps()
	return exitOK
}

func collectAuthFields(p prompt.Prompter, f authFields) (authFields, error) {
	var err error
	if f.backendURL, err = p.Line("Backend URL (control plane)", f.backendURL); err != nil {
		return f, err
	}
	if f.baseURL, err = p.Line("Core URL (data plane)", f.baseURL); err != nil {
		return f, err
	}
	token, err := p.Secret("Organization control token (obx_key_… or JWT)", f.controlToken != "")
	if err != nil {
		return f, err
	}
	if token != "" {
		f.controlToken = token
	}
	return f, nil
}

// writeSecrets writes the org control token to the org-level credential file,
// and nothing else.
//
// This file used to hold this machine's agent key and seed, and deliberately
// not the control token. Both halves of that reversed. The agent credential is
// per tool now and `init` writes it; the control token is persisted here
// because `auth` takes it and `init` needs it, and those are two processes with
// nothing exported between them.
//
// Say what that costs, because no comment or document in this repo may imply
// otherwise: this credential creates and rotates agents across the whole
// organization, the file is plaintext, 0600 on macOS and Linux and unprotected
// on Windows, and anything running as the developer -- including the governed
// agent itself -- can read it. `uninstall` deletes it along with everything
// else. Persisting it buys the auth/init split; it hardens nothing.
func (a *app) writeSecrets(envPath string, f authFields) int {
	token := strings.TrimSpace(f.controlToken)
	if token == "" {
		// Writing the key with an empty value would put a credential name in a
		// file that holds no credential, which reads as "configured" to the next
		// person to open it.
		fmt.Fprintf(a.stdout, "- no organization control token given; %s was not written.\n"+
			"  `openbox init --provider <tool>` needs one to register that tool's agent,\n"+
			"  and will say so if it is still missing.\n", envPath)
		return exitOK
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{devconfig.EnvControlToken: token}); err != nil {
		return a.errorf("write credentials: %v", err)
	}
	fmt.Fprintf(a.stdout, "✓ wrote %s (0600; plaintext;)\n", envPath)
	return exitOK
}

// writeCoordinates deliberately not provider.ConfigUpdate: that always sets
// InstallGitHook to a non-nil value (provider/config.go), so routing through
// it would make `auth` write posture. And deliberately unbound: these are the
// org's coordinates, which `init` reads once and carries into each tool's own
// config.
func (a *app) writeCoordinates(f authFields) int {
	path, err := devconfig.DevConfigWritePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	if err := devconfig.WriteConfig(path, devconfig.Update{
		BackendURL: strings.TrimSpace(f.backendURL),
		BaseURL:    strings.TrimSpace(f.baseURL),
	}); err != nil {
		return a.errorf("write dev config: %v", err)
	}
	fmt.Fprintf(a.stdout, "✓ wrote %s  (URLs; no secrets)\n", path)
	return exitOK
}

// warnShadowedByEnv an exported variable and a file that disagree produce a
// run whose behaviour does not match what was just written, in one direction or
// the other. Both directions are worth a line, and they are not the same line.
//
// The URLs are shadowed the usual way: the variable wins, so the file this run
// wrote will not be used until it is unset.
//
// The control token goes the other way, and the warning has to say so or it
// sends its reader to unset a variable that is already being ignored -- or
// worse, leaves them believing an export they forgot is still in force. Saying
// nothing is not an option either: a developer who exported it deliberately for
// this run needs to know it did not take.
//
// Two pairs plus one inversion, not six: `auth` no longer writes an agent key,
// seed, DID or agent id, and warning about a file this run did not touch would
// send somebody unsetting a variable that is doing no harm.
func (a *app) warnShadowedByEnv(envPath string) {
	devPath, _ := devconfig.DevConfigWritePath()
	for _, s := range []struct{ name, file string }{
		{devconfig.EnvBaseURL, devPath},
		{devconfig.EnvBackendURL, devPath},
	} {
		if a.getenv(s.name) == "" {
			continue
		}
		fmt.Fprintf(a.stderr, "warning: %s is set in this environment, so it overrides what was just written to %s.\n"+
			"         The file is correct; this shell will not use it. Unset the variable to use the file.\n",
			s.name, s.file)
	}
	if a.getenv(devconfig.EnvControlToken) != "" {
		fmt.Fprintf(a.stderr, "warning: %s is also set in this environment, and it is being IGNORED.\n"+
			"         %s now takes precedence for it, so the token just written is the one\n"+
			"         `openbox init` will send. Unset the variable if you meant to use it.\n",
			devconfig.EnvControlToken, envPath)
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (a *app) printAuthNextSteps() {
	fmt.Fprintf(a.stdout, "\nNext: openbox init --provider <%s>\n", strings.Join(provider.Supported(), "|"))
	fmt.Fprintf(a.stdout, "  That registers an agent for the tool, installs its hooks and every model-call\n")
	fmt.Fprintf(a.stdout, "  lane it supports. One run governs every session on this machine, in any\n")
	fmt.Fprintf(a.stdout, "  directory; there is no scope to choose and nothing to repeat per project.\n")
	fmt.Fprintf(a.stdout, "  Run it once per tool: each carries its own agent identity.\n")
	// The URLs written above are the organization's, and `init` copies them into
	// each tool's own config -- one file is loaded, never merged over another.
	// So a re-run that corrects a URL has no runtime effect until every already
	// installed tool re-runs init, and a person who is not told that will watch
	// a corrected core URL change nothing.
	fmt.Fprintf(a.stdout, "  Already installed? Re-run it for each tool to pick up a changed URL.\n")
}
