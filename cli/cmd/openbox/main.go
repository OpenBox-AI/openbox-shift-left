// Command openbox is the developer-runtime governance CLI.
//
// The front door is `openbox init --provider <tool>`: it registers a
// developer agent with OpenBox, captures the agent's credentials into the OS
// secret store, and delegates the tool's native config to that provider's
// adapter installer. After that, governance is ambient — no further UI.
//
// OD17: single static Go binary; no cgo. The OS secret store is reached by
// shelling out to the platform tool (libsecret / macOS security).
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/policysync"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/providers"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
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
// — including the INV-1 credential guards and the HALT-on-no-store path — is
// testable without touching the real environment, OS keychain, or network.
type app struct {
	stdout, stderr  io.Writer
	stdin           io.Reader
	getenv          func(string) string
	openStore       func(kind string) (secret.Store, error)
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
		openStore:       secret.Open,
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
	fs.StringVar(&bundlePath, "bundle", a.env("OPENBOX_SIDECAR_BUNDLE", ""), "local policy bundle to write (default: $XDG_CONFIG_HOME/openbox/policy-bundle.json)")
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
	// keychain read, no network) and print what the real run would call.
	if dryRun {
		baseURL, did := devconfig.ResolveCoordinates()
		fmt.Fprintln(a.stdout, "DRY RUN — openbox dev verify would call (no network, no secret access):")
		fmt.Fprintf(a.stdout, "  request:  GET %s%s\n", baseURL, client.AuthValidatePath)
		fmt.Fprintf(a.stdout, "  base_url: %s\n", baseURL)
		fmt.Fprintf(a.stdout, "  did:      %s\n", displayOrUnset(did))
		return exitOK
	}

	// Reuse the adapter's resolvers (dev.json + OS keychain / file backend / env).
	// A missing identity means onboarding hasn't run — say so, don't half-proceed.
	creds, err := devconfig.ResolveCredentials(devconfig.OSSecretLookup)
	if err != nil {
		return a.errorf("cannot verify — %v.\n"+
			"  Run `openbox init --provider <claude-code|codex|cursor>` first, then retry.", err)
	}

	// client.New enforces the INV-1 TLS guard (refuses plaintext http:// to a
	// non-loopback core) and validates the identity shape before any network I/O.
	c, err := client.New(client.Config{
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		DID:     creds.DID,
		SeedB64: creds.SeedB64,
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
	var backendURL, clientID, secretBackend string
	fs.StringVar(&o.Provider, "provider", "", "developer tool: claude-code|codex|cursor (required)")
	fs.StringVar(&o.Org, "org", a.env("OPENBOX_ORG", ""), "organization namespace for credential storage")
	fs.StringVar(&o.AgentName, "agent-name", "", "override the derived agent name")
	fs.StringVar(&o.Icon, "icon", "", "agent icon string (backend requires non-empty; defaults to an emoji)")
	fs.StringVar(&o.Description, "description", "OpenBox developer-runtime agent", "agent description")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan; make no network or secret-store writes")
	fs.BoolVar(&o.Force, "force", false, "register a new distinctly-named agent even if one exists remotely")
	fs.BoolVar(&o.ManagedEnable, "managed-enable", false, "record the org force-enable substrate (Phase-1: verified, not activated)")
	fs.BoolVar(&o.InstallGitHook, "install-git-hook", false, "enable ambient install of the commit-trailer hook into repos on session start (off by default — it modifies .git/hooks)")
	fs.StringVar(&o.LocalHooksDir, "local-hooks", "", "LOCAL TESTING opt-in: also merge the hook block into <dir>/.claude/settings.local.json so only sessions in that project are governed (production posture is managed-settings/global activation; never set this in production)")
	var enforce, noEnforce bool
	fs.BoolVar(&enforce, "enforce", false, "turn on ENFORCE mode and persist it to dev.json (ADR-0006): the PreToolUse hook blocks/asks/redacts in-process — no daemon, no runtime env. Also enables Tier-2 sync escalation + the Tier-3 findings loop. Off by default = observe-only.")
	fs.BoolVar(&noEnforce, "no-enforce", false, "turn ENFORCE mode off and persist that. Needed because a plain re-init leaves an existing posture alone rather than silently downgrading it.")
	fs.StringVar(&backendURL, "backend-url", a.env("OPENBOX_BACKEND_URL", ""), "openbox-backend control-plane base URL")
	fs.StringVar(&o.BaseURL, "base-url", a.env(devconfig.EnvBaseURL, ""), "openbox-core DATA-PLANE base URL (where events are sent and `dev verify` authenticates). Required for a self-hosted core: the backend's registration reply carries no data-plane URL, so leaving this unset points the install at the SaaS core")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	fs.StringVar(&secretBackend, "secret-backend", a.env("OPENBOX_SECRET_BACKEND", "auto"), "credential store: auto|os (OS keychain, default) or file (opt-in 0600 plaintext file, for machines with no OS keyring)")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if o.Provider == "" {
		return a.errorf("--provider is required (one of: claude-code, codex, cursor)")
	}
	o.BackendURL = backendURL // persist the control-plane base for `dev sync`/staleness
	// ADR-0006: `--enforce` is the one-flag enforce posture. It turns on
	// enforce and its sensible companions (Tier-2 sync escalation + the
	// Tier-3 findings loop) and persists all three to dev.json, so the
	// runtime hook needs no env var. Granular per-toggle tuning remains
	// available via the dev.json fields / OPENBOX_* env overrides.
	// Fail-open stays the default failure policy — enforce never implies
	// fail-closed.
	//
	// Saying nothing leaves o.Enforce nil, which preserves whatever dev.json
	// already holds. Turning enforcement off is therefore an explicit act:
	// re-running `init` to repair an install used to drop the developer
	// back to observe with no indication it had happened.
	switch {
	case enforce && noEnforce:
		return a.errorf("--enforce and --no-enforce are mutually exclusive")
	case enforce:
		t := true
		o.Enforce, o.Tier2, o.Findings = &t, &t, &t
	case noEnforce:
		f := false
		o.Enforce, o.Tier2, o.Findings = &f, &f, &f
	}
	// A posture change is never silent, in either direction.
	if devconfig.WouldDowngradeEnforce(devconfig.DefaultConfigPath(), o.Enforce) {
		fmt.Fprintln(a.stdout, "note: turning ENFORCE off — dev.json currently has enforce: true")
	}

	inst, err := providers.Lookup(o.Provider)
	if err != nil {
		return a.errorf("%v", err)
	}

	d := devinit.Deps{Installer: inst, Out: a.stdout}

	// A self-hosted control plane with the default data plane is the one silent
	// misconfiguration that looks like a broken install later.
	if selfHostedWithoutDataPlane(backendURL, o.BaseURL) {
		fmt.Fprintf(a.stderr,
			"warning: --backend-url points at %s but --base-url is unset, so events and `dev verify` will go\n"+
				"         to %s. If OpenBox is self-hosted, re-run with --base-url <your-openbox-core>.\n",
			backendURL, devconfig.DefaultBaseURL)
	}

	// Dry-run is fully offline: no secret store, no backend, no credential.
	if o.DryRun {
		if _, err := devinit.Run(context.Background(), o, d); err != nil {
			return a.errorf("%v", err)
		}
		return exitOK
	}

	// Credential comes from the environment, never a flag/argv (INV-1).
	credential := a.getenv("OPENBOX_CONTROL_TOKEN")
	if credential == "" {
		return a.errorf("set OPENBOX_CONTROL_TOKEN (an obx_key_ organization key, or a Keycloak JWT) in the\n" +
			"  environment; it is never accepted as a flag so it cannot leak via argv/shell history (INV-1).\n" +
			"  Get one from the dashboard → Organization → API Keys. Note this is NOT the key shown on an\n" +
			"  agent's page — see docs/getting-started.md § Get the right credential.")
	}
	// Say which key they pasted rather than letting the backend answer 401.
	if problem := controlTokenProblem(credential); problem != "" {
		return a.errorf("%s", problem)
	}
	if backendURL == "" {
		return a.errorf("set --backend-url or OPENBOX_BACKEND_URL to the openbox-backend base URL")
	}

	store, err := a.openStore(secretBackend)
	if err != nil {
		if errors.Is(err, secret.ErrNoStore) {
			// Stop condition: no OS keychain and the operator did not opt
			// into an alternative → HALT, never silently write plaintext
			// (INV-1).
			return a.errorf("HALT: %v — refusing to store credentials in plaintext by default.\n"+
				"  Fix by EITHER:\n"+
				"  • installing an OS keyring — Linux: `secret-tool` (e.g. apt install libsecret-tools) with a running\n"+
				"    keyring daemon (gnome-keyring/kwallet); macOS has one built in; then re-run; OR\n"+
				"  • opting into the file backend for this machine (0600 plaintext, no keyring needed):\n"+
				"      openbox init --provider %s --secret-backend file   (or OPENBOX_SECRET_BACKEND=file)",
				err, o.Provider)
		}
		return a.errorf("%v", err)
	}
	// The file backend trades away at-rest encryption; make that explicit and
	// tell the operator how to point the adapter hook at the same store.
	if secretBackend == "file" {
		fmt.Fprintf(a.stderr,
			"warning: using the file secret backend — credentials are stored PLAINTEXT (0600) at %s.\n"+
				"         Prefer an OS keyring where available. The Claude Code hook reads this store when you set\n"+
				"         OPENBOX_SECRET_FILE=%s in its environment.\n",
			secret.DefaultFilePath(), secret.DefaultFilePath())
	}
	d.Store = store
	d.Registrar = a.newRegistrar(backendURL, credential, clientID)

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

	// `init`'s last step best-effort pulls the agent's current policy
	// into the local bundle so enforce mode has a policy on first run. It
	// is best-effort — a fetch failure warns (stderr) and does not fail
	// init (the agent is already registered and configured; the user can
	// re-run `openbox dev sync`). The agent id was persisted by the
	// installer; resolve it back out.
	if agentID := devconfig.ResolveAgentID(); agentID != "" && a.newPolicyReader != nil {
		bundlePath := hookflow.ResolveBundlePath()
		if err := policysync.Run(context.Background(), a.newPolicyReader(backendURL, credential, clientID), agentID, bundlePath, a.stdout); err != nil {
			fmt.Fprintf(a.stderr, "note: initial policy sync skipped (%v); run `openbox dev sync` when ready.\n", err)
		}
	}

	// One closing block so a first-time user knows what to do next and how to
	// check it. Onboarding that ends in a wall of notes reads as "did that
	// work?", and the answer is one command away.
	fmt.Fprintf(a.stdout, "\nDone. Governance is ambient from here — no daemon, no environment to keep set.\n")
	fmt.Fprintf(a.stdout, "  openbox dev verify     confirm this machine can reach and authenticate to OpenBox\n")
	fmt.Fprintf(a.stdout, "  openbox doctor         the effective posture, and where each value came from\n")
	if o.Enforce == nil || !*o.Enforce {
		fmt.Fprintf(a.stdout, "  mode: OBSERVE — telemetry and lineage only. Re-run with --enforce to gate tool calls.\n")
	}
	return exitOK
}

func (a *app) env(key, def string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return def
}

func (a *app) usage() {
	fmt.Fprint(a.stderr, `openbox — OpenBox developer-runtime governance CLI

Usage:
  openbox init --provider <claude-code|codex|cursor> [--enforce] [flags]
  openbox init --role approver --org <id> [--host claude-code] [flags]
  openbox dev verify [--dry-run]
  openbox dev sync [--bundle <path>]
  openbox managed install --provider <claude-code,codex> [--dry-run] [--force]
  openbox approve list [--org <id>] [--watch]
  openbox approve <allow|deny> <event-id> [--org <id>]
  openbox approve --watch --auto [--host claude-code] [--decide]   (ADR-0012)
  openbox doctor
  openbox version

Environment (needed only at 'init' time):
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key)
  OPENBOX_BACKEND_URL     openbox-backend base URL (or --backend-url)
  OPENBOX_BASE_URL        openbox-core DATA-PLANE base URL (or --base-url) — set it for a
                          self-hosted core; unset means the SaaS core
  OPENBOX_ORG             organization namespace for credential storage

After 'init' governance is ambient — no daemon to run and no runtime env to set.
Run 'openbox init -h' for flags.
`)
}
