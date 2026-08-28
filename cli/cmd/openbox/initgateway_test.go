package main

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayservice"
)

// stubSupervisor replaces the launchctl/systemctl calls, and optionally brings a
// real listener up so the readiness probe has something to find.
func stubSupervisor(t *testing.T, addr string, startFails bool) {
	t.Helper()
	origRun, origUID := run, currentUID
	t.Cleanup(func() { run, currentUID = origRun, origUID })

	// Route the unit write through the PATH-EXPLICIT writer for the duration of the
	// test. Production uses kardianos/service, which ignores $HOME on darwin and
	// would install a real launchd unit into the developer's home on every
	// `go test` — see installUnitFn. Identical bytes either way; only the location
	// differs, and here the location is the whole point.
	origInstall, origUninstall := installUnitFn, uninstallUnitFn
	t.Cleanup(func() { installUnitFn, uninstallUnitFn = origInstall, origUninstall })
	installUnitFn = func(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
		_, err := gatewayservice.WriteUnit(goos, homeDir, binPath, addr, upstream, verbose)
		return err
	}
	uninstallUnitFn = func(goos, homeDir string) error {
		_, err := gatewayservice.RemoveUnit(goos, homeDir)
		return err
	}

	currentUID = func() string { return "501" }
	run = func(string, ...string) error {
		if startFails {
			return errors.New("supervisor refused the unit")
		}
		if addr != "" {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return nil // already listening: fine for this test's purposes
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
		}
		return nil
	}
}

// freeAddr reserves a loopback port and releases it, so the stub can bind it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func skipUnlessSupervised(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no daemon packaging on %s (phase 07 req 7)", runtime.GOOS)
	}
}

// TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart is the safety property, and
// it is the one that would actually hurt a developer.
//
// If ANTHROPIC_BASE_URL is written while nothing listens, a dead localhost fails
// closed and EVERY model call on the machine fails — `init` would have broken the
// developer's tool while printing success. So the env write is last and
// conditional, and a failed start must leave the machine exactly as it was.
func TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, _, _ := testApp(nil)
	stubSupervisor(t, "", true) // start fails

	// freeAddr, NOT the production default. This hardcoded gateway.DefaultAddr
	// ("127.0.0.1:8788"), so on any machine actually running `openbox gateway` —
	// i.e. anyone dogfooding this feature — setupGateway returned at the
	// port-occupied pre-check and never reached the failed-start branch this case
	// is about. The assertion then failed on the wrong error and the CLI suite went
	// red for exactly the developers most likely to run it.
	err := a.setupGateway(home, freeAddr(t), "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway reported success though the daemon never started")
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written with no gateway listening — every model call on this machine would now fail")
	}
	// The error has to say the machine still works, or a developer will assume
	// the opposite and start undoing things.
	if !strings.Contains(err.Error(), "NOT set") {
		t.Errorf("the error does not say the env var was left alone: %v", err)
	}
}

// TestGatewayEnvIsNotWrittenWhenTheListenerNeverComesUp is the other half: the
// supervisor accepted the unit but nothing is listening. Accepting a unit is not
// evidence that a process is serving.
func TestGatewayEnvIsNotWrittenWhenTheListenerNeverComesUp(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, _, _ := testApp(nil)
	// Supervisor succeeds but starts nothing.
	stubSupervisor(t, "", false)

	// Shorten the wait: this test is about the branch, not about the timeout.
	orig := waitForListenerFn
	waitForListenerFn = func(string, time.Duration) bool { return false }
	t.Cleanup(func() { waitForListenerFn = orig })

	err := a.setupGateway(home, freeAddr(t), "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway succeeded with nothing listening")
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written though the readiness probe failed")
	}
}

// TestGatewaySetupWritesEnvOnlyAfterTheListenerIsUp is the happy path, and it
// asserts the ORDER rather than only the outcome.
func TestGatewaySetupWritesEnvOnlyAfterTheListenerIsUp(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, out, _ := testApp(nil)
	addr := freeAddr(t)
	stubSupervisor(t, addr, false)

	if err := a.setupGateway(home, addr, "https://api.anthropic.com", false); err != nil {
		t.Fatalf("setupGateway: %v", err)
	}
	v, present := gatewayservice.CurrentEnv(home)
	if !present || v != "http://"+addr {
		t.Errorf("env = %q, %v; want http://%s", v, present, addr)
	}
	// The unit exists too.
	unit := gatewayservice.LaunchdPath(home)
	if runtime.GOOS == "linux" {
		unit = gatewayservice.SystemdPath(home)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("unit not written: %v", err)
	}
	// "listening" must be reported BEFORE the env line, because that is the order
	// the safety property depends on.
	s := out.String()
	iListen, iEnv := strings.Index(s, "listening on"), strings.Index(s, gatewayservice.EnvKey)
	if iListen < 0 || iEnv < 0 {
		t.Fatalf("output missing one of the two steps:\n%s", s)
	}
	if iListen > iEnv {
		t.Errorf("env was reported before readiness — the order is the safety property:\n%s", s)
	}
}

// TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon is the reverse order, for the
// mirror-image reason: removing the daemon first would leave a window where the
// tool points at something gone and every model call fails.
func TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, out, _ := testApp(nil)
	addr := freeAddr(t)
	stubSupervisor(t, addr, false)

	if err := a.setupGateway(home, addr, "https://api.anthropic.com", false); err != nil {
		t.Fatalf("setup: %v", err)
	}
	out.Reset()

	if err := a.removeGateway(home); err != nil {
		t.Fatalf("removeGateway: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("the env var survived removal")
	}
	s := out.String()
	iEnv, iUnit := strings.Index(s, gatewayservice.EnvKey), strings.Index(s, ".plist")
	if runtime.GOOS == "linux" {
		iUnit = strings.Index(s, ".service")
	}
	if iEnv >= 0 && iUnit >= 0 && iEnv > iUnit {
		t.Errorf("the daemon was removed before the env var was unset:\n%s", s)
	}
}

// TestGatewaySetupRejectsANonLoopbackAddr keeps the phase 04 invariant reachable
// from the install path, not just from the gateway package.
func TestGatewaySetupRejectsANonLoopbackAddr(t *testing.T) {
	home := t.TempDir()
	a, _, _ := testApp(nil)
	err := a.setupGateway(home, "0.0.0.0:8788", "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway accepted a listener bound to every interface")
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("a rejected configuration still wrote the env var")
	}
}

// TestGatewayFlagsAreMutuallyExclusive — asking to install and remove in one run
// is a mistake worth naming rather than resolving by flag order.
func TestGatewayFlagsAreMutuallyExclusive(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--gateway", "--remove-gateway"}); code != exitError {
		t.Fatalf("exit = %d want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("stderr does not name the conflict: %q", errb.String())
	}
}

// TestGatewayIsOffByDefault pins the opt-in decision. This redirects live model
// traffic, so a plain `init` must not enable it — and if that default is ever
// flipped, it should be a deliberate change to this test, not a silent one.
func TestGatewayIsOffByDefault(t *testing.T) {
	a, out, errb := testApp(nil)
	// `init -h` lists every flag with its default, which is the surface a
	// developer actually reads. The flag package writes usage to stderr, so read
	// both rather than assuming which.
	a.run([]string{"init", "-h"})
	help := out.String() + errb.String()
	if !strings.Contains(help, "-gateway") {
		t.Fatalf("--gateway is not listed in init's help:\n%s", help)
	}
	// A bool flag defaulting to true renders "(default true)"; false renders
	// nothing. Assert the absence next to the flag's own description.
	i := strings.Index(help, "-gateway")
	window := help[i:min(i+400, len(help))]
	if strings.Contains(window, "(default true)") {
		t.Errorf("--gateway defaults to true; a plain init would redirect live model traffic:\n%s", window)
	}
	if !strings.Contains(window, "OFF by default") {
		t.Errorf("the help text does not tell a developer it is off:\n%s", window)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestOccupiedPortIsRefusedRatherThanAdopted closes the gap the readiness probe
// cannot: a bare TCP connect proves SOMETHING listens, not that it is ours.
//
// Without the pre-check, a foreign process holding the port means our gateway
// fails to bind, the probe connects to the stranger, and init points the
// developer's model traffic at an unknown local service while printing success.
func TestOccupiedPortIsRefusedRatherThanAdopted(t *testing.T) {
	skipUnlessSupervised(t)
	// A stranger on the port, up before init runs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	home := t.TempDir()
	a, _, _ := testApp(nil)
	stubSupervisor(t, "", false)

	err = a.setupGateway(home, ln.Addr().String(), "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway adopted a port held by a foreign process")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the error does not name the cause: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written pointing at an unknown local service")
	}
}

// TestReInstallReplacesOurOwnGatewayInsteadOfRefusing is the "test the SECOND
// invocation" rule this repo already learned once, applied to the gateway.
//
// The port pre-check could not tell our own daemon from a stranger, so the second
// `init --gateway` on any gateway-enabled machine returned BEFORE WriteUnit — and
// a unit written by an older binary could never be refreshed. That is the same
// stale-path failure `init` repairs for hook registrations, and the remedy its own
// error recommends ("re-run init") was the thing that could not work.
func TestReInstallReplacesOurOwnGatewayInsteadOfRefusing(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	addr := freeAddr(t)
	a, out, _ := testApp(nil)
	stubSupervisor(t, addr, false)

	// First install writes the unit and (via the stub) brings a listener up.
	if err := a.setupGateway(home, addr, "https://api.anthropic.com", false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(home); !present {
		t.Fatal("first install did not write the env var")
	}

	// Now the port IS occupied — by us. A re-run must replace, not refuse.
	occupied, _ := portOccupied(addr)
	if !occupied {
		t.Skip("the stub listener did not stay up; this case needs a held port")
	}
	// The stubbed supervisor cannot actually stop its own listener — `run` is a
	// no-op — so the socket-clear wait is stubbed too. What is under test is
	// replace-vs-REFUSE: that setupGateway gets past the port pre-check and
	// rewrites the unit, not that launchctl works.
	origFree := waitForPortFreeFn
	waitForPortFreeFn = func(string, time.Duration) bool { return true }
	t.Cleanup(func() { waitForPortFreeFn = origFree })

	if err := a.setupGateway(home, addr, "https://api.anthropic.com", false); err != nil {
		t.Errorf("re-install refused instead of replacing: %v", err)
	}
	if !strings.Contains(out.String(), "replacing") {
		t.Errorf("the replace was silent; a swap has to say what it retired:\n%s", out.String())
	}
}

// TestAForeignProcessOnThePortIsStillRefused is the other half: the ownership test
// must not become a licence to stop whatever is listening. Over-refuse, never
// over-terminate.
func TestAForeignProcessOnThePortIsStillRefused(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()

	// Something else holds a port, and no unit of ours names it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	addr := ln.Addr().String()

	a, _, _ := testApp(nil)
	stubSupervisor(t, "", false)
	err = a.setupGateway(home, addr, "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway proceeded over a foreign listener")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the error does not name the conflict: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written while a foreign process held the port")
	}
}

// TestAFailedInstallLeavesNoUnitBehind is setupGateway's own documented promise —
// "any failure leaves the machine unconfigured rather than half-configured".
//
// Every failure after WriteUnit broke it: the unit stayed on disk with
// KeepAlive/Restart=always, so the supervisor kept restart-looping a gateway
// nobody was told about, `init` still exited 0 (main.go downgrades the error to a
// warning), and the error's own remedy — re-run init — was blocked by the port
// pre-check seeing that very daemon.
func TestAFailedInstallLeavesNoUnitBehind(t *testing.T) {
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, _, _ := testApp(nil)
	stubSupervisor(t, "", false) // supervisor accepts, nothing listens

	orig := waitForListenerFn
	waitForListenerFn = func(string, time.Duration) bool { return false }
	t.Cleanup(func() { waitForListenerFn = orig })

	if err := a.setupGateway(home, freeAddr(t), "https://api.anthropic.com", false); err == nil {
		t.Fatal("setupGateway reported success with nothing listening")
	}
	if path := gatewayservice.UnitPath(runtime.GOOS, home); path != "" {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s survived a failed install — the supervisor will restart-loop a gateway "+
				"the developer was never told about, and the port pre-check will then block the re-run", path)
		}
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written despite the failure")
	}
}
