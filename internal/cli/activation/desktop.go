package activation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DesktopCoverage is what can be said without knowing how to route it: DETECTION
// only, and COVERAGE rather than "bypass", because an unconfigured machine looks
// identical to an evaded one. docs/coverage.md §1b.
type DesktopCoverage struct {
	Supported bool
	Running   bool
	// PresenceUnknown: no connections AND presence undetermined, not a finding.
	PresenceUnknown  bool
	DirectToProvider bool
	ThroughRelay     bool
	Note             string
}

func (d DesktopCoverage) Routed() bool {
	return d.Running && d.ThroughRelay && !d.DirectToProvider
}

// Describe is one line for `openbox doctor`.
func (d DesktopCoverage) Describe(relayAddr string) string {
	switch {
	case !d.Supported:
		return fmt.Sprintf("cannot be inspected on %s; whether the desktop app is governed here is "+
			"unknown, not confirmed", runtime.GOOS)
	case d.PresenceUnknown:
		return "holds no provider connections, and whether it is open could not be " +
			"established here, so this says nothing about its coverage either way"
	case !d.Running:
		return "not running, so there is nothing to say about its coverage"
	case d.Routed():
		return fmt.Sprintf("running and reaching the provider through %s", relayAddr)
	case d.DirectToProvider:
		return fmt.Sprintf("running and NOT routed through the relay: it holds its own connections " +
			"to a provider. Environment routing binds at process start, so restart it after " +
			"`openbox init`. This is a coverage gap, not evidence of an attempt to avoid governance")
	}
	return "running, and holding no provider connections right now, so its routing cannot be " +
		"determined from here"
}

type connLister func(ctx context.Context, process string) ([]string, error)

type presenceCheck func(ctx context.Context, process string) (running, known bool)

const desktopProcess = "Claude"

func InspectDesktop(ctx context.Context, relayPort string) DesktopCoverage {
	return inspectDesktopWith(ctx, relayPort, lsofConns, pgrepPresent)
}

func inspectDesktopWith(ctx context.Context, relayPort string, list connLister, present presenceCheck) DesktopCoverage {
	if runtime.GOOS != "darwin" || list == nil {
		return DesktopCoverage{}
	}
	conns, err := list(ctx, desktopProcess)
	if err != nil {
		// A check that cannot run says so; never that the surface is fine.
		return DesktopCoverage{Supported: true, Note: err.Error()}
	}
	d := DesktopCoverage{Supported: true}
	if len(conns) > 0 {
		d.Running = true
	} else if running, known := askPresence(ctx, present); known {
		d.Running = running
	} else {
		d.PresenceUnknown = true
	}
	for _, remote := range conns {
		switch {
		case isLoopbackEndpoint(remote):
			if strings.HasSuffix(remote, ":"+relayPort) {
				d.ThroughRelay = true
			}
		case strings.HasSuffix(remote, ":443"):
			d.DirectToProvider = true
		}
	}
	return d
}

// isLoopbackEndpoint reduces a host:port through netip like isLoopbackURL: all of
// 127.0.0.0/8 is loopback, so a prefix test would misread 127.0.0.2.
func isLoopbackEndpoint(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func lsofConns(ctx context.Context, process string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-a",
		"-c", process, "-iTCP", "-sTCP:ESTABLISHED")
	out, err := cmd.Output()
	if err != nil {
		// lsof exits 1 with no output when nothing MATCHES: "not running", not a
		// fault. Everything else IS a fault -- a missing binary, a denied permission
		// and this deadline all produce empty output, and calling those "not running"
		// is the false negative this check exists to remove.
		var exit *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("listing the desktop app's connections timed out")
		}
		return nil, fmt.Errorf("listing the desktop app's connections: %w", err)
	}
	return parseLsofRemotes(string(out)), nil
}

func parseLsofRemotes(out string) []string {
	var remotes []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[len(fields)-1]
		if strings.HasPrefix(name, "(") && len(fields) > 1 {
			name = fields[len(fields)-2]
		}
		local, remote, ok := strings.Cut(name, "->")
		if !ok || local == "" || remote == "" {
			continue
		}
		remotes = append(remotes, remote)
	}
	return remotes
}

func askPresence(ctx context.Context, present presenceCheck) (running, known bool) {
	if present == nil {
		return false, false
	}
	return present(ctx, desktopProcess)
}

// pgrepPresent asks only whether the process exists; exit 1 IS the answer here.
func pgrepPresent(ctx context.Context, process string) (running, known bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pgrep", "-x", process).Output()
	if err == nil {
		return len(strings.TrimSpace(string(out))) > 0, true
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}
