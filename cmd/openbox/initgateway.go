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

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
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
// That decision's lesson is that a default-off headline feature stays off, and
// that argues for defaulting this ON. It is deliberately off anyway, because the
// two cases are not alike: enforcement-by-default is INERT without an org policy,
// so flipping it could not break anyone. This redirects live model traffic
// through a process that has never run against a real stack, whose refusal shape
// is still unprobed, and which has no daemon packaging on Windows at all. A
// default that can break the developer's tool before the path is proven end to
// end is the opposite of the caution this repo applies everywhere else.
//
// Revisit the default when phase 08 has run the end-to-end path. That is the
// owner's call, and it should be made with evidence rather than by symmetry with.

// gatewayReadyTimeout bounds how long init waits for the supervisor to bring the
// daemon up before deciding it did not.
const gatewayReadyTimeout = 10 * time.Second

// setupGateway installs, starts and verifies the gateway, then points the tool at
// it. Any failure leaves the machine unconfigured rather than half-configured.
//
// The ORDER — unit, start, prove, env — and the rollback live in setupLane, which
// all three lanes share. What stays here is what is only true of this lane: its
// config validation, its one env key, and the seams its own tests bind.
func (a *app) setupGateway(homeDir, addr, upstream string, verbose bool) error {
	cfg := gateway.Config{Addr: addr, Upstream: upstream}
	if err := cfg.Validate(); err != nil {
		return err
	}
	binPath, err := a.selfPath()
	if err != nil {
		return err
	}
	return a.setupLane(laneInstall{
		label:        "gateway",
		addr:         cfg.Addr,
		homeDir:      homeDir,
		laneIdentity: gatewayIdentity(homeDir),
		// The gateway's own seams, not the shared ones: nine tests bind these
		// directly, and they are the tests that prove ANTHROPIC_BASE_URL is never
		// written while nothing listens.
		installUnit: func() error {
			return installUnitFn(runtime.GOOS, homeDir, binPath, cfg.Addr, cfg.Upstream, verbose)
		},
		uninstallUnit: func() error { return uninstallUnitFn(runtime.GOOS, homeDir) },
		envNotSet:     gatewayservice.EnvKey + " was NOT set, so model calls still work and are ungoverned",
		activate: func() ([]string, error) {
			replaced, err := gatewayservice.WriteEnv(homeDir, cfg.Addr)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(a.stdout, "  %s  %s (user scope: %s)\n",
				gatewayservice.EnvKey, "http://"+cfg.Addr, gatewayservice.SettingsPath(homeDir))
			return replaced, nil
		},
	})
}

// gatewayIdentity is how the supervisor addresses the gateway. Built from
// gatewayservice's own constants rather than from a lane spec, because those
// constants are what doctor and the re-install detection already read.
func gatewayIdentity(homeDir string) laneIdentity {
	return laneIdentity{
		unitPath:     gatewayservice.UnitPath(runtime.GOOS, homeDir),
		launchdLabel: gatewayservice.LaunchdLabel,
		systemdUnit:  gatewayservice.SystemdUnitName,
	}
}

// selfPath resolves this binary's path for a service unit.
func (a *app) selfPath() (string, error) {
	binPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve this binary's path for the service unit: %w", err)
	}
	return binPath, nil
}

// gatewaySettingsPath is the settings file every lane writes, resolved in one
// place so the lanes and doctor cannot disagree about which file that is.
func gatewaySettingsPath(homeDir string) string { return gatewayservice.SettingsPath(homeDir) }

// fileExists reports whether a path is there. Used where naming a file that is
// absent would break the tool rather than degrade a lane.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeGateway is the uninstall path, in the opposite order.
//
// Env var FIRST, so the tool stops pointing at the gateway before the gateway
// goes away. The reverse would leave a window where every model call fails.
func (a *app) removeGateway(homeDir string) error {
	return a.removeLane(laneRemoval{
		label:         "gateway",
		laneIdentity:  gatewayIdentity(homeDir),
		uninstallUnit: func() error { return uninstallUnitFn(runtime.GOOS, homeDir) },
		deactivate: func() error {
			removed, restored, err := gatewayservice.RemoveEnvDetailed(homeDir)
			if err != nil {
				return err
			}
			for _, key := range removed {
				fmt.Fprintf(a.stdout, "  removed        %s from %s\n", key, gatewayservice.SettingsPath(homeDir))
			}
			// A restore has to be SAID. It is the opposite of a removal — the
			// machine is back to what the org configured, not unconfigured — and a
			// developer told "removed" about a value that was actually put back
			// will go looking for the wrong thing.
			if restored != "" {
				fmt.Fprintf(a.stdout, "  restored       %s = %s (the value that was there before OpenBox)\n",
					gatewayservice.EnvKey, restored)
			}
			return nil
		},
	})
}

// loadUnit asks the OS supervisor to take ownership. Best-effort by design on the
// unload side, strict here: if the supervisor will not take it, the caller must
// not proceed to the env write.
//
// It takes the lane's IDENTITY rather than reading the gateway's, and that is a
// correctness fix rather than a tidy-up: `systemctl --user enable --now` names a
// unit, and `launchctl bootout` names a label, so a hardcoded gateway identity
// here would have started the gateway when asked to start telemetry — reporting
// success while the lane the developer installed never came up.
func (a *app) loadUnit(id laneIdentity) error {
	switch runtime.GOOS {
	case "darwin":
		// bootstrap is the modern spelling; `load` is kept as a fallback because
		// it is what older macOS accepts, and an install that works on one and
		// not the other is a support problem rather than a purity question.
		if err := run("launchctl", "bootstrap", "gui/"+currentUID(), id.unitPath); err == nil {
			return nil
		}
		return run("launchctl", "load", "-w", id.unitPath)
	case "linux":
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return run("systemctl", "--user", "enable", "--now", id.systemdUnit)
	default:
		return fmt.Errorf("no supervisor integration for %s", runtime.GOOS)
	}
}

// unloadUnit is best-effort: the file may already be gone, and a supervisor that
// has forgotten the unit is the desired end state either way.
func (a *app) unloadUnit(id laneIdentity) {
	switch runtime.GOOS {
	case "darwin":
		if run("launchctl", "bootout", "gui/"+currentUID()+"/"+id.launchdLabel) != nil {
			_ = run("launchctl", "unload", id.unitPath)
		}
	case "linux":
		_ = run("systemctl", "--user", "disable", "--now", id.systemdUnit)
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
// installUnitFn / uninstallUnitFn are the unit-file seam.
//
// Production goes through kardianos/service (D-OSS-3). That library derives the
// darwin plist path from user.Current() and IGNORES $HOME, with no override — so
// calling it from a test would write a live launchd unit into the home directory of
// whoever ran `go test`, including CI, no matter what homeDir the test passed.
//
// The nine tests that drive setupGateway are the ones that prove the safety
// property this repo cares most about — that ANTHROPIC_BASE_URL is never written
// while nothing listens, and that a failed start leaves no unit behind. Gating them
// off by default to accommodate the library would remove that proof from every
// ordinary test run, which is a worse trade than routing them through the
// path-explicit writer. Both produce identical bytes: the bodies come from the same
// renderers, and gatewayservice.TestSuppliedTemplatesSurviveRendering pins that the
// library's template render is an identity transform over them. The library's own
// write is covered by its opt-in artifact test.
var installUnitFn = gatewayservice.Reinstall

var uninstallUnitFn = gatewayservice.Uninstall

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
// A search of the unit body, which is exactly right here: both renderers put the
// address in the argv as `--addr <addr>` (the plist as its own <string>, the
// systemd unit inside ExecStart), so finding it means OUR unit is configured for
// this port. Reading the file is also what makes this an OWNERSHIP test rather
// than a socket probe — the thing the old pre-check could not do.
//
// WHOLE TOKEN, not a substring, because the answer authorizes KILLING whatever
// holds the port. A plain strings.Contains makes a shorter address a match for a
// longer one — a unit for `127.0.0.1:8788` "describes" `127.0.0.1:878` — so
// installing on :878 while a stranger held it would unload the working gateway on
// :8788 and then fail, leaving ANTHROPIC_BASE_URL pointed at a port with nothing
// behind it. Requiring the neighbouring bytes to be outside an address token
// (both renderers put a quote, an angle bracket or whitespace there) keeps the
// error on the over-refuse side, which is the direction this check chose.
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
	return containsAddrToken(string(body), addr)
}

// containsAddrToken reports whether addr appears in body delimited by bytes that
// cannot be part of an address.
func containsAddrToken(body, addr string) bool {
	for i := 0; ; {
		j := strings.Index(body[i:], addr)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(addr)
		if !addrByte(body, start-1) && !addrByte(body, end) {
			return true
		}
		i = start + 1
	}
}

// addrByte reports whether the byte at i could be part of an address token. Out
// of range counts as a delimiter: the string boundary ends the token.
func addrByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= '0' && c <= '9' ||
		c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c == '.' || c == ':' || c == '-' || c == '_' || c == '[' || c == ']'
}
