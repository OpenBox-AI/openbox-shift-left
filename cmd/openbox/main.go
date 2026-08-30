// Command openbox is the developer-runtime governance CLI.
package main

import (
	"context"
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
	"io"
	"log"
	"net"
	"os"
	"strings"
)

var version = "0.1.0-dev"

const (
	exitOK         = 0
	exitError      = 1
	exitConfigOnly = 2
)

type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	getenv         func(string) string
	newRegistrar   func(baseURL, credential, clientID string) devinit.Registrar

	gatewayReady func(net.Addr)
	gatewayCtx   context.Context

	telemetryReady func(addr string)
	telemetryCtx   context.Context

	transportReady func(addr string)
	transportCtx   context.Context
}

func defaultApp() *app {
	return &app{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		stdin:        os.Stdin,
		getenv:       os.Getenv,
		newRegistrar: func(u, c, id string) devinit.Registrar { return backend.New(u, c, id) },
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
	case "gateway":
		return a.runGateway(args[1:])
	case "telemetry":
		return a.runTelemetry(args[1:])
	case "transport":
		return a.runTransport(args[1:])
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
		return a.errorf("usage: openbox dev verify [flags]")
	}
	switch args[0] {
	case "init":
		return a.errorf("`openbox dev init` no longer exists; use `openbox init` (same flags), " +
			"or `openbox init --role approver` to install an approver")
	case "verify":
		return a.runDevVerify(args[1:])
	case "sync":
		return a.errorf("`openbox dev sync` no longer exists; policy is evaluated by OpenBox " +
			"on every gated tool call, so there is no local bundle to fetch. " +
			"Any leftover policy-bundle.json on this machine is inert and can be deleted.")
	default:
		return a.errorf("usage: openbox dev verify [flags]")
	}
}

func (a *app) runDevVerify(args []string) int {
	fs := a.newFlagSet("openbox dev verify")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan (method, path, base_url, DID); make no network call")
	fs.BoolVar(&dryRun, "print-plan", false, "alias for --dry-run")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if dryRun {
		baseURL, did := devconfig.ResolveCoordinates()
		fmt.Fprintln(a.stdout, "DRY RUN; openbox dev verify would call (no network, no secret access):")
		fmt.Fprintf(a.stdout, "  request:  GET %s%s\n", baseURL, client.AuthValidatePath)
		fmt.Fprintf(a.stdout, "  base_url: %s\n", baseURL)
		fmt.Fprintf(a.stdout, "  did:      %s\n", displayOrUnset(did))
		return exitOK
	}

	creds, err := devconfig.ResolveCredentials()
	if err != nil {
		return a.errorf("cannot verify; %v.\n"+
			"  Run `openbox init --provider <claude-code|codex|cursor>` first, then retry.", err)
	}

	c, err := client.New(client.Config{
		BaseURL:       creds.BaseURL,
		APIKey:        creds.APIKey,
		DID:           creds.DID,
		PrivateKeyB64: creds.PrivateKeyB64,
	})
	if err != nil {
		return a.errorf("%v", err)
	}

	if err := c.Validate(context.Background()); err != nil {
		// It never contains the key/seed/nonce/signature (INV-1); only status +
		// guidance.
		fmt.Fprintf(a.stderr, "✗ %v\n", err)
		return exitError
	}
	fmt.Fprintf(a.stdout, "✓ verified: %s @ %s\n", creds.DID, creds.BaseURL)
	return exitOK
}

func displayOrUnset(s string) string {
	if s == "" {
		return "(not configured; run `openbox init`)"
	}
	return s
}

// runHook iNV-3 (the reason this does not go through errorf/usage): the hook
// path must always return exitOK; a non-zero exit blocks the tool call.
func (a *app) runHook(args []string) (code int) {
	code = exitOK
	// Report it on stderr, which the tool shows as a diagnostic and never parses
	// as hook output.
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
	if args[0] == "git" {
		obgit.SetAttestContext(attestContext)
		obgit.RunHook(args[1:], []string{"hook", "git", "prepare-commit-msg"}, logger.Printf)
		return exitOK
	}

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

// runRewake is the background approval watcher (E9 §2.2), invoked by an
// `asyncRewake` hook handler alongside the gate.
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
	var movedOrg, movedAgentName, movedIcon, movedDescription string
	var movedBaseURL, movedBackendURL string
	var movedForce bool
	var goneSecretBackend, goneClientID, goneLocalHooks string
	var goneManagedEnable bool

	fs.StringVar(&o.Provider, "provider", "", "developer tool: claude-code|codex|cursor (required)")
	fs.StringVar(&scope, "scope", "", "which sessions this install governs: local (default; this directory only) or global (every project, pending a managed-settings deployment)")
	var enforce, noEnforce bool
	fs.BoolVar(&enforce, "enforce", true, "ENFORCE mode: the PreToolUse hook blocks/asks/redacts in-process, no daemon and no runtime env. ON BY DEFAULT; inert until your org publishes a policy, and fail-open, so an OpenBox outage never blocks you. Pass --enforce=false to opt out; the opt-out persists.")
	fs.BoolVar(&noEnforce, "no-enforce", false, "alias for --enforce=false")
	fs.BoolVar(&o.InstallGitHook, "install-git-hook", false, "enable ambient install of the commit-trailer hook into repos on session start (off by default; it modifies .git/hooks)")
	// OFF by default, deliberately: unlike enforcement-by-default, which is inert
	// without an org policy, this redirects live model traffic.
	var withGateway, removeGateway bool
	var gatewayAddr, gatewayUpstream string
	var gatewayVerbose bool
	fs.BoolVar(&withGateway, "gateway", false, "also install and start the local model-call gateway, and point this machine at it (OFF by default: it redirects live model traffic)")
	fs.BoolVar(&removeGateway, "remove-gateway", false, "stop the local gateway and remove only the configuration OpenBox owns")
	fs.StringVar(&gatewayAddr, "gateway-addr", gateway.DefaultAddr, "loopback address the gateway listens on")
	fs.StringVar(&gatewayUpstream, "gateway-upstream", gateway.DefaultUpstream, "provider base URL the gateway forwards to")
	fs.BoolVar(&gatewayVerbose, "gateway-verbose", false, "run the gateway with --verbose, logging every relayed call to ~/.openbox/gateway.log (no credentials, headers or bodies)")

	var withFull, removeAll bool
	var withTelemetry, removeTelemetryLane bool
	var withTransport, removeTransportLane bool
	var telemetryAddr, transportAddr string
	var laneVerbose, forceRestore bool
	fs.BoolVar(&withFull, "full", false, "install and enable everything: hooks, the telemetry receiver and the in-path transport relay")
	fs.BoolVar(&removeAll, "remove-all", false, "remove every OpenBox lane: restore all managed env keys, unload and delete all units, and delete the CA, the logs and the activation record. The spool is KEPT; it is shared with the hook path, which this does not remove")
	fs.BoolVar(&withTelemetry, "telemetry", false, "install and start the local OTLP telemetry receiver, and point the tool's own telemetry at it")
	fs.BoolVar(&removeTelemetryLane, "remove-telemetry", false, "stop the telemetry receiver and restore the env keys it displaced")
	fs.BoolVar(&withTransport, "transport", false, "install and start the in-path transport relay, and point the tool's proxy and CA trust at it")
	fs.BoolVar(&removeTransportLane, "remove-transport", false, "stop the transport relay and restore the env keys it displaced")
	fs.StringVar(&telemetryAddr, "telemetry-addr", telemetry.DefaultAddr, "loopback address the telemetry receiver listens on")
	fs.StringVar(&transportAddr, "transport-addr", transport.DefaultAddr, "loopback address the transport relay listens on")
	fs.BoolVar(&laneVerbose, "lane-verbose", false, "run the telemetry and transport daemons with --verbose, logging to ~/.openbox/<lane>.log")
	fs.BoolVar(&forceRestore, "force-restore", false, "during removal, restore env keys even where the value changed after OpenBox set it (the conflict is named either way)")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan; make no network or filesystem writes")
	var role string
	fs.StringVar(&role, "role", "dev", "dev (default) or approver; an approver is a queue client, not a governed runtime")

	fs.StringVar(&movedOrg, "org", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedAgentName, "agent-name", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedIcon, "icon", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedDescription, "description", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedBaseURL, "base-url", "", "MOVED to `openbox auth`")
	fs.StringVar(&movedBackendURL, "backend-url", "", "MOVED to `openbox auth`")
	fs.BoolVar(&movedForce, "force", false, "MOVED to `openbox auth`")

	fs.StringVar(&goneSecretBackend, "secret-backend", "", "REMOVED; credentials live in ~/.openbox/.env; run `openbox auth`")
	fs.StringVar(&goneClientID, "client-id", "", "REMOVED; `init` makes no control-plane call")
	fs.BoolVar(&goneManagedEnable, "managed-enable", false, "REMOVED; recorded a Phase-1 substrate nothing read")
	fs.StringVar(&goneLocalHooks, "local-hooks", "", "DEPRECATED; use --scope local (accepted for one release)")

	fs.Usage = a.initUsage(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	for _, m := range []struct{ flag, value string }{
		{"--org", movedOrg}, {"--agent-name", movedAgentName}, {"--icon", movedIcon},
		{"--description", movedDescription}, {"--base-url", movedBaseURL}, {"--backend-url", movedBackendURL},
	} {
		if m.value != "" {
			return a.errorf("%s moved to `openbox auth`; `init` no longer registers agents or touches credentials.\n"+
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
			" Credentials live in ~/.openbox/.env (plaintext, 0600; see.\n" +
			"  Write them with `openbox auth`, then re-run `openbox init` without this flag.")
	}
	if goneClientID != "" {
		return a.errorf("--client-id was removed: `init` makes no control-plane call, so there is no header to set.\n" +
			"  Registration moved to `openbox auth`, which sets it itself.")
	}
	if goneManagedEnable {
		return a.errorf("--managed-enable was removed. It recorded a Phase-1 force-enable substrate in the agent's\n" +
			"  backend config that nothing ever read. Org-wide activation is a managed-settings\n" +
			"  deployment; see `openbox managed install` and deployments/managed/.")
	}

	if o.Provider == "" {
		return a.errorf("--provider is required (one of: claude-code, codex, cursor)")
	}

	enforceGiven := flagPassed(fs, "enforce")
	switch {
	case noEnforce && enforceGiven && enforce:
		return a.errorf("--enforce and --no-enforce are mutually exclusive")
	case noEnforce:
		f := false
		o.Enforce, o.Findings = &f, &f
	case enforceGiven:
		v := enforce // honours --enforce=false as well as --enforce
		o.Enforce, o.Findings = &v, &v
	}

	// Under enforce-by- default this compares resolved postures, so it still
	// fires for a config that never wrote the field.
	if devconfig.WouldDowngradeEnforce(devconfig.DefaultConfigPath(), o.Enforce) {
		fmt.Fprintln(a.stdout, "note: turning ENFORCE off; this machine was enforcing (explicitly, or by default).")
	}

	if withGateway && removeGateway {
		return a.errorf("--gateway and --remove-gateway are mutually exclusive")
	}
	if withFull {
		withTelemetry, withTransport = true, true
	}
	for _, pair := range []struct {
		on, off   bool
		onN, offN string
	}{
		{withTelemetry, removeTelemetryLane, "--telemetry", "--remove-telemetry"},
		{withTransport, removeTransportLane, "--transport", "--remove-transport"},
		{withFull, removeAll, "--full", "--remove-all"},
	} {
		if pair.on && pair.off {
			return a.errorf("%s and %s are mutually exclusive", pair.onN, pair.offN)
		}
	}

	laneFlags := []struct {
		name string
		on   bool
	}{
		{"--full", withFull}, {"--remove-all", removeAll},
		{"--telemetry", withTelemetry}, {"--remove-telemetry", removeTelemetryLane},
		{"--transport", withTransport}, {"--remove-transport", removeTransportLane},
	}
	for _, f := range laneFlags {
		if f.on && o.Provider != "claude-code" {
			return a.errorf("%s applies to --provider claude-code only (got %q); these lanes observe the Anthropic Messages API and are configured through Claude Code's own settings", f.name, o.Provider)
		}
	}

	if (withGateway || removeGateway) && o.Provider != "claude-code" {
		flag := "--gateway"
		if removeGateway {
			flag = "--remove-gateway"
		}
		return a.errorf("%s applies to --provider claude-code only (got %q); the gateway relays the Anthropic Messages API and is configured through Claude Code's own settings", flag, o.Provider)
	}

	inst, err := providers.Lookup(o.Provider)
	if err != nil {
		return a.errorf("%v", err)
	}

	if goneLocalHooks != "" {
		if scope != "" {
			return a.errorf("--local-hooks and --scope cannot both be given; --local-hooks is the deprecated spelling of --scope local")
		}
		fmt.Fprintf(a.stderr, "warning: --local-hooks is deprecated; use --scope local (this release still accepts it).\n"+
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

	if o.DryRun {
		if _, err := devinit.Run(context.Background(), o, d); err != nil {
			return a.errorf("%v", err)
		}
		a.printGovernedScope(o, resolvedScope)
		a.printGatewayPlan(withGateway, removeGateway, gatewayAddr, gatewayUpstream)
		a.printLanePlan(lanePlan{
			telemetry: withTelemetry, transport: withTransport,
			removeTelemetry: removeTelemetryLane || removeAll,
			removeTransport: removeTransportLane || removeAll,
			purge:           removeAll,
			telemetryAddr:   telemetryAddr, transportAddr: transportAddr,
		})
		return exitOK
	}

	if removeGateway || removeAll || removeTelemetryLane || removeTransportLane {
		home, code := a.gatewayHome()
		if code != exitOK {
			return code
		}
		return a.runRemovals(home, removalRequest{
			gateway:   removeGateway || removeAll,
			telemetry: removeTelemetryLane || removeAll,
			transport: removeTransportLane || removeAll,
			purge:     removeAll,
			force:     forceRestore,
		})
	}

	a.migrateLegacyConfig()
	if code := a.requireCredentials(); code != exitOK {
		return code
	}

	res, runErr := devinit.Run(context.Background(), o, d)
	if runErr != nil && res != nil && res.ConfigManualOnly {
		fmt.Fprintln(a.stderr, "note: "+runErr.Error())
		return exitConfigOnly
	}
	if runErr != nil {
		return a.errorf("%v", runErr)
	}

	if o.Provider == "codex" && res != nil && res.ConfigApplied {
		fmt.Fprintln(a.stdout, "Next step: open Codex and run /hooks to review and TRUST the new OpenBox hooks; they do not run until trusted (Codex hash-trusts non-managed hooks; re-running init re-hashes them).")
	}

	gatewayRunning := false
	if withGateway {
		fmt.Fprintf(a.stdout, "\nLocal gateway (model-call governance)\n")
		home, code := a.gatewayHome()
		if code != exitOK {
			return code
		}
		gatewayRunning = true
		if err := a.setupGateway(home, gatewayAddr, gatewayUpstream, gatewayVerbose); err != nil {
			gatewayRunning = false
			fmt.Fprintf(a.stderr, "warning: gateway setup did not complete: %v\n", err)
		}
	}

	laneReport := a.setupLanes(laneRequest{
		telemetry:     withTelemetry,
		transport:     withTransport,
		telemetryAddr: telemetryAddr,
		transportAddr: transportAddr,
		verbose:       laneVerbose,
	})

	switch {
	case withGateway && gatewayRunning:
		fmt.Fprintf(a.stdout, "\nDone. A supervised gateway is running and this machine's model calls now route through it.\n")
	case withGateway:
		fmt.Fprintf(a.stdout, "\nDone for the hooks; they are in place and governing tool calls. The gateway did NOT come up; see the warning above, and run `openbox doctor` for where this machine's model calls are pointed.\n")
	default:
		fmt.Fprintf(a.stdout, "\nDone. Nothing to run and no environment to keep set; the hooks do the rest.\n")
	}
	fmt.Fprintf(a.stdout, "  openbox dev verify     confirm this machine can reach and authenticate to OpenBox\n")
	fmt.Fprintf(a.stdout, "  openbox doctor         the effective posture, and where each value came from\n")
	if o.Enforce != nil && !*o.Enforce {
		fmt.Fprintf(a.stdout, "  mode: OBSERVE; telemetry and lineage only, by your explicit --enforce=false.\n")
	} else {
		fmt.Fprintf(a.stdout, "  mode: ENFORCE; tool calls are gated in-process. Inert until your org publishes a\n")
		fmt.Fprintf(a.stdout, "        policy, and fail-open, so an OpenBox outage never blocks you. `--enforce=false` opts out.\n")
	}
	a.printGovernedScope(o, resolvedScope)
	laneReport.print(a)
	return exitOK
}

func (a *app) env(key, def string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return def
}

func (a *app) usage() {
	fmt.Fprint(a.stderr, `openbox; OpenBox developer-runtime governance CLI

Setup is two commands, in this order:
  openbox auth                                        credentials for this machine
  openbox init --provider <claude-code|codex|cursor>  install hooks + posture

Usage:
  openbox auth [--rotate] [flags]
  openbox init --provider <claude-code|codex|cursor> [--scope local|global] [flags]
  openbox init --provider claude-code --full          hooks + telemetry + transport
  openbox init --provider claude-code --remove-all    every lane, restored and deleted
  openbox init --role approver --org <id> [--host claude-code] [flags]
  openbox dev verify [--dry-run]
  openbox managed install --provider <claude-code,codex> [--dry-run] [--force]
  openbox approve list [--org <id>] [--watch]
  openbox approve <allow|deny> <event-id> [--org <id>]
  openbox approve --watch --auto [--host claude-code] [--decide]
  openbox gateway [--addr <loopback host:port>] [--upstream <provider base URL>]
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

Credentials live in ~/.openbox/.env (plaintext, 0600); posture and
coordinates in ~/.openbox/dev.json. OPENBOX_HOME relocates both. A real
environment variable always wins over either file.

Nothing to run after 'init'; no daemon, no runtime env. Coverage is a separate
question from mechanism: a bare 'init' governs ONE directory (see --scope).
Run 'openbox auth -h' or 'openbox init -h' for flags.
`)
}
