package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// laneCapable reports whether a provider has model-call lanes at all. Claude
// Code gets both lanes (telemetry via its own settings, transport as an
// in-path relay of the Anthropic Messages API). Codex gets telemetry (it reads
// its own config.toml) and, where a system PAC exists (macOS), the same relay,
// which it reaches through the PAC rather than an env block -- see
// hasTransportArm. Muse is hooks-only: no lane has been
// shown to see its model calls (no documented OTel exporter, and no proof it
// follows the system PAC or trusts the relay's CA), so it has none.
func laneCapable(name string) bool {
	switch provider.Name(name) {
	case provider.ClaudeCode, provider.Codex:
		return true
	default:
		return false
	}
}

type laneRequest struct {
	telemetry, transport         bool
	telemetryAddr, transportAddr string
	verbose                      bool
	// provider selects which activation this request performs when telemetry
	// installs: Claude Code writes settings.json env keys, Codex writes an
	// owned config.toml [otel] block. "" defaults to Claude Code so an older
	// caller (there is only one) is unaffected.
	provider string
}

type laneReport struct {
	installed []string
	failed    []string
	retired   []string
	// keys is every env key the installed lanes wrote, and settings the one
	// file they wrote them to. Summarized rather than reported per lane: on a
	// success the reader needs the total and the file, and `openbox doctor`
	// has the rest.
	keys     int
	settings string
	// addrs is each installed lane's address, for the one summary row.
	addrs map[string]string
	// systemPAC is the transport lane's own system PAC/CA-trust outcome
	// (systempac.go), zero-value (Class == "") whenever the transport lane
	// was not installed at all or never reached its own activate step.
	systemPAC activation.Outcome
	// codex is whether this install was Codex's, which changes what the system
	// PAC disclosure and its fallback line say.
	codex bool
}

// row prints one label/value line of the install report. The whole report is
// this column plus prose indented under it, so the width lives here rather
// than in a format string per call site.
func (a *app) row(label, format string, args ...any) {
	fmt.Fprintf(a.stdout, "  %-12s %s\n", label, fmt.Sprintf(format, args...))
}

// note prints prose under the rows it explains, one pre-wrapped line per
// argument, at the row indent rather than the value column: it is a sentence
// about the section, not another value.
func (a *app) note(lines ...string) {
	for _, line := range lines {
		fmt.Fprintf(a.stdout, "  %s\n", line)
	}
}

// detail is note one level deeper, for prose that explains the single line
// above it rather than the whole section.
func (a *app) detail(lines ...string) {
	for _, line := range lines {
		fmt.Fprintf(a.stdout, "    %s\n", line)
	}
}

// wrapRow is row for a value whose length is not knowable when the format
// string is written -- a joined reason, a probe's description. The first line
// carries the label and the rest align under it, so the column holds.
func (a *app) wrapRow(label, format string, args ...any) {
	lines := wrapAt(fmt.Sprintf(format, args...), rowValueWidth)
	if len(lines) == 0 {
		return
	}
	a.row(label, "%s", lines[0])
	for _, line := range lines[1:] {
		a.row("", "%s", line)
	}
}

// rowValueWidth keeps a wrapped value inside 78 columns once the label column
// in front of it is counted.
const rowValueWidth = 63

// wrapAt breaks text on spaces. Prose written here is wrapped by hand, where
// the wording can be chosen to fit; this is for the values that arrive as one
// string from somewhere else.
func wrapAt(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// print is the whole of a successful lane install: what is running, where its
// keys went, and the two things that capture means. The unit paths, the CA and
// the election are facts about a healthy machine, so they belong to `openbox
// doctor`; only a lane that did NOT come up explains itself here.
func (r laneReport) print(a *app) {
	for _, note := range r.retired {
		a.row("retired", "%s", note)
	}
	if len(r.installed) > 0 {
		a.row("lanes", "%s (running)", strings.Join(r.running(), ", "))
		a.row("env", "%d lane keys in %s", r.keys, r.settings)
		// The one thing an install must never leave to doctor: routing these
		// lanes is what turns capture on, and that is a decision only the
		// person reading this can make. Said per lane that actually came up --
		// the two fail independently, and claiming TLS interception for a
		// transport lane that did not start would be a false disclosure.
		a.note(r.captureNotes()...)
		a.note("What leaves this machine is gated by content_capture.")
		r.printSystemPAC(a)
	}
	for _, lane := range r.failed {
		a.row("NOT UP", "%s; see the warning above. `openbox doctor` reports where this", lane)
		a.row("", "machine's model calls go.")
	}
}

// captureNotes is what each installed lane means for the developer's own data:
// one disclosure per lane that is actually running, pre-wrapped.
func (r laneReport) captureNotes() []string {
	var out []string
	for _, lane := range r.installed {
		switch lane {
		case "telemetry":
			out = append(out,
				"The tool EXPORTS its own telemetry to that receiver, prompt and tool",
				"content included.")
		case "transport":
			out = append(out,
				"The relay INTERCEPTS the provider's TLS on this machine; every other host",
				"is tunnelled uninspected.")
		}
	}
	return out
}

// running pairs each installed lane with the address it answers on, which is
// the one coordinate a reader needs before doctor.
func (r laneReport) running() []string {
	out := make([]string, 0, len(r.installed))
	for _, lane := range r.installed {
		out = append(out, lane+" "+r.addrs[lane])
	}
	return out
}

func (a *app) setupLanes(req laneRequest) laneReport {
	var report laneReport
	report.codex = provider.Name(req.provider) == provider.Codex
	if !req.telemetry && !req.transport {
		return report
	}
	// A platform with no daemon packaging has nothing to install, and saying a
	// lane "did NOT come up" there is false in the direction that matters: that
	// wording is reserved for a lane that should be running and is not, and
	// printing it on every install teaches the reader to skip it.
	if laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, "x") == "" {
		fmt.Fprintf(a.stdout, "\nModel-call lanes: not packaged for %s; hooks only.\n", runtime.GOOS)
		a.note("Tool calls are still governed. To observe model calls here, run `openbox",
			"telemetry` and `openbox transport` in the foreground, or supervise them with",
			"the platform's own service manager.")
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

	// The gateway is Claude Code's base-URL relay, so only Claude Code's install
	// retires it; Codex's never touches that env block.
	codex := provider.Name(req.provider) == provider.Codex
	if req.transport && !codex {
		if value, present := gatewayservice.CurrentEnv(home); present && laneRouted(home, activation.LaneGateway) {
			fmt.Fprintf(a.stdout, "\nRetiring the local gateway - the transport relay supersedes it\n")
			if err := a.removeGateway(home); err != nil {
				fmt.Fprintf(a.stderr, "warning: could not retire the gateway at %s: %v\n", value, err)
			} else {
				report.retired = append(report.retired, "the local gateway ("+value+"), superseded by the in-path transport relay")
			}
		}
	}

	report.settings = gatewayservice.SettingsPath(home)
	report.addrs = map[string]string{}

	installTelemetry := func() {
		if !req.telemetry {
			return
		}
		setup := a.setupTelemetry
		if codex {
			setup = a.setupCodexTelemetry
		}
		keys, err := setup(home, req.telemetryAddr, req.verbose)
		if err != nil {
			fmt.Fprintf(a.stderr, "warning: telemetry setup did not complete: %v\n", err)
			report.failed = append(report.failed, "telemetry")
			return
		}
		report.installed = append(report.installed, "telemetry")
		report.addrs["telemetry"] = req.telemetryAddr
		report.keys += keys
	}
	installTransport := func() {
		if !req.transport {
			return
		}
		keys, err := a.setupTransportFor(provider.Name(req.provider), home, req.transportAddr, req.verbose)
		if err != nil {
			fmt.Fprintf(a.stderr, "warning: transport setup did not complete: %v\n", err)
			report.failed = append(report.failed, "transport")
			return
		}
		report.installed = append(report.installed, "transport")
		report.addrs["transport"] = req.transportAddr
		report.keys += keys
		report.systemPAC = a.lastSystemPACOutcome
	}

	if !(codex && req.transport) {
		installTelemetry()
		// The PAC commit inside the relay's install lists whatever the derived set
		// holds, Codex included, whichever provider is being installed. With Codex
		// in it, an older telemetry daemon left running would keep recording the
		// calls the relay is about to record too, so the commit waits for telemetry
		// to have come up on this binary.
		a.withholdSystemPAC = req.transport && req.telemetry && !slices.Contains(report.installed, "telemetry") &&
			hasProvider(derivedTransportProviders(home, provider.Name(req.provider)), provider.Codex)
		defer func() { a.withholdSystemPAC = false }()
		installTransport()
		return report
	}

	// Codex with a proxy arm. The order is what keeps one Codex call recorded
	// once across an upgrade, and each step is a precondition of the next:
	//  1. the relay's unit, carrying the record path, is started and proven
	//     listening, while nothing yet lists Codex, so it records nothing;
	//  2. the telemetry unit is rewritten and RESTARTED onto this binary, the
	//     one that knows to stand down once the relay is elected. An older
	//     telemetry daemon left running past the commit below would keep
	//     recording every call the relay now also records;
	//  3. only then the system PAC: priors recorded as Pending, the CA trusted
	//     and read back, the PAC written, the record committed. The election
	//     flips per record once the relay has also seen a Codex call.
	installTransport()
	installTelemetry()
	switch {
	case !slices.Contains(report.installed, "transport"):
		// Nothing to point the system at, and the failure was reported above.
	case !slices.Contains(report.installed, "telemetry"):
		fmt.Fprintf(a.stderr, "warning: the system PAC was not activated for Codex: the telemetry "+
			"lane did not come up on this binary, and an older telemetry daemon would keep "+
			"recording the calls the relay is about to record too. Fix the telemetry lane and re-run "+
			"`openbox init --provider codex`; Codex's model calls stay with the telemetry lane\n")
	default:
		report.systemPAC = a.activateCodexSystemPAC(home, req.transportAddr)
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

	// First, before any daemon, PAC or unit is touched: commit the removal of
	// Codex from the system-PAC record. The election reads that record, so
	// from here Codex's model calls are telemetry's again, and nothing below
	// (the relay stopping, the PAC being restored) can leave them with no
	// recorder.
	if req.transport {
		a.retireCodexProxyArm(home)
	}

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
			// System PAC deactivation (restore scopes, untrust, delete cert by
			// SHA-1) BEFORE purgeLaneData deletes the CA files: the
			// trust-before-PAC ordering, reversed on the way out, requires
			// untrusting a CA before its files are gone. A machine that never activated it has no record
			// and this is a no-op.
			keepRecord := a.deactivateSystemPAC(home)
			if req.uninstall {
				traceUninstallStep("system-pac-revert", nil, map[string]any{"kept_record": keepRecord})
			}
			a.purgeLaneData(home, req.uninstall, keepRecord)
		} else {
			a.row("kept", "the activation record and the CA: a lane is still routed, and")
			a.row("", "that record is the only thing that can restore its env keys.")
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
// keepRecord leaves the activation record in place (the CA files still go:
// a trusted certificate without its key cannot mint anything). It is set
// when the system proxy or CA trust could not be restored, because the
// record is then the only description of what is still on the machine.
func (a *app) purgeLaneData(home string, uninstalling, keepRecord bool) {
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
	}
	if keepRecord {
		a.row("kept", "%s: the system proxy and CA trust were not restored;", activation.RecordPath(home))
		a.row("", "re-run `openbox uninstall` to retry, or run the commands above.")
	} else {
		paths = append(paths, activation.RecordPath(home))
	}
	for _, path := range paths {
		if !fileExists(path) {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not delete %s: %v\n", path, err)
			continue
		}
		a.row("deleted", "%s", path)
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
		a.row("kept", "%s (%d undelivered event file(s))", spool, len(entries))
		a.row("", "Shared with the hook path, which this command does not remove.")
		a.row("", "Delete it by hand if you mean to discard that evidence.")
	}
}

// laneLogPath is where a lane's supervised stdio is kept, used by doctor. Not
// laneservice.Spec.LogPath: doctor asks about a lane it may not have a full
// Spec for.
func laneLogPath(spec laneservice.Spec, home string) string {
	return filepath.Join(home, ".openbox", spec.LogFile)
}
