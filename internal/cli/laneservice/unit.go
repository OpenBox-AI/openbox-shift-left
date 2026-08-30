// Package laneservice renders and installs the OS supervisor unit for a per-
// developer OpenBox daemon. Lifecycle belongs to the OS supervisor, never to
// an openbox process: the unit runs the binary in the foreground, and a
// double-fork here would take the restart guarantee away from the thing that
// owns it.
package laneservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StopTimeout is the supervisor's stop timeout, and it must match the daemon's
// own --shutdown-grace. Http.Server.Shutdown never force-closes an active
// connection, so whatever is still streaming when the grace window expires is
// cut when the process exits.
const StopTimeout = 30

// Arg is one argv element plus whether a systemd ExecStart line must quote it.
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
	// Label is the launchd label, reverse-DNS, e.g.
	Label string
	// SystemdName is the systemd unit name without the ".service" suffix. The two
	// platforms do not share a naming convention and one shared value would
	// silently rename one of them.
	SystemdName string
	// DisplayName and ServiceDescription are what the supervisor shows.
	DisplayName        string
	ServiceDescription string
	// UnitDescription is the systemd [Unit] Description line.
	UnitDescription string
	// LogFile is the basename under ~/.openbox where stdio is kept.
	LogFile string
	// Args is the argv after the binary path.
	Args []Arg
}

// SystemdUnitName is the filename systemd looks for.
func (s Spec) SystemdUnitName() string { return s.SystemdName + ".service" }

// LogPath is where this lane's supervised stdio is kept.
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

// UnitPath is where this OS's unit lives, or "" where none is packaged.
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
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>ExitTimeOut</key>\n  <integer>" + strconv.Itoa(StopTimeout) + "</integer>\n")
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(s.LogPath(homeDir)) + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(s.LogPath(homeDir)) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// SystemdUnit renders the Linux user unit.
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
		fmt.Sprintf("TimeoutStopSec=%d", StopTimeout),
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

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
func (s Spec) WriteUnit(goos, homeDir, binPath string) (string, error) {
	var path, body string
	switch goos {
	case "darwin":
		path, body = s.LaunchdPath(homeDir), s.LaunchdPlist(homeDir, binPath)
	case "linux":
		path, body = s.SystemdPath(homeDir), s.SystemdUnit(binPath)
	default:
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

// RemoveUnit is the uninstall half.
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

func (s Spec) foregroundCommand() string {
	if len(s.Args) == 0 {
		return "openbox"
	}
	return "openbox " + s.Args[0].Value
}

// xmlEscape escapes a plist string value. A path can legitimately contain '&'
// or '<', and an unescaped one produces a plist launchd silently refuses to
// load; which presents as "the daemon never starts" with no error anywhere.
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
func systemdArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}
