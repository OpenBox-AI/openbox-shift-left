package main

import (
	"errors"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
)

func stubSupervisor(t *testing.T, addr string, startFails bool) {
	t.Helper()
	origRun, origUID := run, currentUID
	t.Cleanup(func() { run, currentUID = origRun, origUID })

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

// TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart is the safety property,
// and it is the one that would actually hurt a developer. So the env write is
// last and conditional, and a failed start must leave the machine exactly as
// it was.
func TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, _, _ := testApp(nil)
	stubSupervisor(t, "", true) // start fails

	// This hardcoded gateway.DefaultAddr ("127.0.0.1:8788"), so on any machine
	// actually running `openbox gateway`; i.e. Anyone dogfooding this feature;
	// setupGateway returned at the port-occupied pre-check and never reached the
	// failed-start branch this case is about.
	err := a.setupGateway(home, freeAddr(t), "https://api.anthropic.com", false)
	if err == nil {
		t.Fatal("setupGateway reported success though the daemon never started")
	}
	if _, present := gatewayservice.CurrentEnv(home); present {
		t.Error("ANTHROPIC_BASE_URL was written with no gateway listening — every model call on this machine would now fail")
	}
	if !strings.Contains(err.Error(), "NOT set") {
		t.Errorf("the error does not say the env var was left alone: %v", err)
	}
}

// TestGatewayEnvIsNotWrittenWhenTheListenerNeverComesUp is the other half: the
// supervisor accepted the unit but nothing is listening.
func TestGatewayEnvIsNotWrittenWhenTheListenerNeverComesUp(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, _, _ := testApp(nil)
	stubSupervisor(t, "", false)

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
// asserts the order rather than only the outcome.
func TestGatewaySetupWritesEnvOnlyAfterTheListenerIsUp(t *testing.T) {
	memhttptest.RequireBind(t)
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
	unit := gatewayservice.LaunchdPath(home)
	if runtime.GOOS == "linux" {
		unit = gatewayservice.SystemdPath(home)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("unit not written: %v", err)
	}
	s := out.String()
	iListen, iEnv := strings.Index(s, "listening on"), strings.Index(s, gatewayservice.EnvKey)
	if iListen < 0 || iEnv < 0 {
		t.Fatalf("output missing one of the two steps:\n%s", s)
	}
	if iListen > iEnv {
		t.Errorf("env was reported before readiness — the order is the safety property:\n%s", s)
	}
}

// TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon is the reverse order, for
// the mirror-image reason: removing the daemon first would leave a window
// where the tool points at something gone and every model call fails.
func TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon(t *testing.T) {
	memhttptest.RequireBind(t)
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

// TestGatewaySetupRejectsANonLoopbackAddr keeps the phase 04 invariant
// reachable from the install path, not just from the gateway package.
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

// TestGatewayFlagsAreMutuallyExclusive; asking to install and remove in one
// run is a mistake worth naming rather than resolving by flag order.
func TestGatewayFlagsAreMutuallyExclusive(t *testing.T) {
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--gateway", "--remove-gateway"}); code != exitError {
		t.Fatalf("exit = %d want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("stderr does not name the conflict: %q", errb.String())
	}
}

// TestGatewayIsOffByDefault pins the opt-in decision. This redirects live
// model traffic, so a plain `init` must not enable it; and if that default is
// ever flipped, it should be a deliberate change to this test, not a silent
// one.
func TestGatewayIsOffByDefault(t *testing.T) {
	a, out, errb := testApp(nil)
	a.run([]string{"init", "-h"})
	help := out.String() + errb.String()
	if !strings.Contains(help, "-gateway") {
		t.Fatalf("--gateway is not listed in init's help:\n%s", help)
	}
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

// TestOccupiedPortIsRefusedRatherThanAdopted closes the gap the readiness
// probe cannot: a bare TCP connect proves something listens, not that it is
// ours.
func TestOccupiedPortIsRefusedRatherThanAdopted(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
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

// TestReInstallReplacesOurOwnGatewayInsteadOfRefusing is the "test the second
// invocation" rule this repo already learned once, applied to the gateway.
func TestReInstallReplacesOurOwnGatewayInsteadOfRefusing(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	home := t.TempDir()
	addr := freeAddr(t)
	a, out, _ := testApp(nil)
	stubSupervisor(t, addr, false)

	if err := a.setupGateway(home, addr, "https://api.anthropic.com", false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(home); !present {
		t.Fatal("first install did not write the env var")
	}

	// A re-run must replace, not refuse.
	occupied, _ := portOccupied(addr)
	if !occupied {
		t.Skip("the stub listener did not stay up; this case needs a held port")
	}
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

// TestAForeignProcessOnThePortIsStillRefused is the other half: the ownership
// test must not become a licence to stop whatever is listening. Over-refuse,
// never over-terminate.
func TestAForeignProcessOnThePortIsStillRefused(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	home := t.TempDir()

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

// TestAFailedInstallLeavesNoUnitBehind is setupGateway's own documented
// promise; "any failure leaves the machine unconfigured rather than half-
// configured".
func TestAFailedInstallLeavesNoUnitBehind(t *testing.T) {
	memhttptest.RequireBind(t)
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

// TestUnitAddrMatchIsAWholeToken pins the ownership check that authorizes
// killing whatever holds the port. Over-refuse is the direction this check
// chose; over-terminate is the one it must never take.
func TestUnitAddrMatchIsAWholeToken(t *testing.T) {
	const plist = "<string>--addr</string>\n<string>127.0.0.1:8788</string>\n"
	const unitSystemd = `ExecStart=/usr/local/bin/openbox gateway --addr "127.0.0.1:8788"`

	for name, tc := range map[string]struct {
		body, addr string
		want       bool
	}{
		"plist, exact":               {plist, "127.0.0.1:8788", true},
		"systemd, exact":             {unitSystemd, "127.0.0.1:8788", true},
		"plist, shorter port prefix": {plist, "127.0.0.1:878", false},
		"plist, longer port":         {plist, "127.0.0.1:87880", false},
		"plist, different host":      {plist, "127.0.0.2:8788", false},
	} {
		if got := containsAddrToken(tc.body, tc.addr); got != tc.want {
			t.Errorf("%s: containsAddrToken(%q) = %v, want %v", name, tc.addr, got, tc.want)
		}
	}
}
