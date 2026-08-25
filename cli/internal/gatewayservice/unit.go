package gatewayservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// unit.go renders the OS supervisor unit that keeps the gateway running.
//
// Supervision IS the availability story. A crashed gateway restarts; a STOPPED
// one leaves model calls failing against a dead localhost, which is the safe
// direction — and `openbox doctor` says which of the two happened, so a developer
// is not debugging blind.
//
// Lifecycle belongs to the OS supervisor, not to an openbox process: the unit runs
// the binary in the foreground and the supervisor owns restart. A double-fork here
// would take the restart guarantee away from the thing that owns it.

// Label / unit names. Fixed strings, because the uninstall path has to find
// exactly what the install path wrote.
const (
	LaunchdLabel    = "ai.openbox.gateway"
	SystemdUnitName = "openbox-gateway.service"
)

// StopTimeout is the supervisor's stop timeout, and it MUST match the gateway's
// --shutdown-grace.
//
// http.Server.Shutdown never force-closes an ACTIVE connection, so whatever is
// still streaming when the grace window expires is cut when the process exits.
// Exceeding the supervisor's own timeout buys nothing — launchd SIGKILLs at 20s
// by default, systemd at 90s — so the two numbers are chosen together. 30s sits
// inside systemd's default and above launchd's, which is why launchd's is set
// explicitly below rather than left to the default.
const StopTimeout = 30

// LaunchdPlist renders the macOS unit.
func LaunchdPlist(binPath, addr, upstream string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + xmlEscape(LaunchdLabel) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range []string{
		binPath, "gateway",
		"--addr", addr,
		"--upstream", upstream,
		"--shutdown-grace", strconv.Itoa(StopTimeout) + "s",
	} {
		b.WriteString("    <string>" + xmlEscape(arg) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	// KeepAlive: a crashed gateway comes back. RunAtLoad so a login does not
	// leave the developer's first session ungoverned.
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	// launchd's default is 20s, below the gateway's own 30s grace, so a routine
	// restart mid-stream would be SIGKILLed before it finished draining.
	b.WriteString("  <key>ExitTimeOut</key>\n  <integer>" + strconv.Itoa(StopTimeout) + "</integer>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// SystemdUnit renders the Linux USER unit.
//
// A user unit, not a system one: the base tier is user-owned by definition, and a
// system unit would need root for an install that ADR-0016 puts in the
// developer's hands. An org hardening to the MDM tier deploys the same content
// root-owned — identical bytes, different ownership, which is exactly what doctor
// reads the tier from.
func SystemdUnit(binPath, addr, upstream string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=OpenBox local gateway (model-call governance)",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		fmt.Sprintf("ExecStart=%s gateway --addr %s --upstream %s --shutdown-grace %ds",
			binPath, addr, upstream, StopTimeout),
		"Restart=always",
		"RestartSec=2",
		// Must match --shutdown-grace: see StopTimeout.
		fmt.Sprintf("TimeoutStopSec=%d", StopTimeout),
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

// LaunchdPath is where the plist goes for a user-scope install.
func LaunchdPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// SystemdPath is where the user unit goes.
func SystemdPath(homeDir string) string {
	return filepath.Join(homeDir, ".config", "systemd", "user", SystemdUnitName)
}

// WriteUnit writes the unit for the given OS and returns its path.
//
// It does NOT load or start anything: loading is `launchctl`/`systemctl`, which
// the caller runs and reports on. Keeping the write separate from the load means a
// failure to start is distinguishable from a failure to configure — the same
// reason doctor separates "alive" from "actually used".
func WriteUnit(goos, homeDir, binPath, addr, upstream string) (string, error) {
	var path, body string
	switch goos {
	case "darwin":
		path, body = LaunchdPath(homeDir), LaunchdPlist(binPath, addr, upstream)
	case "linux":
		path, body = SystemdPath(homeDir), SystemdUnit(binPath, addr, upstream)
	default:
		// Windows daemon packaging is deferred (phase 07 requirement 7) and the
		// repo's posture there is build-verified only. An error, not a silent
		// no-op: a caller that thinks it installed a service and did not is worse
		// off than one told plainly.
		return "", fmt.Errorf("gatewayservice: no daemon packaging for %s yet — run `openbox gateway` in the foreground, or supervise it with the platform's own service manager", goos)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("gatewayservice: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("gatewayservice: writing %s: %w", path, err)
	}
	return path, nil
}

// RemoveUnit is the uninstall half.
func RemoveUnit(goos, homeDir string) (string, error) {
	var path string
	switch goos {
	case "darwin":
		path = LaunchdPath(homeDir)
	case "linux":
		path = SystemdPath(homeDir)
	default:
		return "", nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("gatewayservice: removing %s: %w", path, err)
	}
	return path, nil
}

// xmlEscape escapes a plist string value. A path can legitimately contain '&' or
// '<', and an unescaped one produces a plist launchd silently refuses to load —
// which would present as "the gateway never starts" with no error anywhere.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
