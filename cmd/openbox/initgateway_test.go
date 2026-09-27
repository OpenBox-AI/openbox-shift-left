package main

import (
	"errors"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

func stubSupervisor(t *testing.T, addr string, startFails bool) {
	t.Helper()
	origRun, origUID := run, currentUID
	t.Cleanup(func() { run, currentUID = origRun, origUID })

	// This stub binds a real loopback listener below, because the readiness gate
	// is the property under test and a faked probe would assert nothing. So it
	// opts back into the genuine probes that TestMain refuses by default.
	withRealProbes(t)

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
		t.Skipf("no daemon packaging on %s ", runtime.GOOS)
	}
}

// TestGatewayEnvIsNotWrittenWhenTheDaemonDoesNotStart is the safety property,
// and it is the one that would actually hurt a developer. So the env write is
// last and conditional, and a failed start must leave the machine exactly as
// it was.
// No command installs a gateway any more, but `openbox uninstall` still has to
// unload one on a machine that ran --gateway before the flag was removed. So
// the fixture is arranged directly rather than through an install path.
func TestRemoveGatewayUnsetsEnvBeforeRemovingTheDaemon(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	home := t.TempDir()
	a, out, _ := testApp(nil)
	addr := freeAddr(t)
	stubSupervisor(t, addr, false)

	if _, err := gatewayservice.WriteEnv(home, addr); err != nil {
		t.Fatalf("seed the env key: %v", err)
	}
	if _, err := gatewayservice.WriteUnit(runtime.GOOS, home, "/bin/openbox", addr,
		"https://api.anthropic.com", false); err != nil {
		t.Fatalf("seed the unit: %v", err)
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestOccupiedPortIsRefusedRatherThanAdopted closes the gap the readiness
// probe cannot: a bare TCP connect proves something listens, not that it is
// ours. Asserted through transport, which is one of the two lanes an install
// still brings up; the check itself is setupLane's and shared by all of them.
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

	h := newLaneHarness(t)
	h.seedCA(t)
	withRealProbes(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	_, err = a.setupTransport(h.home, ln.Addr().String(), false)
	if err == nil {
		t.Fatal("setupTransport adopted a port held by a foreign process")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the error does not name the cause: %v", err)
	}
	if _, present := laneSettings(t, h.home)["HTTPS_PROXY"]; present {
		t.Error("the proxy env key was written pointing at an unknown local service")
	}
}

// TestReInstallReplacesOurOwnLaneInsteadOfRefusing is the "test the second
// invocation" rule this repo already learned once. It matters more now than it
// did with an opt-in gateway: every `init` re-runs both lane installs, so the
// port pre-check meets a port OUR OWN daemon holds on every single re-run.
func TestReInstallReplacesOurOwnLaneInsteadOfRefusing(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	addr := freeAddr(t)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	if _, err := a.setupTransport(h.home, addr, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, present := laneSettings(t, h.home)["HTTPS_PROXY"]; !present {
		t.Fatal("first install did not write the proxy env key")
	}

	// The port now reads as held by a unit whose argv carries this address, which
	// is what authorises a replace rather than a refusal.
	origProbe := portOccupied
	portOccupied = func(string) (bool, string) { return true, " (something is already listening there)" }
	t.Cleanup(func() { portOccupied = origProbe })

	out.Reset()
	if _, err := a.setupTransport(h.home, addr, false); err != nil {
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	addr := ln.Addr().String()

	h := newLaneHarness(t)
	h.seedCA(t)
	withRealProbes(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})
	_, err = a.setupTransport(h.home, addr, false)
	if err == nil {
		t.Fatal("setupTransport proceeded over a foreign listener")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the error does not name the conflict: %v", err)
	}
	if _, present := laneSettings(t, h.home)["HTTPS_PROXY"]; present {
		t.Error("the proxy env key was written while a foreign process held the port")
	}
}

// TestAFailedInstallLeavesNoUnitBehind is setupLane's own documented promise:
// any failure leaves the machine unconfigured rather than half-configured.
func TestAFailedInstallLeavesNoUnitBehind(t *testing.T) {
	memhttptest.RequireBind(t)
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	h.listening = false // the supervisor accepts the unit; nothing ever listens
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if _, err := a.setupTransport(h.home, freeAddr(t), false); err == nil {
		t.Fatal("setupTransport reported success with nothing listening")
	}
	spec := laneservice.Transport("", "", false)
	if path := spec.UnitPath(runtime.GOOS, h.home); path != "" {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s survived a failed install; the supervisor will restart-loop a daemon "+
				"the developer was never told about, and the port pre-check will then block the re-run", path)
		}
	}
	if _, present := laneSettings(t, h.home)["HTTPS_PROXY"]; present {
		t.Error("the proxy env key was written despite the failure")
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

// TestASupervisorUnitRefusesATemporaryBinary. `go run` compiles to a temp
// directory and deletes the binary on exit. The hooks are safe -- they name the
// stable copy in the plugin bundle -- but a lane unit names the running binary,
// with KeepAlive and Restart=always. So the daemon comes up, passes its
// readiness check, gets the proxy and CA keys written behind it, and then
// vanishes: at the next login the supervisor restart-loops a missing file while
// every model call fails closed against a dead port.
//
// This was reachable only through an opt-in flag before the lanes became
// unconditional. It is now the default path for anyone running from source.
func TestASupervisorUnitRefusesATemporaryBinary(t *testing.T) {
	for name, path := range map[string]string{
		"go run build":    filepath.Join(os.TempDir(), "go-build123", "b001", "openbox"),
		"nested go-build": filepath.Join("/var", "folders", "xy", "go-build99", "exe", "openbox"),
	} {
		t.Run(name, func(t *testing.T) {
			if reason := temporaryBuild(path); reason == "" {
				t.Errorf("temporaryBuild(%q) = %q; a unit naming it would outlive the file", path, reason)
			}
		})
	}
	// Including a deliberate build into the temp directory, which is the remedy
	// the refusal itself recommends: the signal is the go-build cache, not the
	// parent directory.
	for _, path := range []string{
		"/usr/local/bin/openbox",
		filepath.Join(os.TempDir(), "openbox"),
		"/Users/dev/.claude/plugins/openbox-observe/bin/openbox",
	} {
		if reason := temporaryBuild(path); reason != "" {
			t.Errorf("temporaryBuild(%q) = %q; an installed binary must be accepted", path, reason)
		}
	}

	// And the refusal has to reach the caller, naming what to do instead.
	origExe := executableFn
	t.Cleanup(func() { executableFn = origExe })
	executableFn = func() (string, error) {
		return filepath.Join(os.TempDir(), "go-build77", "b001", "openbox"), nil
	}
	a, _, _ := testApp(nil)
	_, err := a.selfPath()
	if err == nil {
		t.Fatal("selfPath accepted a temporary build")
	}
	if !strings.Contains(err.Error(), "go build -o") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}
