// Command openbox is the developer-runtime governance CLI.
//
// Setup is TWO commands, in this order, and the split is the point:
//
//	openbox auth    authenticate — collect or register credentials, write
//	                ~/.openbox/.env (secrets) and dev.json (coordinates)
//	openbox init    set up — install the provider's hooks at a chosen scope
//	                and write posture. Touches no credential, ever.
//
// `init` used to be both. Its own package comment described it as registering an
// agent, capturing credentials, AND delegating the tool config — and splitting on
// that "and" is what ADR-0015 did. What it bought: a command that runs in every
// developer's shell can no longer read, write or prompt for a secret, and a
// command that authenticates can now UPDATE, which `init` structurally could not
// (its reuse path returned before any network call, so "re-run init" was never a
// way to change a credential).
//
// A bare `init` governs the CURRENT DIRECTORY and ENFORCES (ADR-0016). Both
// defaults are deliberate reversals, and both are stated at install time rather
// than left to be discovered.
//
// OD17: single static Go binary, no cgo. Since ADR-0015 there is no platform
// secret-store subprocess either — credentials are a plaintext file, which is why
// this works identically on Windows.
package main

import (
	"context"
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/policysync"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/providers"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/provider"
	"io"
	"log"
	"os"
	"strings"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// Exit codes: 0 success, 1 error, 2 registered-but-config-manual (partial).
const (
	exitOK         = 0
	exitError      = 1
	exitConfigOnly = 2
)

// app holds the CLI's external dependencies behind seams so the command wiring
// — including the INV-1 credential guards — is testable without touching the
// real environment or network.
//
// There is no secret-store seam any more: credentials live in a plaintext file
// (ADR-0015), so a test points OPENBOX_HOME at a temp dir and exercises the same
// code production runs.
type app struct {
	stdout, stderr  io.Writer
	stdin           io.Reader
	getenv          func(string) string
	newRegistrar    func(baseURL, credential, clientID string) devinit.Registrar
	newPolicyReader func(baseURL, credential, clientID string) policyReader
}

// policyReader is the control-plane read `dev sync` + the `init`
// last-step need. backend.Client implements it; a fake injects in tests.
type policyReader interface {
	GetCurrentPolicy(ctx context.Context, agentID string) (*backend.Policy, error)
}

func defaultApp() *app {
	return &app{
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		stdin:           os.Stdin,
		getenv:          os.Getenv,
		newRegistrar:    func(u, c, id string) devinit.Registrar { return backend.New(u, c, id) },
		newPolicyReader: func(u, c, id string) policyReader { return backend.New(u, c, id) },
	}
}

func main() { os.Exit(defaultApp().run(os.Args[1:])) }

func (a *app) errorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "error: "+format+"\n", args...)
	return exitError
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		a.usage()
		return exitError
	}
	switch args[0] {
	case "auth":
		return a.runAuth(args[1:])
	case "init":
		return a.runInit(args[1:])
	case "dev":
		return a.runDev(args[1:])
	case "hook":
		return a.runHook(args[1:])
	case "rewake":
		return a.runRewake(args[1:])
	case "approve":
		return a.runApprove(args[1:])
	case "managed":
		return a.runManaged(args[1:])
	case "doctor":
		return a.runDoctor(args[1:])
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, "openbox "+version)
		return exitOK
	case "help", "--help", "-h":
		a.usage()
		return exitOK
	default:
		a.usage()
		return a.errorf("unknown command %q", args[0])
	}
}

func (a *app) runDev(args []string) int {
	if len(args) == 0 {
		return a.errorf("usage: openbox dev <verify|sync> [flags]")
	}
	switch args[0] {
	case "init":
		// Onboarding is `openbox init` — there is one spelling, and this is
		// not it. Saying where it went is an error message, not an alias: the
		// command does not run.
		return a.errorf("`openbox dev init` no longer exists — use `openbox init` (same flags), " +
			"or `openbox init --role approver` to install an approver")
	case "verify":
		return a.runDevVerify(args[1:])
	case "sync":
		return a.runDevSync(args[1:])
	default:
		return a.errorf("usage: openbox dev <verify|sync> [flags]")
	}
}

// runDevSync fetches this agent's current org policy from the control plane
// and writes it as the local policy bundle + pin (ADR-0005): the pull half
// of the pull-at-init + session-start-staleness distribution model. It reads
// the org control-plane credential from OPENBOX_CONTROL_TOKEN (never a
// flag — INV-1), resolves the agent id + backend URL persisted by
// `openbox init` (env overrides), fetches, translates config.policy_builder into a
// builder bundle (or a fail-open-local bundle for raw rego / a no-policy
// allow bundle), writes it 0600, and clears any fail-closed stale markers so
// the enforce gate proceeds. It never prints the org key or rego text
// (INV-1). On any auth/fetch failure it exits non-zero with a mapped hint
// and leaves the last-good bundle untouched.
func (a *app) runDevSync(args []string) int {
	fs := a.newFlagSet("openbox dev sync")
	var bundlePath, clientID string
	fs.StringVar(&bundlePath, "bundle", a.env("OPENBOX_SIDECAR_BUNDLE", ""), "local policy bundle to write (default: <os-config-dir>/openbox/policy-bundle.json — ~/.config on Linux, ~/Library/Application Support on macOS)")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	token := a.getenv("OPENBOX_CONTROL_TOKEN")
	if token == "" {
		return a.errorf("set OPENBOX_CONTROL_TOKEN (an obx_key_ organization key, or a Keycloak JWT) in the " +
			"environment; it is never accepted as a flag so it cannot leak via argv/shell history (INV-1)")
	}
	if problem := controlTokenProblem(token); problem != "" {
		return a.errorf("%s", problem)
	}
	backendURL := devconfig.ResolveBackendURL()
	if backendURL == "" {
		return a.errorf("no backend URL configured — set OPENBOX_BACKEND_URL or re-run `openbox init --backend-url <url>`")
	}
	agentID := devconfig.ResolveAgentID()
	if agentID == "" {
		return a.errorf("no agent id configured — run `openbox init --provider <tool>` first (it persists the agent id), or set OPENBOX_AGENT_ID")
	}
	if bundlePath == "" {
		bundlePath = hookflow.ResolveBundlePath()
	}

	if err := policysync.Run(context.Background(), a.newPolicyReader(backendURL, token, clientID), agentID, bundlePath, a.stdout); err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}

// runDevVerify is the read-only data-plane preflight: `openbox dev verify`
// resolves the finished dev.json + creds (reusing the adapter's resolvers),
// then a signed GET /api/v1/auth/validate against the actual core it emits
// to confirms the obx_ key + Ed25519 signing round-trip work. It is
// strictly read-only — no mint, no config write, no secret printed (INV-1)
// — and prints one ✓ line or a ✗ with the mapped fix hint.
func (a *app) runDevVerify(args []string) int {
	fs := a.newFlagSet("openbox dev verify")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan (method, path, base_url, DID); make no network call")
	fs.BoolVar(&dryRun, "print-plan", false, "alias for --dry-run")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	// --dry-run is fully offline: resolve only the NON-SECRET coordinates (no
	// credential-file read, no network) and print what the real run would call.
	if dryRun {
		baseURL, did := devconfig.ResolveCoordinates()
		fmt.Fprintln(a.stdout, "DRY RUN — openbox dev verify would call (no network, no secret access):")
		fmt.Fprintf(a.stdout, "  request:  GET %s%s\n", baseURL, client.AuthValidatePath)
		fmt.Fprintf(a.stdout, "  base_url: %s\n", baseURL)
		fmt.Fprintf(a.stdout, "  did:      %s\n", displayOrUnset(did))
		return exitOK
	}

	// Reuse the shared resolvers (dev.json for coordinates, env > ~/.openbox/.env
	// for the secrets — ADR-0015).
	// A missing identity means onboarding hasn't run — say so, don't half-proceed.
	creds, err := devconfig.ResolveCredentials()
	if err != nil {
		return a.errorf("cannot verify — %v.\n"+
			"  Run `openbox init --provider <claude-code|codex|cursor>` first, then retry.", err)
	}

	// client.New enforces the INV-1 TLS guard (refuses plaintext http:// to a
	// non-loopback core) and validates the identity shape before any network I/O.
	c, err := client.New(client.Config{
		BaseURL:       creds.BaseURL,
		APIKey:        creds.APIKey,
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
		// No Logger: Validate returns its diagnostic directly (not fail-open).
	})
	if err != nil {
		return a.errorf("%v", err)
	}

	if err := c.Validate(context.Background()); err != nil {
		// ✗ to stderr; the message is the mapped reason + fix hint. It never
		// contains the key/seed/nonce/signature (INV-1) — only status + guidance.
		fmt.Fprintf(a.stderr, "✗ %v\n", err)
		return exitError
	}
	// ✓ names only the DID + base_url (INV-1: no secret in output).
	fmt.Fprintf(a.stdout, "✓ verified: %s @ %s\n", creds.DID, creds.BaseURL)
	return exitOK
}

// displayOrUnset renders an empty coordinate as an actionable placeholder for the
// dry-run plan rather than a blank line.
func displayOrUnset(s string) string {
	if s == "" {
		return "(not configured — run `openbox init`)"
	}
	return s
}

// runHook is the unified observe-only hook entrypoint: `openbox hook
// <provider> <event>`. The plugin's hooks.json invokes it for every Claude
// Code hook.
//
// INV-3 (the reason this does not go through errorf/usage): the hook path
// must always return exitOK — a non-zero exit blocks the tool call. In
// observe mode it also writes nothing to stdout (on
// SessionStart/UserPromptSubmit an exit-0 hook's stdout is injected into the
// model's context). So every diagnostic (bad args, unknown provider, and
// everything inside RunHook) goes to stderr, and we unconditionally return
// 0. Folding the hook into the multi-command binary must not leak
// cobra/flag/usage/banner text to stdout.
//
// Enforce mode (opt-in) is the sole stdout writer: RunHook may emit a
// Claude Code PreToolUse permissionDecision (deny/ask) to a.stdout — still
// exit 0 (the decision travels in the JSON, not the exit code), still
// tighten-only. The permissionDecision JSON is the only structured stdout
// this path ever produces.
func (a *app) runHook(args []string) (code int) {
	// Belt-and-suspenders for INV-3: default to exitOK and swallow any panic that
	// escapes RunHook's own recover, so the hook path can NEVER return non-zero
	// (which would block the tool call).
	code = exitOK
	// A panic must never reach the caller's exit path, but swallowing it
	// silently makes a crashing hook indistinguishable from a working one.
	// Report it on stderr, which the tool shows as a diagnostic and never
	// parses as hook output.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(a.stderr, "openbox hook: recovered from panic: %v\n", r)
		}
	}()
	logger := log.New(a.stderr, "openbox hook: ", 0)
	if len(args) < 1 {
		logger.Printf("usage: openbox hook <provider> <event...>  (providers: %s, git)", strings.Join(provider.Supported(), ", "))
		return exitOK
	}
	// git is not a provider adapter — it is this binary re-invoked by the
	// prepare-commit-msg hook (OD17 folds the standalone git-hook binary in).
	if args[0] == "git" {
		// Supply the attestation signing context (E8-S10). The git module never
		// touches the secret store itself, so the engine injects a resolver;
		// returning ok=false leaves the commit unattested and the lineage
		// inferred, which is the pre-E8 behaviour.
		obgit.SetAttestContext(attestContext)
		obgit.RunHook(args[1:], []string{"hook", "git", "prepare-commit-msg"}, logger.Printf)
		return exitOK
	}

	// Every developer-tool provider dispatches through the registry, so adding
	// one is a registry entry rather than a case in this switch.
	engine, err := providers.Engine(args[0])
	if err != nil {
		logger.Printf("unknown hook provider %q (supported: %s, git)", args[0], strings.Join(provider.Supported(), ", "))
		return exitOK
	}
	if len(args) < 2 {
		logger.Printf("usage: openbox hook %s <event>", args[0])
		return exitOK
	}
	engine.RunHook(args[1], a.stdin, a.stdout, logger)
	return exitOK
}

// runRewake is the background approval watcher (E9 §2.2 Tier 2), invoked by an
// `asyncRewake` hook handler alongside the gate.
//
// It is deliberately NOT under `openbox hook`: that path is contractually
// exit-0 because a non-zero exit there blocks the tool call. Here the exit code
// IS the output — 2 wakes the session and shows stderr to the model as a system
// reminder — so it gets its own command rather than smuggling an exception
// through a path documented to never return non-zero. It writes nothing to
// stdout, and never blocks anything: a background handler has no tool call to
// hold.
func (a *app) runRewake(args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(a.stderr, "openbox rewake: recovered from panic: %v\n", r)
			code = exitOK
		}
	}()
	if len(args) < 1 {
		fmt.Fprintf(a.stderr, "usage: openbox rewake <provider>\n")
		return exitOK
	}
	engine, err := providers.Engine(args[0])
	if err != nil {
		fmt.Fprintf(a.stderr, "openbox rewake: unknown provider %q\n", args[0])
		return exitOK
	}
	// Not every host can wake a live session; those that cannot keep the
	// advisory findings channel. A provider without the capability is a
	// supported state, so this stays silent and exits 0.
	rw, ok := engine.(provider.Rewaker)
	if !ok {
		return exitOK
	}
	return rw.RunRewake(a.stdin, a.stderr, log.New(a.stderr, "openbox rewake: ", 0))
}

func (a *app) runDevInit(args []string) int {
	fs := a.newFlagSet("openbox init")
	var o devinit.Options
	var scope string
	// Flags that MOVED to `openbox auth` (ADR-0015) or were deleted. They are
	// still parsed so that passing one FAILS: a silently-ignored flag leaves a
	// script exiting 0 while the value it supplied goes nowhere, which is worse
	// than an error.
	var movedOrg, movedAgentName, movedIcon, movedDescription string
	var movedBaseURL, movedBackendURL string
	var movedForce bool
	var goneSecretBackend, goneClientID, goneLocalHooks string
	var goneManagedEnable bool

	// --- the seven flags `init` actually has --------------------------------
	fs.StringVar(&o.Provider, "provider", "", "developer tool: claude-code|codex|cursor (required)")
	fs.StringVar(&scope, "scope", "", "which sessions this install governs: local (default — this directory only) or global (every project, pending a managed-settings deployment)")
	var enforce, noEnforce bool
	fs.BoolVar(&enforce, "enforce", true, "ENFORCE mode: the PreToolUse hook blocks/asks/redacts in-process, no daemon and no runtime env. ON BY DEFAULT (ADR-0016) — inert until your org publishes a policy, and fail-open, so an OpenBox outage never blocks you. Pass --enforce=false to opt out; the opt-out persists.")
	fs.BoolVar(&noEnforce, "no-enforce", false, "alias for --enforce=false")
	fs.BoolVar(&o.InstallGitHook, "install-git-hook", false, "enable ambient install of the commit-trailer hook into repos on session start (off by default — it modifies .git/hooks)")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan; make no network or filesystem writes")
	// --role is consumed by runInit before dispatch; declared here so `init -h`
	// lists it and `--role approver` is not reported as an unknown flag.
	var role string
	fs.StringVar(&role, "role", "dev", "dev (default) or approver — an approver is a queue client, not a governed runtime")

	// --- moved to `openbox auth` -------------------------------------------
	fs.StringVar(&movedOrg, "org", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedAgentName, "agent-name", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedIcon, "icon", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedDescription, "description", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedBaseURL, "base-url", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedBackendURL, "backend-url", "", "MOVED to `openbox auth`")
	fs.BoolVar(&movedForce, "force", false, "MOVED to `openbox auth`")

	// --- removed -----------------------------------------------------------
	fs.StringVar(&goneSecretBackend, "secret-backend", "", "REMOVED — credentials live in ~/.openbox/.env; run `openbox auth`")
	fs.StringVar(&goneClientID, "client-id", "", "REMOVED — `init` makes no control-plane call")
	fs.BoolVar(&goneManagedEnable, "managed-enable", false, "REMOVED — recorded a Phase-1 substrate nothing read")
	fs.StringVar(&goneLocalHooks, "local-hooks", "", "DEPRECATED — use --scope local (accepted for one release)")

	fs.Usage = a.initUsage(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	// Every moved flag errors by name, pointing at the command that owns it now.
	for _, m := range []struct{ flag, value string }{
		{"--org", movedOrg}, {"--agent-name", movedAgentName}, {"--icon", movedIcon},
		{"--description", movedDescription}, {"--base-url", movedBaseURL}, {"--backend-url", movedBackendURL},
	} {
		if m.value != "" {
			return a.errorf("%s moved to `openbox auth` — `init` no longer registers agents or touches credentials (ADR-0015).\n"+
				"  Run:  openbox auth %s %s\n"+
				"  then: openbox init --provider %s", m.flag, m.flag, m.value, orDefault(o.Provider, "<tool>"))
		}
	}
	if movedForce {
		return a.errorf("--force moved to `openbox auth`: it governs agent REGISTRATION, which `init` no longer does.\n" +
			"  Run `openbox auth --force`, then `openbox init`.")
	}
	if goneSecretBackend != "" {
		return a.errorf("--secret-backend was removed: there is no secret store to choose any more.\n" +
			"  Credentials live in ~/.openbox/.env (plaintext, 0600 — see docs/adr/ADR-0015-plaintext-credential-file.md).\n" +
			"  Write them with `openbox auth`, then re-run `openbox init` without this flag.")
	}
	if goneClientID != "" {
		return a.errorf("--client-id was removed: `init` makes no control-plane call, so there is no header to set.\n" +
			"  Registration moved to `openbox auth`, which sets it itself.")
	}
	if goneManagedEnable {
		return a.errorf("--managed-enable was removed. It recorded a Phase-1 force-enable substrate in the agent's\n" +
			"  backend config that nothing ever read. Org-wide activation is a managed-settings\n" +
			"  deployment — see `openbox managed install` and deploy/managed/.")
	}

	if o.Provider == "" {
		return a.errorf("--provider is required (one of: claude-code, codex, cursor)")
	}

	// --- enforce posture ---------------------------------------------------
	// ON by default (ADR-0016), and the opt-out PERSISTS. Two distinct mechanisms
	// are needed for that, and getting only one of them was a real bug:
	//
	//  1. `Enforce` is a *bool, so an explicit false survives being marshalled.
	//     As a plain bool with `omitempty` it did not.
	//  2. **o.Enforce stays NIL when this run said nothing about enforce.** Because
	//     the flag DEFAULTS to true, its value alone cannot distinguish "the user
	//     asked to enforce" from "the user said nothing" — so assigning it
	//     unconditionally made every plain `init` write enforce:true, silently
	//     reverting a deliberate `--enforce=false` on the next unrelated re-run
	//     (adding --install-git-hook, repairing hooks, an idempotent setup script).
	//     That reintroduced the same bug class as (1), one layer up. `flagPassed`
	//     exists for exactly this distinction; use it.
	//
	// nil is what makes the default work in both directions: on a first install
	// there is no field, so ResolveEnforce returns its default (on); on a re-run
	// WriteConfig's tri-state merge carries the developer's choice forward. The
	// guarantee is about the RESOLVED posture, not about a literal true on disk.
	//
	// This flips `enforce` ONLY. The tier2/findings writes stay coupled to it as
	// they were, because the tier concept is being removed wholesale and
	// redesigning a doomed coupling would be wasted effort.
	enforceGiven := flagPassed(fs, "enforce")
	switch {
	case noEnforce && enforceGiven && enforce:
		return a.errorf("--enforce and --no-enforce are mutually exclusive")
	case noEnforce:
		f := false
		o.Enforce, o.Tier2, o.Findings = &f, &f, &f
	case enforceGiven:
		v := enforce // honours --enforce=false as well as --enforce
		o.Enforce, o.Tier2, o.Findings = &v, &v, &v
	}

	// A posture change is never silent, in either direction. Under enforce-by-
	// default this compares RESOLVED postures, so it still fires for a config
	// that never wrote the field.
	if devconfig.WouldDowngradeEnforce(devconfig.DefaultConfigPath(), o.Enforce) {
		fmt.Fprintln(a.stdout, "note: turning ENFORCE off — this machine was enforcing (explicitly, or by default).")
	}

	inst, err := providers.Lookup(o.Provider)
	if err != nil {
		return a.errorf("%v", err)
	}

	// --- scope -------------------------------------------------------------
	if goneLocalHooks != "" {
		if scope != "" {
			return a.errorf("--local-hooks and --scope cannot both be given; --local-hooks is the deprecated spelling of --scope local")
		}
		fmt.Fprintf(a.stderr, "warning: --local-hooks is deprecated — use --scope local (this release still accepts it).\n"+
			"         Project scope is now the DEFAULT, so in most cases the flag can be dropped entirely.\n")
		scope = scopeLocal
		o.ProjectDir = goneLocalHooks
	}
	resolvedScope, code := a.resolveScope(scope, o.Provider)
	if code != exitOK {
		return code
	}
	switch resolvedScope {
	case scopeLocal:
		if o.ProjectDir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return a.errorf("cannot resolve the current directory for --scope local: %v", err)
			}
			o.ProjectDir = wd
		}
	case scopeGlobal:
		o.ProjectDir = "" // global scope performs no project merge
	}

	d := devinit.Deps{Installer: inst, Out: a.stdout}

	// Dry-run is fully offline: no backend, no credential, no filesystem write.
	if o.DryRun {
		if _, err := devinit.Run(context.Background(), o, d); err != nil {
			return a.errorf("%v", err)
		}
		// Whether the plan governs anything is part of the plan, and dry-run is
		// where a careful operator looks before committing to it.
		a.printGovernedScope(o, resolvedScope)
		return exitOK
	}

	// --- credentials must already exist ------------------------------------
	// `init` performs NO registration and writes NO credential (ADR-0015). When
	// credentials are absent it exits non-zero naming `auth`; it does not prompt
	// and it does not half-install. That absence is the security property: a
	// command run in every developer's shell can no longer read, write or prompt
	// for a secret.
	a.migrateLegacyConfig()
	if code := a.requireCredentials(); code != exitOK {
		return code
	}

	res, runErr := devinit.Run(context.Background(), o, d)
	// A registered-but-config-manual result is a real partial success worth a
	// distinct exit code so scripts can tell it apart from a hard failure.
	if runErr != nil && res != nil && res.ConfigManualOnly {
		fmt.Fprintln(a.stderr, "note: "+runErr.Error())
		return exitConfigOnly
	}
	if runErr != nil {
		return a.errorf("%v", runErr)
	}

	// Codex hash-trusts non-managed hooks — until the user trusts the
	// freshly-written entries via /hooks inside Codex, they do not run.
	// Surface that as the explicit next step (re-install re-hashes, so it
	// applies to re-inits too).
	if o.Provider == "codex" && res != nil && res.ConfigApplied {
		fmt.Fprintln(a.stdout, "Next step: open Codex and run /hooks to review and TRUST the new OpenBox hooks — they do not run until trusted (Codex hash-trusts non-managed hooks; re-running init re-hashes them).")
	}

	// `init`'s last step best-effort pulls the agent's current policy into the
	// local bundle, so enforce mode has a policy on first run. It matters more now
	// that enforce is the default (ADR-0016): without a bundle, enforcement is
	// inert, which is safe but also means a fresh install gates nothing.
	//
	// Strictly best-effort — a fetch failure warns to stderr and does not fail the
	// install, because the hooks and posture are already in place and
	// `openbox dev sync` can complete it later. The coordinates come from
	// config/env rather than from this command's flags: `init` takes no
	// credential (ADR-0015), so if none is exported there is simply nothing to
	// sync with, and that is a note rather than a problem.
	a.syncInitialPolicy()

	// One closing block so a first-time user knows what to do next and how to
	// check it. Onboarding that ends in a wall of notes reads as "did that
	// work?", and the answer is one command away.
	// NOT "governance is ambient from here" — it used to say that, and under a
	// project-scoped default it directly contradicts the scope report below. The
	// ambient part is true of the MECHANISM (no daemon, nothing to keep running),
	// not of the COVERAGE, and conflating the two is the overstatement this
	// product exists to avoid.
	fmt.Fprintf(a.stdout, "\nDone. Nothing to run and no environment to keep set — the hooks do the rest.\n")
	fmt.Fprintf(a.stdout, "  openbox dev verify     confirm this machine can reach and authenticate to OpenBox\n")
	fmt.Fprintf(a.stdout, "  openbox doctor         the effective posture, and where each value came from\n")
	if o.Enforce != nil && !*o.Enforce {
		fmt.Fprintf(a.stdout, "  mode: OBSERVE — telemetry and lineage only, by your explicit --enforce=false.\n")
	} else {
		fmt.Fprintf(a.stdout, "  mode: ENFORCE — tool calls are gated in-process. Inert until your org publishes a\n")
		fmt.Fprintf(a.stdout, "        policy, and fail-open, so an OpenBox outage never blocks you. `--enforce=false` opts out.\n")
	}
	a.printGovernedScope(o, resolvedScope)
	return exitOK
}

// syncInitialPolicy best-effort fetches this agent's policy bundle at the end of
// an install. Silent when there is no credential to fetch with — `init` does not
// take one.
func (a *app) syncInitialPolicy() {
	agentID := devconfig.ResolveAgentID()
	if agentID == "" || a.newPolicyReader == nil {
		return
	}
	credential := a.getenv(devconfig.EnvControlToken)
	backendURL := devconfig.ResolveBackendURL()
	if credential == "" || backendURL == "" {
		fmt.Fprintf(a.stdout, "  note: no initial policy fetched (no %s exported). Enforcement is inert until a\n", devconfig.EnvControlToken)
		fmt.Fprintf(a.stdout, "        policy is present; run `openbox dev sync` with an org key when ready.\n")
		return
	}
	if err := policysync.Run(context.Background(), a.newPolicyReader(backendURL, credential, "openbox-cli"),
		agentID, hookflow.ResolveBundlePath(), a.stdout); err != nil {
		fmt.Fprintf(a.stderr, "note: initial policy sync skipped (%v); run `openbox dev sync` when ready.\n", err)
	}
}

func (a *app) env(key, def string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return def
}

func (a *app) usage() {
	fmt.Fprint(a.stderr, `openbox — OpenBox developer-runtime governance CLI

Setup is two commands, in this order:
  openbox auth                                        credentials for this machine
  openbox init --provider <claude-code|codex|cursor>  install hooks + posture

Usage:
  openbox auth [--provider <tool>] [--org <id>] [--rotate] [flags]
  openbox init --provider <claude-code|codex|cursor> [--scope local|global] [flags]
  openbox init --role approver --org <id> [--host claude-code] [flags]
  openbox dev verify [--dry-run]
  openbox dev sync [--bundle <path>]
  openbox managed install --provider <claude-code,codex> [--dry-run] [--force]
  openbox approve list [--org <id>] [--watch]
  openbox approve <allow|deny> <event-id> [--org <id>]
  openbox approve --watch --auto [--host claude-code] [--decide]   (ADR-0012)
  openbox doctor
  openbox version

Environment (needed only at 'auth' time, and only to register a new agent):
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key).
                          Never a flag, so it cannot leak via argv or shell history.
  OPENBOX_BACKEND_URL     openbox-backend CONTROL-PLANE base URL (or --backend-url)
  OPENBOX_BASE_URL        openbox-core DATA-PLANE base URL (or --base-url). Self-hosted?
                          Set BOTH: the control plane cannot tell the CLI where your
                          core is, so one default and one override sends events to the
                          hosted core and surfaces later as a 401.
  OPENBOX_ORG             organization namespace, used to derive the agent name

Credentials live in ~/.openbox/.env (plaintext, 0600 — ADR-0015); posture and
coordinates in ~/.openbox/dev.json. OPENBOX_HOME relocates both. A real
environment variable always wins over either file.

Nothing to run after 'init' — no daemon, no runtime env. Coverage is a separate
question from mechanism: a bare 'init' governs ONE directory (see --scope).
Run 'openbox auth -h' or 'openbox init -h' for flags.
`)
}
