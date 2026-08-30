package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

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
		fmt.Fprintf(a.stdout, "  lane %s did NOT come up — see the warning above; `openbox doctor` reports where this machine's model calls go.\n", lane)
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
			fmt.Fprintf(a.stdout, "\nRetiring the local gateway — the transport relay supersedes it\n")
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
		fmt.Fprintf(a.stdout, "\n  model-call producer: %s — %s\n", e.Elected, e.Reason)
	}
	return report
}

// laneRouted reports whether the tool's settings currently point at a lane.
// Through the election's own resolver, so the install path, doctor and the
// telemetry daemon cannot disagree about what "routed" means.
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
}

// runRemovals backs lanes out, in the reverse of install order. It runs before
// the credential gate, and that is a requirement rather than an optimization:
// removal must not require the thing being removed to still be usable.
func (a *app) runRemovals(home string, req removalRequest) int {
	fmt.Fprintf(a.stdout, "\nRemoving OpenBox lane configuration\n")
	var failures []string

	if req.transport {
		if err := a.removeTransport(home, req.force); err != nil {
			fmt.Fprintf(a.stderr, "warning: transport removal did not complete: %v\n", err)
			failures = append(failures, "transport")
		}
	}
	if req.gateway {
		if err := a.removeGateway(home); err != nil {
			fmt.Fprintf(a.stderr, "warning: gateway removal did not complete: %v\n", err)
			failures = append(failures, "gateway")
		}
	}
	if req.telemetry {
		if err := a.removeTelemetry(home, req.force); err != nil {
			fmt.Fprintf(a.stderr, "warning: telemetry removal did not complete: %v\n", err)
			failures = append(failures, "telemetry")
		}
	}

	if req.purge {
		a.purgeLaneData(home)
	}

	if len(failures) > 0 {
		return a.errorf("removal did not complete for: %v — the rest was removed. "+
			"A value that changed after OpenBox set it is not overwritten without --force-restore", failures)
	}
	fmt.Fprintf(a.stdout, "\nDone. `openbox doctor` reports what is left.\n")
	return exitOK
}

// purgeLaneData deletes the artifacts the lanes created. Nothing outside
// ~/.openbox is ever touched here; the settings file is restored by the
// activation record, key by key, and never truncated.
func (a *app) purgeLaneData(home string) {
	openboxHome, err := devconfig.Home()
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: cannot resolve the OpenBox config dir, so its artifacts were left in place: %v\n", err)
		return
	}
	caCert, caKey := transport.CAPaths(openboxHome)
	paths := []string{
		caCert, caKey,
		laneservice.Telemetry("", false).LogPath(home),
		laneservice.Transport("", false).LogPath(home),
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
	// never over-delete.
	spool := devconfig.SpoolDir(transportSpoolSubdir)
	if entries, err := os.ReadDir(spool); err == nil && len(entries) > 0 {
		fmt.Fprintf(a.stdout, "  kept           %s (%d undelivered event file(s))\n", spool, len(entries))
		fmt.Fprintf(a.stdout, "                 Shared with the hook path, which this command does not remove.\n")
		fmt.Fprintf(a.stdout, "                 Delete it by hand if you mean to discard that evidence.\n")
	}
}

type lanePlan struct {
	telemetry, transport             bool
	removeTelemetry, removeTransport bool
	purge                            bool
	telemetryAddr, transportAddr     string
}

func (a *app) printLanePlan(p lanePlan) {
	home := a.homeDir()
	settings := gatewayservice.SettingsPath(home)
	if p.telemetry {
		spec := laneservice.Telemetry(p.telemetryAddr, false)
		keys := activation.TelemetryKeys(p.telemetryAddr)
		fmt.Fprintf(a.stdout, "\nTelemetry receiver — PLANNED\n")
		fmt.Fprintf(a.stdout, "  unit         %s\n", orNoPackaging(spec.UnitPath(runtime.GOOS, home), home))
		fmt.Fprintf(a.stdout, "  listen       %s  (loopback only)\n", p.telemetryAddr)
		fmt.Fprintf(a.stdout, "  settings     %s  sets %d keys: %v\n", settings, len(keys), activation.KeyNames(keys))
		fmt.Fprintf(a.stdout, "               this makes the tool EXPORT its own telemetry, including prompt and\n")
		fmt.Fprintf(a.stdout, "               tool content, to a local receiver. What leaves this machine is still\n")
		fmt.Fprintf(a.stdout, "               gated by the content_capture posture.\n")
	}
	if p.transport {
		spec := laneservice.Transport(p.transportAddr, false)
		openboxHome, _ := devconfig.Home()
		caPath, _ := transport.CAPaths(openboxHome)
		keys := activation.TransportKeys(p.transportAddr, caPath, nil)
		fmt.Fprintf(a.stdout, "\nTransport relay — PLANNED\n")
		fmt.Fprintf(a.stdout, "  unit         %s\n", orNoPackaging(spec.UnitPath(runtime.GOOS, home), home))
		fmt.Fprintf(a.stdout, "  listen       %s  (loopback only)\n", p.transportAddr)
		fmt.Fprintf(a.stdout, "  CA           %s  (generated on first start)\n", caPath)
		fmt.Fprintf(a.stdout, "  settings     %s  sets %d keys: %v\n", settings, len(keys), activation.KeyNames(keys))
		fmt.Fprintf(a.stdout, "               this puts an OpenBox CA on this machine and INTERCEPTS the provider's\n")
		fmt.Fprintf(a.stdout, "               TLS. Every other host is tunnelled uninspected.\n")
	}
	if p.removeTelemetry || p.removeTransport {
		fmt.Fprintf(a.stdout, "\nRemoving lane configuration — PLANNED\n")
		for _, lane := range []struct {
			on   bool
			name string
			spec laneservice.Spec
		}{
			{p.removeTransport, "transport", laneservice.Transport("", false)},
			{p.removeTelemetry, "telemetry", laneservice.Telemetry("", false)},
		} {
			if !lane.on {
				continue
			}
			fmt.Fprintf(a.stdout, "  %-12s %s  (stopped and removed); its env keys restored from %s\n",
				lane.name, orNoPackaging(lane.spec.UnitPath(runtime.GOOS, home), home), activation.RecordPath(home))
		}
	}
	if p.purge {
		openboxHome, _ := devconfig.Home()
		caCert, _ := transport.CAPaths(openboxHome)
		fmt.Fprintf(a.stdout, "  DELETES      %s, the lane logs and %s\n", caCert, activation.RecordPath(home))
		fmt.Fprintf(a.stdout, "  KEEPS        the spool at %s — it is shared with the hook path,\n", devconfig.SpoolDir(transportSpoolSubdir))
		fmt.Fprintf(a.stdout, "               which this command does not remove.\n")
	}
}

func orNoPackaging(path, home string) string {
	if path != "" {
		return path
	}
	_ = home
	return "(no daemon packaging on " + runtime.GOOS + ")"
}

func laneLogPath(spec laneservice.Spec, home string) string {
	return filepath.Join(home, ".openbox", spec.LogFile)
}
