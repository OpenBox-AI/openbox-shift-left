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
)

const gatewayReadyTimeout = 10 * time.Second

func gatewayIdentity(homeDir string) laneIdentity {
	return laneIdentity{
		unitPath:     gatewayservice.UnitPath(runtime.GOOS, homeDir),
		launchdLabel: gatewayservice.LaunchdLabel,
		systemdUnit:  gatewayservice.SystemdUnitName,
	}
}

// selfPath is the binary a supervisor unit will name. It refuses a temporary
// build, which is the one shape that produces a unit pointing at a file that
// will not exist.
//
// `go run` compiles to a temp directory and deletes the binary on exit. The
// hooks are safe -- they name the stable copy in the plugin bundle -- but a
// lane unit names THIS path, with KeepAlive and Restart=always. So the daemon
// comes up, passes its readiness check, gets the proxy and CA env keys written
// behind it, and then vanishes: on the next login the supervisor restart-loops
// a missing file while every model call fails closed against a dead port. It
// was reachable only through an opt-in flag before; an install brings the lanes
// up by default now, so it is the default path for anyone running from source.
func (a *app) selfPath() (string, error) {
	binPath, err := executableFn()
	if err != nil {
		return "", fmt.Errorf("cannot resolve this binary's path for the service unit: %w", err)
	}
	if reason := temporaryBuild(binPath); reason != "" {
		return "", fmt.Errorf("refusing to install a supervisor unit that points at %s: %s.\n"+
			"  The unit would outlive that file, and a supervisor cannot restart a binary that is\n"+
			"  gone -- every model call would then fail against a dead port.\n"+
			"  Build it somewhere that survives first:  go build -o ./openbox ./cmd/openbox && ./openbox init …",
			binPath, reason)
	}
	return binPath, nil
}

// executable is a seam only so a test can present a temporary path without
// building one.
var executableFn = os.Executable

// temporaryBuild names why a path is not durable, or "" when it is.
//
// The signal is a go-build cache segment, which is precisely what `go run`
// produces and deletes. Refusing everything under the temp directory instead
// would be both too broad and self-contradicting: `go build -o /tmp/openbox` is
// the remedy this refusal recommends, and it lands there.
func temporaryBuild(path string) string {
	for _, seg := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if strings.HasPrefix(seg, "go-build") {
			return "it is a `go run` build, deleted when the command exits"
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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
			if restored != "" {
				fmt.Fprintf(a.stdout, "  restored       %s = %s (the value that was there before OpenBox)\n",
					gatewayservice.EnvKey, restored)
			}
			return nil
		},
	})
}

// loadUnit best-effort by design on the unload side, strict here: if the
// supervisor will not take it, the caller must not proceed to the env write.
func (a *app) loadUnit(id laneIdentity) error {
	switch runtime.GOOS {
	case "darwin":
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

var installUnitFn = gatewayservice.Reinstall

var uninstallUnitFn = gatewayservice.Uninstall

var waitForListenerFn = waitForListener

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

var run = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

var currentUID = func() string { return fmt.Sprint(os.Getuid()) }

func (a *app) homeDir() string {
	if h := a.getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// portOccupied is a var like installUnitFn: it reaches the real network, which the
// machine running this suite has its own lanes on.
var portOccupied = func(addr string) (bool, string) {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false, ""
	}
	conn.Close()
	return true, " (something is already listening there)"
}

// gatewayHome refusing is the only safe answer: a home the process cannot name
// is not a home it may guess.
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

const gatewayStopTimeout = 5 * time.Second

var waitForPortFreeFn = waitForPortFree

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
