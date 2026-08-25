package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
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
		// Do NOT write the env var. Report and leave the machine working.
		return fmt.Errorf("wrote %s but could not start it (%w) — ANTHROPIC_BASE_URL was NOT set, so model calls still work and are ungoverned. Start it by hand with `openbox gateway`, then re-run init", unitPath, err)
	}

	if !waitForListenerFn(cfg.Addr, gatewayReadyTimeout) {
		return fmt.Errorf("the gateway did not start listening on %s within %s — ANTHROPIC_BASE_URL was NOT set, so model calls still work and are ungoverned. Check the service logs, or run `openbox gateway` in the foreground to see why", cfg.Addr, gatewayReadyTimeout)
	}
	fmt.Fprintf(a.stdout, "  gateway        listening on %s\n", cfg.Addr)

	// Only now. Everything above had to succeed for this to be safe.
	replaced, err := gatewayservice.WriteEnv(homeDir, cfg.Addr)
	if err != nil {
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
	removed, err := gatewayservice.RemoveEnv(homeDir)
	if err != nil {
		return err
	}
	for _, key := range removed {
		fmt.Fprintf(a.stdout, "  removed        %s from %s\n", key, gatewayservice.SettingsPath(homeDir))
	}

	unitPath, err := gatewayservice.RemoveUnit(runtime.GOOS, homeDir)
	if err != nil {
		return err
	}
	if unitPath != "" {
		a.unloadUnit(unitPath)
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
var waitForListenerFn = func(addr string, args ...any) bool {
	timeout := gatewayReadyTimeout
	if len(args) > 0 {
		if d, ok := args[0].(time.Duration); ok {
			timeout = d
		}
	}
	return waitForListener(addr, timeout)
}

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
