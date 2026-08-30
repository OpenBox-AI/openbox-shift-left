// Package laneservice renders and installs the OS supervisor unit for a
// per-developer OpenBox daemon.
//
// It is the generalization of what shipped as the gateway's own unit writer.
// Three lanes now need a supervised loopback daemon — gateway, telemetry,
// transport — and this repo's stated original sin is that its engine was
// copy-pasted per adapter until the copies drifted, on the enforcement path.
// The unit body is where that drift would be least visible and most expensive:
// a lane whose plist forgot StandardErrorPath logs nowhere, and a lane whose
// ExitTimeOut does not match its own --shutdown-grace is SIGKILLed mid-drain on
// every routine restart. Neither shows up as an error.
//
// ── WHAT IS SHARED, AND WHAT EACH LANE STILL DECIDES ─────────────────────────
//
// Shared: both renderers, both paths, the write/remove pair, the stop timeout,
// KeepAlive/RunAtLoad, stdio capture, XML and systemd escaping.
//
// Per lane (Spec): the label, the unit name, the argv, the log filename and the
// two descriptions. Nothing else — a lane that needs more should say why.
//
// ── SUPERVISION IS THE AVAILABILITY STORY ────────────────────────────────────
//
// A crashed daemon restarts; a STOPPED one leaves its lane silent, which for the
// in-path lanes means model calls fail against a dead loopback port — the safe
// direction — and `openbox doctor` says which of the two happened. Lifecycle
// belongs to the OS supervisor, never to an openbox process: the unit runs the
// binary in the foreground, and a double-fork here would take the restart
// guarantee away from the thing that owns it.
package laneservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StopTimeout is the supervisor's stop timeout, and it MUST match the daemon's
// own --shutdown-grace.
//
// http.Server.Shutdown never force-closes an ACTIVE connection, so whatever is
// still streaming when the grace window expires is cut when the process exits.
// Exceeding the supervisor's own timeout buys nothing — launchd SIGKILLs at 20s
// by default, systemd at 90s — so the two numbers are chosen together. 30s sits
// inside systemd's default and above launchd's, which is why launchd's is set
// explicitly below rather than left to the default.
const StopTimeout = 30

// Arg is one argv element plus whether a systemd ExecStart line must quote it.
//
// Explicit rather than inferred. The rule that shipped quotes the three
// caller-supplied VALUES and leaves flag names and fixed literals bare, and a
// heuristic that tried to rediscover which is which would rewrite every existing
// unit file the first time it guessed differently. Quoting matters for two
// unrelated reasons — a '%' is a systemd SPECIFIER that gets expanded, and a
// space in $HOME breaks the line's parsing — and both produce a unit that
// launches with the wrong argv rather than an error.
type Arg struct {
	Value string
	Quote bool
}

// Literal is a subcommand, a flag name, or a value this package chose itself.
func Literal(v string) Arg { return Arg{Value: v} }

// Value is a caller-supplied value: a path, an address, a URL.
func Value(v string) Arg { return Arg{Value: v, Quote: true} }

// Spec is everything that differs between one supervised lane and another.
type Spec struct {
	// Label is the launchd label, reverse-DNS, e.g. "ai.openbox.gateway". It is
	// also the plist's filename stem.
	Label string
	// SystemdName is the systemd unit name WITHOUT the ".service" suffix. The
	// two platforms do not share a naming convention and one shared value would
	// silently rename one of them.
	SystemdName string
	// DisplayName and ServiceDescription are what the supervisor shows.
	DisplayName        string
	ServiceDescription string
	// UnitDescription is the systemd [Unit] Description line.
	UnitDescription string
	// LogFile is the basename under ~/.openbox where stdio is kept.
	LogFile string
	// Args is the argv AFTER the binary path.
	Args []Arg
}

// SystemdUnitName is the filename systemd looks for.
func (s Spec) SystemdUnitName() string { return s.SystemdName + ".service" }

// LogPath is where this lane's supervised stdio is kept.
//
// Under the OpenBox config dir rather than ~/Library/Logs: it is OpenBox state,
// `openbox doctor` can name one path on both platforms, and the directory
// already exists with the right permissions.
func (s Spec) LogPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", s.LogFile)
}

// LaunchdPath is where the plist goes for a user-scope install.
func (s Spec) LaunchdPath(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", s.Label+".plist")
}

// SystemdPath is where the user unit goes.
func (s Spec) SystemdPath(homeDir string) string {
	return filepath.Join(homeDir, ".config", "systemd", "user", s.SystemdUnitName())
}

// UnitPath is where this OS's unit lives, or "" where none is packaged. Exposed
// so an uninstall can unload the job BEFORE deleting the file the unload needs.
func (s Spec) UnitPath(goos, homeDir string) string {
	switch goos {
	case "darwin":
		return s.LaunchdPath(homeDir)
	case "linux":
		return s.SystemdPath(homeDir)
	default:
		return ""
	}
}

// Argv is the full argv including the binary path.
func (s Spec) Argv(binPath string) []string {
	argv := make([]string, 0, len(s.Args)+1)
	argv = append(argv, binPath)
	for _, a := range s.Args {
		argv = append(argv, a.Value)
	}
	return argv
}

// LaunchdPlist renders the macOS unit.
//
// homeDir is needed for the log path: launchd sends stdio to /dev/null unless
// told otherwise, and a lane's only signal that it is running perfectly and
// recording nothing goes to stderr.
func (s Spec) LaunchdPlist(homeDir, binPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + xmlEscape(s.Label) + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range s.Argv(binPath) {
		b.WriteString("    <string>" + xmlEscape(arg) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	// KeepAlive: a crashed daemon comes back. RunAtLoad so a login does not leave
	// the developer's first session unobserved.
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	// launchd's default is 20s, below the daemon's own 30s grace, so a routine
	// restart mid-stream would be SIGKILLed before it finished draining.
	b.WriteString("  <key>ExitTimeOut</key>\n  <integer>" + strconv.Itoa(StopTimeout) + "</integer>\n")
	// WHERE THE DIAGNOSTICS GO. launchd.plist(5) defaults both of these to
	// /dev/null and the systemd sibling gets the journal for free — so on macOS,
	// the platform that actually runs the KeepAlive daemon, every line written to
	// stderr was discarded. That is not cosmetic: each lane's throttled warnings
	// ("no developer DID configured", "no session header on relayed calls", "NOT
	// elected") are the ONLY signal that a perfectly working daemon is recording
	// nothing, and doctor reports configured/alive/bypass but never "is it
	// recording".
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(s.LogPath(homeDir)) + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(s.LogPath(homeDir)) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// SystemdUnit renders the Linux USER unit.
//
// A user unit, not a system one: the base tier is user-owned by definition, and
// a system unit would need root for an install ADR-0016 puts in the developer's
// hands. An org hardening to the MDM tier deploys the same content root-owned —
// identical bytes, different ownership, which is exactly what doctor reads the
// tier from.
func (s Spec) SystemdUnit(binPath string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=" + s.UnitDescription,
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + s.systemdExec(binPath),
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

// systemdExec renders the ExecStart line, quoting only what Arg says to quote.
func (s Spec) systemdExec(binPath string) string {
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, systemdArg(binPath))
	for _, a := range s.Args {
		if a.Quote {
			parts = append(parts, systemdArg(a.Value))
			continue
		}
		parts = append(parts, a.Value)
	}
	return strings.Join(parts, " ")
}

// WriteUnit writes the unit for the given OS and returns its path.
//
// NOT THE PRODUCTION PATH since D-OSS-3 — Reinstall goes through
// kardianos/service. This survives because it is the only way to assert the
// WRITTEN ARTIFACT at a caller-chosen location: the library derives the darwin
// plist path from user.Current() and ignores $HOME, so a test that used it would
// write a live launchd unit into the home of whoever ran `go test`. The bodies
// are identical either way — both come from the renderers above, and the
// supplied-template test pins that the library's render is an identity transform
// over them — so asserting this artifact asserts the library's too.
//
// It does NOT load or start anything. Keeping the write separate from the load
// is what makes "failed to configure" and "failed to start" distinguishable, the
// same reason doctor separates alive from actually used.
func (s Spec) WriteUnit(goos, homeDir, binPath string) (string, error) {
	var path, body string
	switch goos {
	case "darwin":
		path, body = s.LaunchdPath(homeDir), s.LaunchdPlist(homeDir, binPath)
	case "linux":
		path, body = s.SystemdPath(homeDir), s.SystemdUnit(binPath)
	default:
		// An error, not a silent no-op: a caller that thinks it installed a
		// service and did not is worse off than one told plainly.
		return "", s.UnsupportedPlatform(goos)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("laneservice: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("laneservice: writing %s: %w", path, err)
	}
	return path, nil
}

// RemoveUnit is the uninstall half. Absent is success.
func (s Spec) RemoveUnit(goos, homeDir string) (string, error) {
	path := s.UnitPath(goos, homeDir)
	if path == "" {
		return "", nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("laneservice: removing %s: %w", path, err)
	}
	return path, nil
}

// UnsupportedPlatform is the one wording for a platform with no daemon
// packaging, so every lane's refusal names the foreground command to run
// instead rather than leaving the developer with nothing.
func (s Spec) UnsupportedPlatform(goos string) error {
	return fmt.Errorf("laneservice: no daemon packaging for %s yet — run `%s` in the foreground, "+
		"or supervise it with the platform's own service manager", goos, s.foregroundCommand())
}

// foregroundCommand is the argv's subcommand, which is what a developer types.
func (s Spec) foregroundCommand() string {
	if len(s.Args) == 0 {
		return "openbox"
	}
	return "openbox " + s.Args[0].Value
}

// xmlEscape escapes a plist string value. A path can legitimately contain '&' or
// '<', and an unescaped one produces a plist launchd silently refuses to load —
// which presents as "the daemon never starts" with no error anywhere.
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
// argv — and a supervised service with the wrong argv looks like a daemon that
// does not work rather than like a config bug.
func systemdArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}
