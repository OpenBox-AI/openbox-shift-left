package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// initgateway.go is `init`'s gateway step (phase 07 requirement 1).
//
// ── THE ORDER IS A SAFETY PROPERTY, NOT A STYLE ───────────────────────────────
//
// unit -> start -> PROVE it listens -> only then write ANTHROPIC_BASE_URL.
//
// Writing the env var first would point Claude Code at a port nothing answers,
// and because a dead localhost fails closed, EVERY model call on the machine
// would fail. `init` would have broken the developer's tool while reporting
// success. So the env write is last and conditional, and if the daemon cannot be
// proven up, the variable is not written at all — the machine is left exactly as
// it was, ungoverned for model calls and working.
//
// The reverse order on UNINSTALL, for the same reason: env var first, then the
// daemon, so there is never a window where the tool points at something gone.
//
// ── WHY --gateway IS OPT-IN (an OD-class call, flagged not buried) ────────────
//
// ADR-0016's lesson is that a default-off headline feature stays off, and that
// argues for defaulting this ON. It is deliberately off anyway, because the two
// cases are not alike: enforcement-by-default is INERT without an org policy, so
// flipping it could not break anyone. This redirects live model traffic through a
// process that has never run against a real stack, whose refusal shape is still
// unprobed, and which has no daemon packaging on Windows at all. A default that
// can break the developer's tool before the path is proven end to end is the
// opposite of the caution this repo applies everywhere else.
//
// Revisit the default when phase 08 has run the end-to-end path. That is the
// owner's call, and it should be made with evidence rather than by symmetry with
// ADR-0016.

// gatewayReadyTimeout bounds how long init waits for the supervisor to bring the
// daemon up before deciding it did not.
const gatewayReadyTimeout = 10 * time.Second

// setupGateway installs, starts and verifies the gateway, then points the tool at
// it. Any failure leaves the machine unconfigured rather than half-configured.
func (a *app) setupGateway(homeDir, addr, upstream string) error {
	cfg := gateway.Config{Addr: addr, Upstream: upstream}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Is the port already taken by something else? This has to be checked BEFORE
	// starting, because the readiness probe below cannot tell our gateway from a
	// stranger: it is a bare TCP connect. Without this, a foreign process holding
	// the port means our gateway fails to bind, the probe connects to the stranger
	// anyway, and init writes ANTHROPIC_BASE_URL pointing the developer's model
	// traffic at an unknown local service while reporting success.
	//
	// A pre-check rather than a post-hoc identity check because the relay has no
	// identity to assert — it is a transparent proxy by design, so there is no
	// endpoint that could answer "are you OpenBox?" without breaking that.
	// OUR OWN daemon holding the port is a RE-INSTALL, not a conflict.
	//
	// The pre-check could not tell the two apart, so the second `init --gateway` on
	// any gateway-enabled machine refused before WriteUnit ever ran — which meant a
	// unit written by an older binary could never be refreshed. That is the exact
	// stale-path failure `init` already repairs for hook registrations, and the
	// remedy the error text recommends ("re-run init") was the thing that could not
	// work. A moved binary (an upgrade, a reinstall) then left launchd restarting a
	// path that no longer exists, with no CLI fix.
	//
	// Ownership is decided by OUR UNIT, not by probing the socket: if the unit we
	// wrote names this same address, whatever answers there is ours to replace. A
	// port held by anything else is still refused — over-refuse, never
	// over-terminate.
	if occupied, who := portOccupied(cfg.Addr); occupied {
		unitPath := gatewayservice.UnitPath(runtime.GOOS, homeDir)
		if !unitDescribesAddr(unitPath, cfg.Addr) {
			return fmt.Errorf("%s is already in use%s — refusing to continue, because the readiness check cannot tell our gateway from whatever is listening. Stop it, or choose another --gateway-addr", cfg.Addr, who)
		}
		fmt.Fprintf(a.stdout, "  replacing      the gateway already installed at %s (%s)\n", cfg.Addr, unitPath)
		a.unloadUnit(unitPath)
		// Give the socket a moment to clear so the fresh start can bind. A failure
		// to clear surfaces as the readiness timeout below, which already says what
		// to do about it.
		if !waitForPortFreeFn(cfg.Addr, gatewayStopTimeout) {
			return fmt.Errorf("the gateway already running on %s did not stop within %s — ANTHROPIC_BASE_URL was left as it was. Stop it by hand and re-run init", cfg.Addr, gatewayStopTimeout)
		}
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve this binary's path for the service unit: %w", err)
	}

	unitPath, err := gatewayservice.WriteUnit(runtime.GOOS, homeDir, binPath, cfg.Addr, cfg.Upstream)
	if err != nil {
		// Includes the Windows case, which is an error rather than a silent skip:
		// a developer who believes a service was installed and finds none later is
		// worse off than one told plainly at install time.
		return err
	}
	fmt.Fprintf(a.stdout, "  gateway unit   %s\n", unitPath)

	if err := a.loadUnit(unitPath); err != nil {
		// Do NOT write the env var. Report and leave the machine working — and
		// leave NO unit behind either: see rollbackUnit.
		a.rollbackUnit(unitPath)
		return fmt.Errorf("wrote %s but could not start it (%w) — ANTHROPIC_BASE_URL was NOT set, so model calls still work and are ungoverned. Start it by hand with `openbox gateway`, then re-run init", unitPath, err)
	}

	if !waitForListenerFn(cfg.Addr, gatewayReadyTimeout) {
		a.rollbackUnit(unitPath)
		return fmt.Errorf("the gateway did not start listening on %s within %s — ANTHROPIC_BASE_URL was NOT set, so model calls still work and are ungoverned. Check the service logs, or run `openbox gateway` in the foreground to see why", cfg.Addr, gatewayReadyTimeout)
	}
	fmt.Fprintf(a.stdout, "  gateway        listening on %s\n", cfg.Addr)

	// Only now. Everything above had to succeed for this to be safe.
	replaced, err := gatewayservice.WriteEnv(homeDir, cfg.Addr)
	if err != nil {
		a.rollbackUnit(unitPath)
		return err
	}
	fmt.Fprintf(a.stdout, "  %s  %s (user scope: %s)\n",
		gatewayservice.EnvKey, "http://"+cfg.Addr, gatewayservice.SettingsPath(homeDir))
	for _, r := range replaced {
		// Never silent: the developer or their org had pointed this elsewhere.
		fmt.Fprintf(a.stdout, "  replaced       %s\n", r)
	}
	return nil
}

// removeGateway is the uninstall path, in the opposite order.
func (a *app) removeGateway(homeDir string) error {
	// Env var FIRST, so the tool stops pointing at the gateway before the gateway
	// goes away. The reverse would leave a window where every model call fails.
	removed, restored, err := gatewayservice.RemoveEnvDetailed(homeDir)
	if err != nil {
		return err
	}
	for _, key := range removed {
		fmt.Fprintf(a.stdout, "  removed        %s from %s\n", key, gatewayservice.SettingsPath(homeDir))
	}
	// A restore has to be SAID. It is the opposite of a removal — the machine is
	// back to what the org configured, not unconfigured — and a developer who is
	// told "removed" about a value that was actually put back will go looking for
	// the wrong thing.
	if restored != "" {
		fmt.Fprintf(a.stdout, "  restored       %s = %s (the value that was there before OpenBox)\n",
			gatewayservice.EnvKey, restored)
	}

	// Unload BEFORE deleting the file. launchctl's fallback spelling
	// (`launchctl unload <path>`) has to READ the plist to identify the job, so
	// doing this after os.Remove left the fallback structurally unable to help: if
	// the primary `bootout` failed, a KeepAlive job could stay running and
	// restarting with no unit on disk, silently, while init reported success.
	a.unloadUnit(gatewayservice.UnitPath(runtime.GOOS, homeDir))

	unitPath, err := gatewayservice.RemoveUnit(runtime.GOOS, homeDir)
	if err != nil {
		return err
	}
	if unitPath != "" {
		fmt.Fprintf(a.stdout, "  removed        %s\n", unitPath)
	}
	return nil
}

// loadUnit asks the OS supervisor to take ownership. Best-effort by design on the
// unload side, strict here: if the supervisor will not take it, the caller must
// not proceed to the env write.
func (a *app) loadUnit(unitPath string) error {
	switch runtime.GOOS {
	case "darwin":
		// bootstrap is the modern spelling; `load` is kept as a fallback because
		// it is what older macOS accepts, and an install that works on one and
		// not the other is a support problem rather than a purity question.
		if err := run("launchctl", "bootstrap", "gui/"+currentUID(), unitPath); err == nil {
			return nil
		}
		return run("launchctl", "load", "-w", unitPath)
	case "linux":
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return run("systemctl", "--user", "enable", "--now", gatewayservice.SystemdUnitName)
	default:
		return fmt.Errorf("no supervisor integration for %s", runtime.GOOS)
	}
}

// unloadUnit is best-effort: the file is already gone, and a supervisor that has
// forgotten the unit is the desired end state either way.
func (a *app) unloadUnit(unitPath string) {
	switch runtime.GOOS {
	case "darwin":
		if run("launchctl", "bootout", "gui/"+currentUID()+"/"+gatewayservice.LaunchdLabel) != nil {
			_ = run("launchctl", "unload", unitPath)
		}
	case "linux":
		_ = run("systemctl", "--user", "disable", "--now", gatewayservice.SystemdUnitName)
		_ = run("systemctl", "--user", "daemon-reload")
	}
}

// waitForListenerFn is the readiness probe, behind a seam so a test can drive the
// "supervisor accepted the unit but nothing is serving" branch without waiting out
// a real timeout. Accepting a unit is not evidence that a process is serving, and
// that distinction is the whole point of probing at all.
// Typed, not variadic-any. It took `(addr string, args ...any)` and asserted a
// time.Duration out of args[0] to receive one argument its only caller always
// passes explicitly — so `waitForListenerFn(addr, 5)` compiled, silently failed
// the assertion, and waited the default instead of 5. A plain function value keeps
// the seam and lets the compiler check it.
var waitForListenerFn = waitForListener

// waitForListener polls until something accepts a connection, or the deadline
// passes. Polling rather than sleeping-then-checking: a supervisor's start is not
// instantaneous and is not a fixed duration either.
func waitForListener(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// run executes a supervisor command, discarding output. Its error is what the
// caller acts on.
var run = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

// currentUID is the launchd domain target. os.Getuid returns -1 on Windows, which
// this path never reaches.
var currentUID = func() string { return fmt.Sprint(os.Getuid()) }

// homeDir resolves the developer's home. A seam on app so a test can point the
// whole gateway step at a temp dir rather than at the real machine — the same
// discipline isolateHome already applies to everything else init writes.
func (a *app) homeDir() string {
	if h := a.getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// portOccupied reports whether something already accepts connections at addr.
//
// A successful DIAL, not a failed bind: binding to test would race the daemon we
// are about to start, and on some platforms a bind-then-close leaves the port in a
// state the real listener has to wait out.
func portOccupied(addr string) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false, ""
	}
	conn.Close()
	// No identity to report: whatever answered, it answered before our gateway
	// existed, so it is not ours.
	return true, " (something is already listening there)"
}

// printGatewayPlan describes the gateway half of a --dry-run.
//
// It exists because the dry-run branch returns before the gateway blocks, so the
// plan covered hooks and posture and said nothing about a supervised daemon or a
// rewritten ANTHROPIC_BASE_URL — the one action scope.go singles out as "the
// largest-blast-radius thing this command can do", and the one an operator
// vetting a fleet script most needs named.
func (a *app) printGatewayPlan(withGateway, removeGateway bool, addr, upstream string) {
	home := a.homeDir()
	switch {
	case withGateway:
		fmt.Fprintf(a.stdout, "\nLocal gateway (model-call governance) — PLANNED\n")
		fmt.Fprintf(a.stdout, "  unit         %s\n", unitPathForPlan(home))
		fmt.Fprintf(a.stdout, "  listen       %s  (loopback only)\n", addr)
		fmt.Fprintf(a.stdout, "  upstream     %s\n", upstream)
		fmt.Fprintf(a.stdout, "  settings     %s  sets %s=http://%s\n",
			gatewayservice.SettingsPath(home), gatewayservice.EnvKey, addr)
		fmt.Fprintf(a.stdout, "               this REDIRECTS every model call this machine makes.\n")
		fmt.Fprintf(a.stdout, "               The settings write happens last and only once the daemon is proven up.\n")
	case removeGateway:
		fmt.Fprintf(a.stdout, "\nRemoving local gateway configuration — PLANNED\n")
		fmt.Fprintf(a.stdout, "  unit         %s  (stopped and removed)\n", unitPathForPlan(home))
		fmt.Fprintf(a.stdout, "  settings     %s  unsets %s\n",
			gatewayservice.SettingsPath(home), gatewayservice.EnvKey)
		if prior, present := gatewayservice.CurrentEnv(home); present {
			fmt.Fprintf(a.stdout, "  current      %s=%s\n", gatewayservice.EnvKey, prior)
		}
	}
}

// unitPathForPlan names the unit this OS would write, or says none is packaged.
func unitPathForPlan(home string) string {
	if p := gatewayservice.UnitPath(runtime.GOOS, home); p != "" {
		return p
	}
	return "(no daemon packaging on " + runtime.GOOS + ")"
}

// gatewayHome resolves the home directory the gateway writes into, refusing an
// empty one.
//
// homeDir() falls back to "" when neither $HOME nor os.UserHomeDir() answers — a
// CI runner, `env -i`, a `su` without `-`. Every path built from it then becomes
// RELATIVE to the working directory, which for `init` is the developer's project:
// gatewayservice.SettingsPath("") is `.claude/settings.json`, i.e. the project's
// own often-committed settings file, and WriteEnv would put
// ANTHROPIC_BASE_URL=http://127.0.0.1:8788 in it — pointing every teammate who
// checks it out at a loopback port that does not exist on their machine.
// LaunchdPath("") is likewise a relative Library/LaunchAgents inside the repo, and
// launchctl would be handed a relative path.
//
// Refusing is the only safe answer: a home the process cannot name is not a home
// it may guess.
func (a *app) gatewayHome() (string, int) {
	home := a.homeDir()
	if home == "" {
		return "", a.errorf("cannot resolve a home directory for the gateway configuration: " +
			"set HOME to an absolute path. Refusing to write to paths relative to the current " +
			"directory, which would put " + gatewayservice.EnvKey + " in this project's own settings file")
	}
	if !filepath.IsAbs(home) {
		return "", a.errorf("HOME is %q, which is not absolute; the gateway's unit and settings paths would resolve against the current directory", home)
	}
	return home, exitOK
}

// gatewayStopTimeout bounds how long a replace waits for the old daemon's socket
// to clear. Short: it is a local process being asked to stop, and the readiness
// timeout below already covers a slow start.
const gatewayStopTimeout = 5 * time.Second

// waitForPortFreeFn is a seam, like waitForListenerFn, so a test can drive the
// replace path without a real socket.
var waitForPortFreeFn = waitForPortFree

// waitForPortFree polls until nothing accepts connections at addr.
func waitForPortFree(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if occupied, _ := portOccupied(addr); !occupied {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// unitDescribesAddr reports whether the unit we wrote names this listen address.
//
// A substring match on the unit body, which is exactly right here: both renderers
// put the address in the argv as `--addr <addr>` (the plist as its own <string>,
// the systemd unit inside ExecStart), so finding it means OUR unit is configured
// for this port. Reading the file is also what makes this an OWNERSHIP test
// rather than a socket probe — the thing the old pre-check could not do.
//
// Any read failure answers false: no unit we can read means nothing we can claim,
// and refusing is the safe direction.
func unitDescribesAddr(unitPath, addr string) bool {
	if unitPath == "" || addr == "" {
		return false
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), addr)
}

// rollbackUnit undoes a unit that was written and then could not be made to work.
//
// setupGateway's own doc promises "any failure leaves the machine unconfigured
// rather than half-configured", and every failure after WriteUnit broke that: the
// unit stayed on disk with KeepAlive / Restart=always, so the supervisor kept
// restart-looping a gateway the developer was never told about — while `init`
// exited 0 because main.go downgrades the error to a warning. Worse, the error's
// own remedy ("re-run init") was then blocked by the port pre-check seeing that
// very daemon.
//
// Best-effort and silent about its own failures: this runs on a path that is
// already reporting an error, and a second error about cleaning up the first
// would bury it.
func (a *app) rollbackUnit(unitPath string) {
	if unitPath == "" {
		return
	}
	a.unloadUnit(unitPath)
	if err := os.Remove(unitPath); err == nil {
		fmt.Fprintf(a.stdout, "  rolled back    removed %s\n", unitPath)
	}
}
