package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// laneCapable reports whether a provider has model-call lanes at all. The
// telemetry receiver and the transport relay observe the Anthropic Messages API
// through Claude Code's own settings, so there is nothing for them to read on
// any other provider.
func laneCapable(name string) bool { return provider.Name(name) == provider.ClaudeCode }

type laneRequest struct {
	telemetry, transport         bool
	telemetryAddr, transportAddr string
	verbose                      bool
}

type laneReport struct {
	installed []string
	failed    []string
	retired   []string
}

func (r laneReport) print(a *app) {
	for _, note := range r.retired {
		fmt.Fprintf(a.stdout, "  retired: %s\n", note)
	}
	if len(r.installed) > 0 {
		for _, lane := range r.installed {
			fmt.Fprintf(a.stdout, "  lane %s is installed and running.\n", lane)
		}
	}
	for _, lane := range r.failed {
		fmt.Fprintf(a.stdout, "  lane %s did NOT come up; see the warning above; `openbox doctor` reports where this machine's model calls go.\n", lane)
	}
	if len(r.installed) > 0 {
		fmt.Fprintf(a.stdout, "  note: the tool reads these settings at SESSION START, so a session that is\n")
		fmt.Fprintf(a.stdout, "        already open keeps using whatever it started with. Restart it.\n")
	}
}

func (a *app) setupLanes(req laneRequest) laneReport {
	var report laneReport
	if !req.telemetry && !req.transport {
		return report
	}
	// A platform with no daemon packaging has nothing to install, and saying a
	// lane "did NOT come up" there is false in the direction that matters: that
	// wording is reserved for a lane that should be running and is not, and
	// printing it on every install teaches the reader to skip it.
	if laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, "x") == "" {
		fmt.Fprintf(a.stdout, "\nModel-call lanes: not packaged for %s; hooks only.\n", runtime.GOOS)
		fmt.Fprintf(a.stdout, "  Tool calls are still governed. To observe model calls here, run `openbox\n")
		fmt.Fprintf(a.stdout, "  telemetry` and `openbox transport` in the foreground, or supervise them with\n")
		fmt.Fprintf(a.stdout, "  the platform's own service manager.\n")
		return report
	}
	home, code := a.gatewayHome()
	if code != exitOK {
		if req.telemetry {
			report.failed = append(report.failed, "telemetry")
		}
		if req.transport {
			report.failed = append(report.failed, "transport")
		}
		return report
	}

	if req.transport {
		if value, present := gatewayservice.CurrentEnv(home); present && laneRouted(home, activation.LaneGateway) {
			fmt.Fprintf(a.stdout, "\nRetiring the local gateway; the transport relay supersedes it\n")
			if err := a.removeGateway(home); err != nil {
				fmt.Fprintf(a.stderr, "warning: could not retire the gateway at %s: %v\n", value, err)
			} else {
				report.retired = append(report.retired, "the local gateway ("+value+"), superseded by the in-path transport relay")
			}
		}
	}

	if req.telemetry {
		fmt.Fprintf(a.stdout, "\nTelemetry receiver (the tool's own OTLP exports)\n")
		if err := a.setupTelemetry(home, req.telemetryAddr, req.verbose); err != nil {
			fmt.Fprintf(a.stderr, "warning: telemetry setup did not complete: %v\n", err)
			report.failed = append(report.failed, "telemetry")
		} else {
			report.installed = append(report.installed, "telemetry")
		}
	}

	if req.transport {
		fmt.Fprintf(a.stdout, "\nTransport relay (in-path model-call observation)\n")
		if err := a.setupTransport(home, req.transportAddr, req.verbose); err != nil {
			fmt.Fprintf(a.stderr, "warning: transport setup did not complete: %v\n", err)
			report.failed = append(report.failed, "transport")
		} else {
			report.installed = append(report.installed, "transport")
		}
	}

	e := activation.ResolveElection(gatewayservice.SettingsPath(home))
	if e.Elected != "" {
		fmt.Fprintf(a.stdout, "\n  model-call producer: %s; %s\n", e.Elected, e.Reason)
	}
	return report
}

// laneRouted through the election's own resolver, so the install path, doctor
// and the telemetry daemon cannot disagree about what "routed" means.
func laneRouted(home string, lane activation.Lane) bool {
	for _, r := range activation.ResolveElection(gatewayservice.SettingsPath(home)).Routed {
		if r == lane {
			return true
		}
	}
	return false
}

type removalRequest struct {
	gateway, telemetry, transport bool
	purge                         bool
	force                         bool
	// uninstall marks the caller as `openbox uninstall`, which is the only
	// caller there is now. It survives because purgeLaneData still needs to know
	// that the spool is being deleted by the same run rather than kept.
	uninstall bool
}

// removalResult is what actually happened, so a caller can say something true
// about the residue instead of inferring it from an exit code.
type removalResult struct {
	// failed names the lanes that did not come down.
	failed []string
	// stillRouted is true when a lane is left routed AND running: its
	// deactivate refused, so removeLane returned before unloading the unit.
	// That is a different residue from a lane whose unit could not be deleted,
	// and only this one keeps intercepting model calls.
	stillRouted bool
}

func (r removalResult) ok() bool { return len(r.failed) == 0 }

// runRemovals backs lanes out, in the reverse of install order. It runs before
// the credential gate, and that is a requirement rather than an optimization:
// removal must not require the thing being removed to still be usable.
func (a *app) runRemovals(home string, req removalRequest) removalResult {
	fmt.Fprintf(a.stdout, "\nRemoving OpenBox lane configuration\n")
	var res removalResult

	for _, lane := range []struct {
		on     bool
		label  string
		remove func() error
	}{
		{req.transport, "transport", func() error { return a.removeTransport(home, req.force) }},
		{req.gateway, "gateway", func() error { return a.removeGateway(home) }},
		{req.telemetry, "telemetry", func() error { return a.removeTelemetry(home, req.force) }},
	} {
		if !lane.on {
			continue
		}
		err := lane.remove()
		if err == nil {
			continue
		}
		// A platform with no daemon packaging has no unit to remove, so the
		// refusal the renderer raises is "nothing to do" here, not a failure --
		// and reporting it as one told a Windows operator their lanes were still
		// running on a machine that cannot run them.
		if laneservice.IsNotInstalled(err) || isUnsupportedPlatform(err) {
			continue
		}
		fmt.Fprintf(a.stderr, "warning: %s removal did not complete: %v\n", lane.label, err)
		res.failed = append(res.failed, lane.label)
		if isActivationConflict(err) {
			res.stillRouted = true
		}
	}

	// Purge only once every lane is down. A conflicted deactivate leaves the
	// daemon loaded and its env keys routed, and the activation record is the
	// only thing that can restore those keys: deleting it here left the machine
	// intercepting model calls with no record of what to put back, and a re-run
	// reporting a clean machine. The CA goes with it, because a deleted CA turns
	// a live transport lane into a total TLS failure rather than a removable one.
	if req.purge {
		if res.ok() {
			a.purgeLaneData(home, req.uninstall)
		} else {
			fmt.Fprintf(a.stdout, "  kept           the activation record and the CA: a lane is still routed, and\n")
			fmt.Fprintf(a.stdout, "                 that record is the only thing that can restore its env keys.\n")
		}
	}

	if !res.ok() {
		// No command accepts a flag that would overwrite a changed value, so the
		// remedy is the only one there is: resolve it by hand and run again.
		a.errorf("removal did not complete for: %v; the rest was removed. "+
			"A value that changed after OpenBox set it was left alone; resolve it, then re-run.",
			res.failed)
	}
	return res
}

// isActivationConflict distinguishes the one failure that leaves a lane routed
// and running from every other way a removal can fail.
func isActivationConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "refusing to overwrite")
}

// isUnsupportedPlatform matches the renderer's refusal for an OS with no daemon
// packaging. Matched on the message because it is a bare formatted error.
func isUnsupportedPlatform(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no daemon packaging for")
}

// purgeLaneData deletes the artifacts the lanes created. Nothing outside
// ~/.openbox is ever touched here; the settings file is restored by the
// activation record, key by key, and never truncated.
func (a *app) purgeLaneData(home string, uninstalling bool) {
	openboxHome, err := devconfig.Home()
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: cannot resolve the OpenBox config dir, so its artifacts were left in place: %v\n", err)
		return
	}
	caCert, caKey := transport.CAPaths(openboxHome)
	paths := []string{
		caCert, caKey,
		laneservice.Telemetry("", "", false).LogPath(home),
		laneservice.Transport("", "", false).LogPath(home),
		gatewayservice.LogPath(home),
		activation.RecordPath(home),
	}
	for _, path := range paths {
		if !fileExists(path) {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not delete %s: %v\n", path, err)
			continue
		}
		fmt.Fprintf(a.stdout, "  deleted        %s\n", path)
	}
	// This repo's stated direction of error for exactly this shape is over-keep,
	// never over-delete. `uninstall` is the one caller that does delete it, and
	// it reports the flush and the deletion itself — so saying "kept" here would
	// be false, and saying "deleted" would report it twice.
	if uninstalling {
		return
	}
	spool := devconfig.SpoolDir(transportSpoolSubdir)
	if entries, err := os.ReadDir(spool); err == nil && len(entries) > 0 {
		fmt.Fprintf(a.stdout, "  kept           %s (%d undelivered event file(s))\n", spool, len(entries))
		fmt.Fprintf(a.stdout, "                 Shared with the hook path, which this command does not remove.\n")
		fmt.Fprintf(a.stdout, "                 Delete it by hand if you mean to discard that evidence.\n")
	}
}

// laneLogPath is where a lane's supervised stdio is kept, used by doctor. Not
// laneservice.Spec.LogPath: doctor asks about a lane it may not have a full
// Spec for.
func laneLogPath(spec laneservice.Spec, home string) string {
	return filepath.Join(home, ".openbox", spec.LogFile)
}
