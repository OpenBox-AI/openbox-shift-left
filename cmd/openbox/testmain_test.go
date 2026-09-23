package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
)

// TestMain is the asserted-hermeticity control for the package that owns
// `init`, matching the one in internal/adapters/claude-code and
// internal/adapters/codex.
//   - It does NOT redirect the working directory.
//   - It pins the Go tool's caches to their real locations before moving HOME.
func TestMain(m *testing.M) {
	scrubAmbientSessionEnv()

	if os.Getenv("GOCACHE") == "" {
		if dir, err := os.UserCacheDir(); err == nil && dir != "" {
			os.Setenv("GOCACHE", filepath.Join(dir, "go-build"))
		}
	}
	if os.Getenv("GOPATH") == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			os.Setenv("GOPATH", filepath.Join(home, "go"))
		}
	}

	sentinel, err := os.MkdirTemp("", "openbox-hermetic-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermeticity guard: cannot create sentinel dir: %v\n", err)
		os.Exit(1)
	}
	xdgConfig := filepath.Join(sentinel, "xdg-config")
	os.Setenv("HOME", sentinel)
	os.Setenv("XDG_CONFIG_HOME", xdgConfig)

	refuseTheRealSupervisor()

	code := m.Run()

	if leaks := filesUnder(sentinel, goConfigDirs(sentinel, xdgConfig)); len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "HERMETICITY VIOLATION: %d file(s) written under the sentinel HOME; "+
			"a test escaped isolateHome and would have written into the developer's real home:\n", len(leaks))
		for _, l := range leaks {
			fmt.Fprintf(os.Stderr, "  %s\n", l)
		}
		if code == 0 {
			code = 1
		}
	}
	os.RemoveAll(sentinel)
	os.Exit(code)
}

// refuseTheRealSupervisor makes every seam that leaves this process fail loudly
// by default, so a test can only reach a supervisor by asking for a fake one.
//
// The hermeticity guard above cannot see this class of escape. kardianos/service
// ignores $HOME, so it writes into the developer's REAL ~/Library/LaunchAgents,
// and `launchctl` writes nowhere under the sentinel at all; a test that installs
// a unit leaves no file for the walk to find. What it leaves is a unit whose
// ProgramArguments name os.Executable() — the test binary — with KeepAlive and
// Restart=always, which the supervisor then restart-loops. portOccupied and
// waitForListener dial 127.0.0.1 for real, so they also answer from the
// developer's own lanes.
//
// It panics rather than returning an error on purpose: the lane install path
// swallows a failure into a warning, so an errored seam would leave a test green
// and this guard silent. newLaneHarness and stubSupervisor replace these per
// test; anything reaching them here has escaped both.
// realPortOccupied and realWaitForListener are the genuine probes, kept so a
// test that deliberately binds its own loopback listener can opt back in. That
// is what stubSupervisor does: proving the readiness gate works needs a real
// dial against a real socket.
var (
	realPortOccupied    func(string) (bool, string)
	realWaitForListener func(string, time.Duration) bool
	realWaitForPortFree func(string, time.Duration) bool
)

// withRealProbes opts one test back into the genuine loopback probes, for the
// tests that are ABOUT those probes and bind their own sockets to exercise
// them. Everything else keeps TestMain's refusal.
func withRealProbes(t *testing.T) {
	t.Helper()
	origProbe, origListen, origFree := portOccupied, waitForListenerFn, waitForPortFreeFn
	t.Cleanup(func() { portOccupied, waitForListenerFn, waitForPortFreeFn = origProbe, origListen, origFree })
	portOccupied, waitForListenerFn, waitForPortFreeFn = realPortOccupied, realWaitForListener, realWaitForPortFree
}

func refuseTheRealSupervisor() {
	const escaped = "reached the real supervisor from a test: use newLaneHarness or stubSupervisor"
	realPortOccupied, realWaitForListener, realWaitForPortFree = portOccupied, waitForListenerFn, waitForPortFreeFn
	run = func(name string, args ...string) error {
		panic(escaped + " (ran " + name + " " + strings.Join(args, " ") + ")")
	}
	currentUID = func() string { panic(escaped + " (currentUID)") }
	portOccupied = func(string) (bool, string) { panic(escaped + " (portOccupied dials a real port)") }
	waitForListenerFn = func(string, time.Duration) bool { panic(escaped + " (waitForListener dials a real port)") }
	waitForPortFreeFn = func(string, time.Duration) bool { panic(escaped + " (waitForPortFree dials a real port)") }
	installLaneUnitFn = func(laneservice.Spec, string, string, string) error { panic(escaped + " (installLaneUnit)") }
	uninstallLaneUnitFn = func(laneservice.Spec, string, string) error { panic(escaped + " (uninstallLaneUnit)") }
	installUnitFn = func(string, string, string, string, string, bool) error { panic(escaped + " (installUnit)") }
	uninstallUnitFn = func(string, string) error { panic(escaped + " (uninstallUnit)") }
	// The system PAC step shells to sudo, networksetup and security; refused by
	// default for the same reason as everything above. fakeSupervisor and
	// newLaneHarness are the only fixtures that opt back in (with a no-op, not
	// a real Runner), and a dedicated systempac_test.go test restores the real
	// activation functions and drives systemPACRunner with its own fake.
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		panic(escaped + " (activateSystemPAC)")
	}
	deactivateSystemPACFn = func(context.Context, activation.Runner, activation.SystemEntry) (activation.Report, error) {
		panic(escaped + " (deactivateSystemPAC)")
	}
	liveSystemPACFn = func(context.Context, activation.Runner) ([]activation.ScopeState, error) {
		panic(escaped + " (liveSystemPAC)")
	}
	systemPACRunner = func(context.Context, string, ...string) ([]byte, error) {
		panic(escaped + " (systemPACRunner)")
	}
}

func filesUnder(root string, skipDirs []string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			out = append(out, fmt.Sprintf("(walk error at %s: %v)", path, err))
			return nil
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			for _, prefix := range skipDirs {
				if strings.HasPrefix(rel, prefix) {
					return nil
				}
			}
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// goConfigDirs returns the sentinel-relative directories where the Go
// toolchain keeps its own bookkeeping. Skipping only a `go/` directory cannot
// mask us: our own writes land under `openbox/`.
func goConfigDirs(sentinel, xdgConfig string) []string {
	sep := string(filepath.Separator)
	dirs := []string{
		filepath.Join("Library", "Application Support", "go") + sep, // macOS
		filepath.Join(".config", "go") + sep,                        // a child that clears XDG_CONFIG_HOME
	}
	if rel, err := filepath.Rel(sentinel, xdgConfig); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dirs = append(dirs, filepath.Join(rel, "go")+sep)
	}
	return dirs
}
