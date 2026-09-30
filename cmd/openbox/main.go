// Command openbox is the developer-runtime governance CLI.
package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"

	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

var version = "0.1.0-dev"

// resolveTraceDir is trace.SetDir's argument, computed exactly once at
// process start: the env var wins (a daemon carries it because it has no
// $HOME; see laneUnitEnv), else the same per-machine config directory every
// other coordinate resolves under.
func resolveTraceDir(getenv func(string) string) string {
	if dir := getenv(trace.EnvDir); dir != "" {
		return dir
	}
	return filepath.Join(devconfig.ConfigDir(), "trace")
}

const (
	exitOK    = 0
	exitError = 1
)

type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	// getenv is for reporting which variables are set -- the shadow warnings,
	// the attestation markers, doctor's "in effect" line. Credential and token
	// RESOLUTION deliberately does not go through it: those read the real
	// environment through devconfig, because that is what the runtime reads,
	// and a gate asking a different source than the resolver is how an install
	// ends up accepting an identity the resolver cannot find.
	getenv       func(string) string
	newRegistrar func(baseURL, credential, clientID string) devinit.Registrar
	// newPrompt is the seam `auth` reaches the terminal through. One seam, not
	// two: the terminal REQUIREMENT and the prompter are the same decision, and
	// a test that could supply answers but not get past RequireTerminal could
	// not drive the command at all.
	newPrompt func() (prompt.Prompter, error)
	// newWorkloadKey generates the RSA key a fresh v3 registration proves
	// possession of; nil in production, which devinit.Deps.GenerateKey reads
	// as "use workloadauth.GenerateKey". The seam exists for a test that must
	// prove the registered key is the one a hook run afterward actually
	// signs with (e.g. against a fake core with its own fixed identity).
	newWorkloadKey func() (*rsa.PrivateKey, error)

	gatewayReady func(net.Addr)
	gatewayCtx   context.Context

	telemetryReady func(addr string)
	telemetryCtx   context.Context

	transportReady func(addr string)
	transportCtx   context.Context

	// lastSystemPACOutcome is setupTransport's own out-of-band return for its
	// system PAC activation step (systempac.go): written by its activate
	// closure, read immediately after by setupLanes. A dedicated field rather
	// than widening setupTransport's own (int, error) signature, which ~20
	// call sites across this package's tests share unchanged.
	lastSystemPACOutcome activation.Outcome
}

func defaultApp() *app {
	return &app{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		stdin:        os.Stdin,
		getenv:       os.Getenv,
		newRegistrar: func(u, c, id string) devinit.Registrar { return backend.New(u, c, id) },
		// A struct-literal closure cannot reference `a`, so it names the same
		// concrete files assigned above.
		newPrompt: func() (prompt.Prompter, error) {
			if err := prompt.RequireTerminal(os.Stdin); err != nil {
				return nil, err
			}
			return prompt.New(os.Stdin, os.Stdout), nil
		},
	}
}

func main() {
	trace.SetDir(resolveTraceDir(os.Getenv))
	client.SetTokenObserver(traceTokenAcquisition)
	os.Exit(defaultApp().run(os.Args[1:]))
}

func (a *app) errorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "error: "+format+"\n", args...)
	return exitError
}

// run is every command's one entry point, and the one place a proc.start /
// proc.exit pair can be emitted for ALL of them, hook included: it stamps
// argv (already secret-free -- the control token is an env var, never a
// flag, per README), version and os/arch on the way in, and the exit code
// and wall-clock duration on the way out, however dispatch returns.
func (a *app) run(args []string) (code int) {
	if len(args) == 0 {
		a.usage()
		return exitError
	}
	trace.SetProcess(args[0])
	start := time.Now()
	trace.Emit(trace.Record{
		Stage: trace.StageProcStart,
		Detail: map[string]any{
			"argv":    args,
			"version": version,
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		},
	})
	defer func() {
		trace.Emit(trace.Record{
			Stage: trace.StageProcExit,
			DurMS: float64(time.Since(start)) / float64(time.Millisecond),
			Detail: map[string]any{
				"exit_code": code,
			},
		})
	}()
	code = a.dispatch(args)
	return code
}

func (a *app) dispatch(args []string) int {
	switch args[0] {
	case "auth":
		return a.runAuth(args[1:])
	case "init":
		return a.runDevInit(args[1:])
	case "hook":
		return a.runHook(args[1:])
	case "rewake":
		return a.runRewake(args[1:])
	case "doctor":
		return a.runDoctor(args[1:])
	case "uninstall":
		return a.runUninstall(args[1:])
	case "trace":
		return a.runTrace(args[1:])
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

// hookEngineFn resolves a provider's hook engine; a seam so a test can run
// runHook's own fault handling against an engine that panics.
var hookEngineFn = providers.Engine

func (a *app) runHook(args []string) (code int) {
	code = exitOK
	// faultCode is what this run exits with if it cannot produce an answer:
	// 0 unless the engine's host reads exit 0 as allow (FaultExiter).
	faultCode := func() int { return exitOK }
	// Report it on stderr, which the tool shows as a diagnostic and never parses
	// as hook output.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(a.stderr, "openbox hook: recovered from panic: %v\n", r)
			code = faultCode()
		}
	}()
	// First, and before the logger below: the trace it tees into lives under the
	// OpenBox home, and the deny-only successor must work, and touch nothing,
	// on a machine where that home, the config and the identity are all broken.
	if code, handled := a.tryFailClosed(args); handled {
		return code
	}
	// Teed into the trace: a hook exiting 0 has its stderr discarded by the
	// tool, so an early return (no identity, unknown provider) would
	// otherwise leave no reason anywhere.
	logger := tracedLogger(a.stderr, "openbox hook: ", 0)
	if len(args) < 1 {
		logger.Printf("usage: openbox hook <provider> <event...>  (providers: %s, git)", strings.Join(provider.Supported(), ", "))
		return exitOK
	}
	if args[0] == "git" {
		// A commit hook has no provider on argv, so the tool is read from the
		// markers it leaves in the environment. newCommitSink resolves and binds
		// that identity per resolved session (routeCommitTool, commitevent.go), so
		// there is no ambient bind here: a hand commit (no marker) never binds any
		// identity at all.
		obgit.SetCommitSink(newCommitSink(a.getenv, logger))
		obgit.RunHook(args[1:], []string{"hook", "git", "prepare-commit-msg"}, logger.Printf)
		return exitOK
	}

	engine, err := hookEngineFn(args[0])
	if err != nil {
		logger.Printf("unknown hook provider %q (supported: %s, git)", args[0], strings.Join(provider.Supported(), ", "))
		return exitOK
	}
	name, rest := args[0], args[1:]
	home, rest, homeErr := splitHookHome(rest)
	if len(rest) < 1 {
		logger.Printf("usage: openbox hook %s [--home <abs dir>] <event>", name)
		return exitOK
	}
	event := rest[0]
	if fe, ok := engine.(provider.FaultExiter); ok {
		faultCode = func() int { return fe.FaultExitCode(event) }
	}
	if homeErr != nil {
		logger.Printf("%v", homeErr)
		return faultCode()
	}
	if home != "" {
		// A host that clears the hook's environment cannot deliver
		// OPENBOX_HOME, so the installer bakes it into argv instead; set
		// before BindProvider so the per-tool store resolves under it.
		if err := os.Setenv(devconfig.EnvHome, home); err != nil {
			logger.Printf("cannot set %s: %v", devconfig.EnvHome, err)
			return faultCode()
		}
	}
	// After the name is known to be a real provider, so a typo cannot create a
	// directory under ~/.openbox.
	release, err := devconfig.BindProvider(name)
	if err != nil {
		logger.Printf("cannot resolve %s's identity store: %v", name, err)
		return faultCode()
	}
	defer release()
	a.warnIfUnconfigured(name, logger)
	engine.RunHook(event, a.stdin, a.stdout, logger)
	return exitOK
}

// tryFailClosed serves `openbox hook <provider> [--home <dir>] --fail-closed
// <event>`, the deny-only successor a host that fails open runs when a gate
// handler fails. It needs no config, identity or network and writes nothing
// under the OpenBox home, so it binds no provider store and ignores --home:
// the directory is only for a handler that has a store to find. Only a provider
// whose engine implements FailClosedRunner has this form; for any other the
// arguments fall through to the ordinary hook route, unchanged.
func (a *app) tryFailClosed(args []string) (code int, handled bool) {
	if len(args) < 2 || args[0] == "git" {
		return 0, false
	}
	engine, err := hookEngineFn(args[0])
	if err != nil {
		return 0, false
	}
	fc, ok := engine.(provider.FailClosedRunner)
	if !ok {
		return 0, false
	}
	rest := args[1:]
	if len(rest) >= 2 && rest[0] == "--home" {
		rest = rest[2:]
	}
	if len(rest) == 0 || rest[0] != "--fail-closed" {
		return 0, false
	}
	event := ""
	if len(rest) > 1 {
		event = rest[1]
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(a.stderr, "openbox hook: recovered from panic in the fail-closed handler: %v\n", r)
			code, handled = exitError, true
			if fe, ok := engine.(provider.FaultExiter); ok && fe.FaultExitCode(event) != 0 {
				code = fe.FaultExitCode(event)
			}
		}
	}()
	return fc.RunFailClosed(event, a.stderr), true
}

// splitHookHome takes an optional `--home <dir>` off the front of a hook's
// arguments. The directory must be absolute: a relative one would resolve
// against whatever cwd the host happened to run the hook in.
func splitHookHome(args []string) (home string, rest []string, err error) {
	if len(args) == 0 || args[0] != "--home" {
		return "", args, nil
	}
	if len(args) < 2 {
		return "", nil, errors.New("--home needs a directory")
	}
	if !filepath.IsAbs(args[1]) {
		return "", args[2:], fmt.Errorf("--home %q is not an absolute path", args[1])
	}
	return filepath.Clean(args[1]), args[2:], nil
}

// warnIfUnconfigured names the fix once when the bound tool has no identity at
// all. Under the no-legacy-fallback rule an upgraded machine governs nothing
// until `init` runs again per tool, and the symptom is silence -- so the line
// exists to be the answer to "why did governance stop".
//
// It never blocks: a hook that failed closed on a missing credential would
// stop the developer working, which is a worse failure than not governing.
// And it is a developer-facing notice, never an event; nothing here reaches
// the spool or the enforcement sink.
func (a *app) warnIfUnconfigured(name string, logger *log.Logger) {
	did, err := devconfig.ResolveDID()
	switch {
	case errors.Is(err, devconfig.ErrLegacyStore):
		logger.Printf("%s has a legacy (pre-IAMv3) identity; hooks send nothing. Run `openbox init --provider %s`", name, name)
		return
	case did == "":
		logger.Printf("no agent identity for %s on this machine; governance is inactive. Run `openbox init --provider %s`", name, name)
		return
	}
	// An agent id alone is not an identity. A half-written store, a restored
	// backup or a hand-deleted credential file all leave it behind, and every
	// gated call then fails to resolve and fails open -- silently, which is the
	// state this line exists to explain. Stat, never parse: the hot path does
	// zero secret I/O (INV-1), and existence is the whole question.
	if devconfig.EnvIdentityPresent() {
		return // exported credentials answer for every tool; there is no file to miss
	}
	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		logger.Printf("cannot resolve %s's credential file: %v; governance is inactive", name, err)
		return
	}
	if _, err := os.Stat(envPath); err != nil {
		logger.Printf("%s has an agent id but no credential file at %s; governance is inactive. Run `openbox init --provider %s`",
			name, envPath, name)
	}
}

// runRewake is the background approval watcher, invoked by an
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
	// Before RunRewake, which takes a config Pin as its first act: a Pin
	// freezes the resolved config across a path change, so a bind inside one
	// would be read through the previous tool's snapshot.
	release, err := devconfig.BindProvider(args[0])
	if err != nil {
		fmt.Fprintf(a.stderr, "openbox rewake: cannot resolve %s's identity store: %v\n", args[0], err)
		return exitOK
	}
	defer release()
	rw, ok := engine.(provider.Rewaker)
	if !ok {
		return exitOK
	}
	return rw.RunRewake(a.stdin, a.stderr, log.New(a.stderr, "openbox rewake: ", 0))
}

func (a *app) runDevInit(args []string) int {
	fs := a.newFlagSet("openbox init")
	var o devinit.Options
	// One flag. Everything an install used to be able to vary is now fixed
	// once, for every machine: full for the provider,
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
	// Before anything is written or registered: a refusal here leaves no agent
	// behind that nothing can be installed for.
	if pf, ok := inst.(provider.Preflighter); ok {
		warning, err := pf.Preflight()
		if err != nil {
			return a.errorf("%v", err)
		}
		if warning != "" {
			fmt.Fprintf(a.stderr, "warning: %s\n", warning)
		}
	}

	// The commit-trailer hook is on. This is the one posture field init writes
	// unconditionally, so OPENBOX_INSTALL_GIT_HOOK=false is its only opt-out --
	// and the write is what makes that env var the whole story rather than one
	// of two places to look.
	o.InstallGitHook = true
	// Enforcement is always on now (ResolveEnforce), so `init` has no enforce
	// flag or field left to set at all.
	// ProjectDir stays empty: the install is user-wide and the adapter sweeps
	// the working directory itself.

	a.migrateLegacyConfig()

	// Read the org config BEFORE the bind, and not a line later: after it this
	// resolves the tool's own config, which on a first install is absent. These
	// two URLs are the organization's, `auth` writes them once, and `init`
	// carries them into each tool's config -- a per-tool store does not inherit
	// them, because one file is loaded, not merged over another.
	orgCfg, _ := devconfig.Load(devconfig.DefaultConfigPath())
	o.BackendURL, o.BaseURL = orgCfg.BackendURL, orgCfg.BaseURL

	// After the migration, which is org-level by construction, and before
	// anything reads a credential.
	release, err := devconfig.BindProvider(o.Provider)
	if err != nil {
		return a.errorf("cannot resolve %s's identity store: %v", o.Provider, err)
	}
	defer release()
	plan, code := a.requireCredentials()
	if code != exitOK {
		return code
	}

	d := devinit.Deps{Installer: inst, Out: a.stdout, GenerateKey: a.newWorkloadKey, DiscardLegacySpool: discardLegacySpool}
	var tokenFrom string
	if !plan.reuse {
		// Adopt first, and before the token check: pasting an agent's own key
		// needs no organization credential, and requiring one locked out the
		// developer whose org key lives with an administrator.
		adopted, code := a.adoptExistingAgent(o.Provider)
		if code != exitOK {
			return code
		}
		if adopted {
			// adopt just wrote this tool's own v3 store (agent id, API key,
			// workload key), which devinit's ordinary reuse check already
			// recognizes as complete; no separate signal is needed to skip
			// registration.
		} else {
			token, tokenSource, code := a.requireControlToken()
			if code != exitOK {
				return code
			}
			tokenFrom = tokenSource
			// Wired on this branch only. A registrar present on the reuse path
			// would make an offline re-run one refactor away from a network call,
			// and the reuse path is the one that has to work on a plane.
			//
			// The environment outranks the org config for the backend URL at
			// runtime, so it has to here too: minting the agent in one deployment
			// and posting its events to another surfaces later as a 401 and never
			// as a message about URLs.
			d.Registrar = a.newRegistrar(firstNonEmptyStr(
				a.getenv(devconfig.EnvBackendURL), o.BackendURL, devconfig.DefaultBackendURL),
				token, "openbox-cli")
		}
	}
	res, runErr := devinit.Run(context.Background(), o, d)
	if runErr != nil {
		// Naming the credential's source belongs here rather than in devinit,
		// which never resolved it. It matters on exactly one failure: the
		// backend refusing the token. The environment outranks the org file, so
		// a stale export in a long-lived shell beats every `openbox auth` and
		// every hand edit -- and a refusal that names only the backend sends its
		// reader to change the file again, which cannot work.
		var apiErr *backend.APIError
		if tokenFrom != "" && errors.As(runErr, &apiErr) &&
			(apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			return a.errorf("%v\n  The token it sent came from %s.", runErr, tokenFrom)
		}
		return a.errorf("%v", runErr)
	}

	if o.Provider == "codex" && res != nil && res.ConfigApplied {
		fmt.Fprintln(a.stdout, "Next step: open Codex and run /hooks to review and TRUST the new OpenBox hooks; they do not run until trusted (Codex hash-trusts non-managed hooks; re-running init re-hashes them).")
	}

	// Full means every lane the provider supports, which is a derivation rather
	// than a choice. Codex gets the telemetry lane, reading its own
	// config.toml, and on macOS a proxy arm: the relay plus the system PAC
	// that routes Codex to it. An unsupported provider gets hooks alone and is told why: erroring
	// because a provider cannot have a lane it never asked for would be a
	// regression.
	var laneReport laneReport
	if laneCapable(o.Provider) {
		// Before the transport unit is (re)installed: a legacy constrained CA
		// re-issued AFTER the unit started would leave a daemon serving a
		// certificate no longer on disk. Both providers with a transport arm
		// use this CA.
		if hasTransportArm(provider.Name(o.Provider)) {
			if err := a.reissueLegacyCAIfNeeded(); err != nil {
				fmt.Fprintf(a.stderr, "warning: could not re-issue the legacy CA: %v\n", err)
			}
		}
		laneReport = a.setupLanes(laneRequest{
			telemetry: true,
			// Claude Code routes through the relay by its env block; Codex, on
			// macOS, by the system PAC the relay serves.
			transport:     hasTransportArm(provider.Name(o.Provider)),
			telemetryAddr: telemetry.DefaultAddr,
			transportAddr: transport.DefaultAddr,
			provider:      o.Provider,
		})
	} else {
		a.row("lanes", "hooks only for %s; no model-call lane reads this provider's own", o.Provider)
		a.row("", "settings yet")
	}
	laneReport.print(a)

	// Every posture, and the scope they apply to, in one block. The opt-out
	// environment variables live in `openbox init --help`, which is where
	// somebody looking for them goes; repeating them on every success turned
	// three facts into three paragraphs.
	a.printGovernedScope(o)

	fmt.Fprintf(a.stdout, "\nDone. `openbox doctor` explains every value above, and where it came from.\n")
	return exitOK
}

// discardLegacySpool implements devinit.Deps.DiscardLegacySpool: the
// named provider's own spool directory, discarded under the spool lock and
// recorded in its discard ledger. register() calls this only after a
// successful WriteWorkloadIdentity, and only for a tool whose store was
// legacy before that write -- never for a fresh or already-v3 store.
func discardLegacySpool(tool string) (int, error) {
	dir := providers.SpoolDirFor(tool)
	if dir == "" {
		return 0, nil
	}
	return hookflow.Spool{Dir: dir}.DiscardAll("queued under the previous identity")
}

func (a *app) usage() {
	fmt.Fprint(a.stderr, `openbox; OpenBox developer-runtime governance CLI

Setup is two commands, in this order:
  openbox auth                                 connect your organization
  openbox init --provider <claude-code|codex>  register that tool's agent and
                                               install hooks, lanes and posture
                                               (run it once per tool)

Usage:
  openbox auth
  openbox init --provider <claude-code|codex>
  openbox doctor
  openbox uninstall
  openbox trace <session-id|run-id> | --list | --against-core --agent <id>
  openbox version

One install governs EVERY session on this machine, in any directory, and takes
effect immediately: the tool watches its settings file, so sessions already
running are governed too. It installs every model-call lane the provider
supports, enforces, and turns on commit trailers. There are no flags to choose
between, because there is one right answer for each of those.

Each governed tool carries its OWN agent identity, in its own
~/.openbox/<tool>/ store, so run 'init' once for each tool you use. A tool that
already has an agent is reused offline, with no control-plane call at all.

Two postures stay per-machine, as environment variables rather than flags:
  OPENBOX_ENFORCE=false            observe only, for this run; nothing persists
  OPENBOX_INSTALL_GIT_HOOK=false   do not touch any repo's .git/hooks

Environment (needed at 'init' time, to register a tool's agent; 'auth' can
store it for you instead):
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key).
                          Never a flag, so it cannot leak via argv or shell history.
                          It can create and rotate agents across your whole
                          organization: 'auth' persists it in plaintext to
                          ~/.openbox/.env, so export it for one run instead if
                          you would rather it never touch the disk.
  OPENBOX_BACKEND_URL     openbox-backend CONTROL-PLANE base URL
  OPENBOX_BASE_URL        openbox-core DATA-PLANE base URL. Self-hosted? Set BOTH: the
                          control plane cannot tell the CLI where your core is, so one
                          default and one override sends events to the hosted core and
                          surfaces later as a 401.

Each tool's credentials live in ~/.openbox/<tool>/.env (plaintext, 0600), with
its posture and coordinates in ~/.openbox/<tool>/dev.json. The org-level
~/.openbox/.env holds the control token and ~/.openbox/dev.json the
organization's URLs. OPENBOX_HOME relocates all of it. A real environment
variable always wins over every file, for every tool at once.

'openbox uninstall' reverses all of it, including the credentials.
'openbox doctor' reports the effective posture and where each value came from.
`)
}
