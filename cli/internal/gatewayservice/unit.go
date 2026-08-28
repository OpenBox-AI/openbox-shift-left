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

// verboseFlag is the one spelling of the flag. Both renderers reference it, and
// TestBothUnitsCarryVerboseOnlyWhenAsked holds them together — the two platforms
// drifting on whether a supervised gateway logs would be invisible until someone
// tried to debug the one that does not.
const verboseFlag = "--verbose"

// gatewayArgv is the launchd argv.
//
// --verbose belongs in the UNIT, not only on a hand-started relay. The daemon owns
// the port, so without this the flag is reachable only by stopping the supervised
// job and racing it for the port — which is exactly how a developer ends up unable
// to answer "is anything flowing through this at all?". stdio already goes to
// ~/.openbox/gateway.log, so the lines land somewhere tailable.
func gatewayArgv(binPath, addr, upstream string, verbose bool) []string {
	argv := []string{
		binPath, "gateway",
		"--addr", addr,
		"--upstream", upstream,
		"--shutdown-grace", strconv.Itoa(StopTimeout) + "s",
	}
	if verbose {
		argv = append(argv, verboseFlag)
	}
	return argv
}

// LaunchdPlist renders the macOS unit.
//
// homeDir is needed for the log paths: launchd sends stdio to /dev/null unless
// told otherwise, and the gateway's only signal that it is recording nothing goes
// to stderr.
func LaunchdPlist(homeDir, binPath, addr, upstream string, verbose bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + xmlEscape(LaunchdLabel) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range gatewayArgv(binPath, addr, upstream, verbose) {
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
	// WHERE THE DIAGNOSTICS GO. launchd.plist(5) defaults both of these to
	// /dev/null, and the systemd sibling gets the journal for free — so on macOS,
	// the platform that actually runs the KeepAlive daemon, every line the gateway
	// writes to stderr was discarded.
	//
	// That is not cosmetic. The emitter's own design rests on those lines: "every
	// path that declines to emit is a governance gap, and a gap nobody is told
	// about is indistinguishable from a working gateway". Its two throttled
	// warnings — no developer DID configured, and no session header on relayed
	// calls — are the ONLY signal that a perfectly working relay is recording
	// nothing, and `openbox doctor` reports configured/alive/tier/bypass but never
	// "is it recording". A developer who ran `init --gateway` before `openbox
	// auth` had no way to find out.
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(LogPath(homeDir)) + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(LogPath(homeDir)) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// LogPath is where a supervised gateway's stdio is kept.
//
// Under the OpenBox config dir rather than ~/Library/Logs: it is OpenBox state,
// `openbox doctor` can name one path on both platforms, and the directory already
// exists with the right permissions.
func LogPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "gateway.log")
}

// SystemdUnit renders the Linux USER unit.
//
// A user unit, not a system one: the base tier is user-owned by definition, and a
// system unit would need root for an install that ADR-0016 puts in the
// developer's hands. An org hardening to the MDM tier deploys the same content
// root-owned — identical bytes, different ownership, which is exactly what doctor
// reads the tier from.
func SystemdUnit(binPath, addr, upstream string, verbose bool) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=OpenBox local gateway (model-call governance)",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		// QUOTED. The macOS sibling escapes its argv and this did not: a binPath
		// containing a space (an $HOME with one is ordinary) breaks systemd's line
		// parsing, and a '%' in --upstream — plausible under the proxy and
		// egress-control setups the MDM recipe documents — is a systemd specifier
		// that gets expanded rather than passed through.
		"ExecStart=" + systemdExec(binPath, addr, upstream, verbose),
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

// systemdExec renders the ExecStart line.
//
// Only the three VALUES are quoted, deliberately: the macOS sibling escapes its
// argv and this once did not — a binPath containing a space (an $HOME with one is
// ordinary) breaks systemd's line parsing, and a '%' in --upstream, plausible
// under the proxy and egress-control setups the MDM recipe documents, is a systemd
// specifier that gets expanded rather than passed through. Flag NAMES need no
// quoting, and quoting them would rewrite every existing unit file for nothing.
func systemdExec(binPath, addr, upstream string, verbose bool) string {
	line := fmt.Sprintf("%s gateway --addr %s --upstream %s --shutdown-grace %ds",
		systemdArg(binPath), systemdArg(addr), systemdArg(upstream), StopTimeout)
	if verbose {
		line += " " + verboseFlag
	}
	return line
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
// NOT THE PRODUCTION PATH since D-OSS-3 — `Install`/`Reinstall` in service.go are,
// and they go through kardianos/service. This survives because it is the only way
// to assert the WRITTEN ARTIFACT at a caller-chosen location: the library derives
// the darwin plist path from `user.Current()` and ignores `$HOME`, so a test that
// used it would write a live launchd unit into whoever ran it. The bodies are
// identical either way — both come from the renderers above, and
// TestSuppliedTemplatesSurviveRendering pins that the library's template render is
// an identity transform over them — so asserting this artifact asserts the
// library's too.
//
// It does NOT load or start anything: loading is `launchctl`/`systemctl`, which
// the caller runs and reports on. Keeping the write separate from the load means a
// failure to start is distinguishable from a failure to configure — the same
// reason doctor separates "alive" from "actually used".
func WriteUnit(goos, homeDir, binPath, addr, upstream string, verbose bool) (string, error) {
	var path, body string
	switch goos {
	case "darwin":
		path, body = LaunchdPath(homeDir), LaunchdPlist(homeDir, binPath, addr, upstream, verbose)
	case "linux":
		path, body = SystemdPath(homeDir), SystemdUnit(binPath, addr, upstream, verbose)
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

// UnitPath is where this OS's unit lives, or "" where none is packaged. Exposed so
// an uninstall can unload the job BEFORE deleting the file the unload needs.
func UnitPath(goos, homeDir string) string {
	switch goos {
	case "darwin":
		return LaunchdPath(homeDir)
	case "linux":
		return SystemdPath(homeDir)
	default:
		return ""
	}
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

// systemdArg quotes an argument for a systemd ExecStart line.
//
// Two escapes matter and they are not the same: '%' is a systemd SPECIFIER
// introducer and is escaped by doubling it, while quotes and backslashes are
// shell-ish quoting inside the double-quoted form. Getting only one of them
// produces a unit that either fails to parse or silently launches with the wrong
// argv — and a supervised service with the wrong argv looks like a gateway that
// does not work rather than like a config bug.
func systemdArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}
