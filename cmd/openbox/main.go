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
	case "uninstall":
		return a.runUninstall(args[1:])
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
	// One flag. Everything an install used to be able to vary is now a decision
	// the owner already made once, for every machine: full for the provider,
	// user-wide, enforcing, with the commit-trailer hook on. A knob that only
	// ever has one correct setting is a way to get it wrong.
	fs.StringVar(&o.Provider, "provider", "",
		"developer tool: "+strings.Join(provider.Supported(), "|")+" (required)")
	fs.Usage = a.initUsage(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if o.Provider == "" {
		return a.errorf("--provider is required (one of: %s)", strings.Join(provider.Supported(), ", "))
	}
	inst, err := providers.Lookup(o.Provider)
	if err != nil {
		return a.errorf("%v", err)
	}

	// The commit-trailer hook is on. This is the one posture field init writes
	// unconditionally, so OPENBOX_INSTALL_GIT_HOOK=false is its only opt-out --
	// and the write is what makes that env var the whole story rather than one
	// of two places to look.
	o.InstallGitHook = true
	// o.Enforce is left nil, deliberately and permanently. A bool that defaults
	// to true cannot express "said nothing", so assigning it here would write an
	// enforce key indistinguishable from a machine that had explicitly opted in
	// -- and would silently revert anybody who had opted out. Nil resolves to
	// true through ResolveEnforce; OPENBOX_ENFORCE is the escape hatch.
	// ProjectDir stays empty: the install is user-wide and the adapter sweeps
	// the working directory itself.

	a.migrateLegacyConfig()
	if code := a.requireCredentials(); code != exitOK {
		return code
	}

	d := devinit.Deps{Installer: inst, Out: a.stdout}
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

	// Full means every lane the provider supports, which is a derivation rather
	// than a choice. Codex gets hooks alone and is told why: erroring because a
	// provider cannot have a lane it never asked for would be a regression.
	var laneReport laneReport
	if laneCapable(o.Provider) {
		laneReport = a.setupLanes(laneRequest{
			telemetry:     true,
			transport:     true,
			telemetryAddr: telemetry.DefaultAddr,
			transportAddr: transport.DefaultAddr,
		})
	} else {
		fmt.Fprintf(a.stdout, "\nModel-call lanes: hooks only for %s.\n", o.Provider)
		fmt.Fprintf(a.stdout, "  The telemetry receiver and the transport relay observe the Anthropic Messages\n")
		fmt.Fprintf(a.stdout, "  API through Claude Code's own settings, so there is nothing for them to read\n")
		fmt.Fprintf(a.stdout, "  here. Tool calls are still governed by the hooks above.\n")
	}

	fmt.Fprintf(a.stdout, "\nDone.\n")
	fmt.Fprintf(a.stdout, "  openbox doctor         the effective posture, and where each value came from\n")
	fmt.Fprintf(a.stdout, "  mode: ENFORCE; tool calls are gated in-process. Inert until your org publishes a\n")
	fmt.Fprintf(a.stdout, "        policy, and fail-open, so an OpenBox outage never blocks you.\n")
	fmt.Fprintf(a.stdout, "        OPENBOX_ENFORCE=false opts out, per run; nothing is persisted either way.\n")
	fmt.Fprintf(a.stdout, "  commit trailers: ON. A session installs prepare-commit-msg and post-commit into\n")
	fmt.Fprintf(a.stdout, "        the repo it runs in, so a commit is attributed to the session that made it.\n")
	fmt.Fprintf(a.stdout, "        A hook somebody else wrote is never overwritten. OPENBOX_INSTALL_GIT_HOOK=false\n")
	fmt.Fprintf(a.stdout, "        turns it off.\n")
	a.printGovernedScope(o)
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
  openbox auth                                 credentials for this machine
  openbox init --provider <claude-code|codex>  install hooks, lanes and posture

Usage:
  openbox auth
  openbox init --provider <claude-code|codex>
  openbox doctor
  openbox uninstall
  openbox version

One install governs EVERY session on this machine, in any directory, and takes
effect immediately: the tool watches its settings file, so sessions already
running are governed too. It installs every model-call lane the provider
supports, enforces, and turns on commit trailers. There are no flags to choose
between, because there is one right answer for each of those.

Two postures stay per-machine, as environment variables rather than flags:
  OPENBOX_ENFORCE=false            observe only, for this run; nothing persists
  OPENBOX_INSTALL_GIT_HOOK=false   do not touch any repo's .git/hooks

Environment (needed only at 'auth' time, and only to register a new agent):
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key).
                          Never a flag, so it cannot leak via argv or shell history.
  OPENBOX_BACKEND_URL     openbox-backend CONTROL-PLANE base URL
  OPENBOX_BASE_URL        openbox-core DATA-PLANE base URL. Self-hosted? Set BOTH: the
                          control plane cannot tell the CLI where your core is, so one
                          default and one override sends events to the hosted core and
                          surfaces later as a 401.
  OPENBOX_ORG             organization namespace, used to derive the agent name

Credentials live in ~/.openbox/.env (plaintext, 0600); posture and coordinates
in ~/.openbox/dev.json. OPENBOX_HOME relocates both. A real environment
variable always wins over either file.

'openbox uninstall' reverses all of it, including the credentials.
'openbox doctor' reports the effective posture and where each value came from.
`)
}
