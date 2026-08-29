package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/activation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/laneservice"
	"github.com/openbox-ai/openbox-shift-left/transport"
)

// initlanes.go is `init`'s multi-lane orchestration: what `--full` installs,
// what `--remove-all` takes away, and what each reports when only half of it
// worked.
//
// ── PARTIAL FAILURE IS THE NORMAL CASE, NOT THE EXCEPTION ────────────────────
//
// Installing two supervised daemons and rewriting a settings file has more ways
// to half-succeed than to fail outright. So every lane is installed
// independently, a failure in one leaves the others working, and the closing
// report states exactly which lanes are in force — because the exit code is 0
// either way and stdout is all a fleet script has to go on.

// laneRequest is what one `init` run asked for.
type laneRequest struct {
	telemetry, transport         bool
	telemetryAddr, transportAddr string
	verbose                      bool
}

// laneReport is what actually happened, printed at the very end so it is the
// last thing a developer reads.
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
		// NOT silent, and NOT phrased as success. A lane that did not come up is
		// a governance gap, and a gap nobody is told about is indistinguishable
		// from a working lane.
		fmt.Fprintf(a.stdout, "  lane %s did NOT come up — see the warning above; `openbox doctor` reports where this machine's model calls go.\n", lane)
	}
	if len(r.installed) > 0 {
		fmt.Fprintf(a.stdout, "  note: the tool reads these settings at SESSION START, so a session that is\n")
		fmt.Fprintf(a.stdout, "        already open keeps using whatever it started with. Restart it.\n")
	}
}

// setupLanes installs the requested lanes, in precedence order.
func (a *app) setupLanes(req laneRequest) laneReport {
	var report laneReport
	if !req.telemetry && !req.transport {
		return report
	}
	home, code := a.gatewayHome()
	if code != exitOK {
		// gatewayHome already printed why. Recording the lanes as FAILED rather
		// than returning an empty report is the difference between "you asked for
		// nothing" and "what you asked for did not happen" — and the exit code is
		// 0 either way, so stdout is all a fleet script has to go on.
		if req.telemetry {
			report.failed = append(report.failed, "telemetry")
		}
		if req.transport {
			report.failed = append(report.failed, "transport")
		}
		return report
	}

	// The transport lane SUPERSEDES the gateway: both observe the same call, and
	// the in-path relay outranks the loopback base-URL one. Leaving both routed
	// is not a double-count — the client reaches only one of them — but it does
	// leave a developer with two supervised daemons and an election they did not
	// choose, so the weaker one is retired and SAID rather than left to be found.
	if req.transport {
		// ROUTED, not "elected". Reading the election here would make the retire
		// depend on a ranking that is itself a function of what is routed — and
		// under the base-URL correction a routed gateway always outranks the
		// relay, so the two happen to agree today. They would stop agreeing the
		// moment either rule moved, and this check is about the machine's state,
		// not about the ranking.
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
			// NOT fatal to the whole install: hooks are already in place and
			// governing tool calls. Reporting this as a total failure would tell a
			// developer to undo work that succeeded.
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

	// The election is DERIVED, so it is now whatever the settings say — including
	// after a partial failure. Printing it here means the developer sees the same
	// answer doctor would give, on the same run that changed it.
	e := activation.ResolveElection(gatewayservice.SettingsPath(home))
	if e.Elected != "" {
		fmt.Fprintf(a.stdout, "\n  model-call producer: %s — %s\n", e.Elected, e.Reason)
	}
	return report
}

// laneRouted reports whether the tool's settings currently point at a lane.
//
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

// ── removal ──────────────────────────────────────────────────────────────────

// removalRequest is what `--remove-all` and the per-lane removal flags ask for.
type removalRequest struct {
	gateway, telemetry, transport bool
	// purge additionally deletes the CA, the logs and the spool — data
	// destruction by design, so it is named rather than implied.
	purge bool
	force bool
}

// runRemovals backs lanes out, in the reverse of install order.
//
// It runs BEFORE the credential gate, and that is a requirement rather than an
// optimization: removal must not require the thing being removed to still be
// usable. `--remove-gateway` once sat below that gate, so a machine whose
// credentials had been deleted — an offboarding, a rotation, a wiped ~/.openbox
// — could not unset ANTHROPIC_BASE_URL at all: every model call kept failing
// closed against a dead loopback port, and the only remaining fix was
// hand-editing the tool's settings file.
//
// Tolerant of partial state throughout: a lane that was never installed is not
// an error, and one lane's failure must not stop the others being removed. The
// worst outcome here is an orphaned KeepAlive daemon nobody was told about.
func (a *app) runRemovals(home string, req removalRequest) int {
	fmt.Fprintf(a.stdout, "\nRemoving OpenBox lane configuration\n")
	var failures []string

	// Reverse of the install order: the strongest lane first, so a machine that
	// is only partly cleaned is left with the WEAKER observation rather than a
	// stronger one nobody expects.
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

// purgeLaneData deletes the artifacts the lanes created.
//
// DATA DESTRUCTION BY DESIGN, so each path is printed as it goes. Nothing
// outside ~/.openbox is ever touched here — the settings file is restored by the
// activation record, key by key, and never truncated.
//
// The CA is included deliberately: leaving a trusted signing key on the machine
// after the relay that used it is gone is a strictly worse posture than removing
// it, and it is regenerated on the next install.
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
	// THE SPOOL IS DELIBERATELY NOT DELETED, against this phase's own requirement
	// text, because that text contradicts its own security constraint — "never
	// delete anything outside ~/.openbox/ and the managed keys".
	//
	// Two independent reasons, and either alone settles it. The spool resolves
	// from os.UserConfigDir() (~/.config/openbox/…), which is outside ~/.openbox.
	// And it is SHARED with the hook path: `--remove-all` removes lanes, not
	// hooks, so deleting it would destroy undelivered governed tool-call evidence
	// belonging to a component that is still installed and still running. This
	// repo's stated direction of error for exactly this shape is over-keep, never
	// over-delete.
	//
	// Named rather than silent, because a developer expecting a full teardown
	// should know what survived and where it is.
	spool := devconfig.SpoolDir(transportSpoolSubdir)
	if entries, err := os.ReadDir(spool); err == nil && len(entries) > 0 {
		fmt.Fprintf(a.stdout, "  kept           %s (%d undelivered event file(s))\n", spool, len(entries))
		fmt.Fprintf(a.stdout, "                 Shared with the hook path, which this command does not remove.\n")
		fmt.Fprintf(a.stdout, "                 Delete it by hand if you mean to discard that evidence.\n")
	}
}

// ── dry run ──────────────────────────────────────────────────────────────────

// lanePlan describes what a --dry-run would do to the lanes.
type lanePlan struct {
	telemetry, transport             bool
	removeTelemetry, removeTransport bool
	purge                            bool
	telemetryAddr, transportAddr     string
}

// printLanePlan names the daemons and the env keys, because those are the
// largest-blast-radius things this command does and an operator vetting a fleet
// script needs them named rather than counted.
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

// orNoPackaging names the unit this OS would write, or says none is packaged.
func orNoPackaging(path, home string) string {
	if path != "" {
		return path
	}
	_ = home
	return "(no daemon packaging on " + runtime.GOOS + ")"
}

// laneLogPath is where a lane's supervised stdio lands, for doctor.
func laneLogPath(spec laneservice.Spec, home string) string {
	return filepath.Join(home, ".openbox", spec.LogFile)
}
