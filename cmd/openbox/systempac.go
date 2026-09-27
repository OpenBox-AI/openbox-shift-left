package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// This file is the WIRING half of the system PAC/CA-trust activation
// (phase-03-system-pac-activation-and-ca-trust.md, macOS arm): the library
// itself (internal/cli/activation) never decides WHEN to run, only HOW. Every
// call into it goes through the four seams below, so a test can make the
// whole step a no-op (fakeSupervisor, newLaneHarness) or drive it against a
// scripted argv recorder without ever shelling out to sudo, networksetup or
// security.
var (
	activateSystemPACFn   = activation.ActivateSystemPAC
	deactivateSystemPACFn = activation.DeactivateSystemPAC
	liveSystemPACFn       = activation.LiveSystemPAC
	// systemPACRunner is the Runner every one of the three functions above is
	// handed. Production is activation.ExecRunner; a test overrides this
	// instead of (or as well as) the three function seams when it wants the
	// REAL darwin arm exercised against a fake argv recorder.
	systemPACRunner activation.Runner = activation.ExecRunner
)

// transportSystemPACProviders is the union System PAC activation records for
// the transport lane. Claude Code only, today: Codex never gets a transport
// lane at all (initlanes.go's laneRequest.transport is CC-only, per
// plan.md's "Codex rows stay out of the union" ruling), so there is exactly
// one entry until that changes.
var transportSystemPACProviders = []string{string(provider.ClaudeCode)}

// systemPACURL is the relay's own PAC endpoint. Never "localhost": some
// clients resolve that name differently than the loopback IP the relay
// actually listens on (matches the library's own Plan.PACURL doc).
func systemPACURL(addr string) string { return "http://" + addr + "/proxy.pac" }

// runSystemPACActivation is called from setupTransport's own activate
// closure, strictly AFTER activation.Activate has written the transport
// lane's env keys -- so a failure here never rolls back a working, env-routed
// lane: the PAC write is a second, later activation step layered onto an
// already-successful lane, not a precondition for it. It is never reached at
// all when the listen check failed or the lane rolled back, because
// setupLane only calls activate() once readiness is proven.
func (a *app) runSystemPACActivation(homeDir, addr, caPath string) activation.Outcome {
	pacURL := systemPACURL(addr)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return activation.Outcome{Class: activation.Failed,
			Reason: fmt.Sprintf("reading the CA certificate at %s: %v", caPath, err)}
	}
	if entry, active := systemPACAlreadyActive(homeDir, pacURL, caPEM); active {
		return activation.Outcome{Class: activation.Activated, Entry: entry}
	}
	outcome, err := activateSystemPACFn(context.Background(), systemPACRunner, activation.Plan{
		HomeDir:   homeDir,
		PACURL:    pacURL,
		CAPath:    caPath,
		CAPEM:     caPEM,
		Providers: transportSystemPACProviders,
	})
	if err != nil {
		return activation.Outcome{Class: activation.Failed, Reason: err.Error()}
	}
	return outcome
}

// systemPACAlreadyActive is the second-invocation skip: a second `init` must
// not prompt again when nothing about this machine's activation has
// changed. The library itself always authorizes before doing anything, so
// this check lives here, in the wiring, not in internal/cli/activation. It
// returns the loaded entry (nil on any read error, alongside false) so the
// caller never has to re-load it itself. Active requires all of:
//   - a record already claiming Activated for this exact PAC URL;
//   - the recorded CA fingerprint matching the CA CURRENTLY on disk -- a
//     legacy-CA re-issue (internal/transport) changes the file's content
//     under the same path without updating this record itself, and skipping
//     this check would leave the new CA silently untrusted while this record
//     and `doctor` both keep reporting a healthy activation;
//   - a LIVE read confirming every enabled scope still points at us;
//   - that same fingerprint still present in the System keychain's trust
//     store -- a hand-removed trust entry must not read as still active.
func systemPACAlreadyActive(homeDir, pacURL string, caPEM []byte) (*activation.SystemEntry, bool) {
	entry, err := activation.LoadSystemEntry(homeDir)
	if err != nil || entry == nil || !entry.PACActivated || entry.PACURL != pacURL {
		return entry, false
	}
	if entry.CATrust == nil || entry.CATrust.SHA1 == "" {
		return entry, false
	}
	currentSHA1, err := activation.SHA1Fingerprint(caPEM)
	if err != nil || currentSHA1 != entry.CATrust.SHA1 {
		return entry, false
	}
	live, err := liveSystemPACFn(context.Background(), systemPACRunner)
	if err != nil || len(live) == 0 {
		return entry, false
	}
	for _, scope := range live {
		if !scope.URLPresent || scope.URL != pacURL {
			return entry, false
		}
	}
	present, err := activation.CATrustPresent(context.Background(), systemPACRunner, currentSHA1)
	if err != nil || !present {
		return entry, false
	}
	return entry, true
}

// deactivateSystemPAC restores the system PAC and untrusts the CA before the
// caller (runRemovals) lets purgeLaneData delete the CA files
// entry.CATrust names -- the ordering CLAUDE.md's "trust-before-PAC
// ordering" requires: untrusting a CA whose files are already gone can never
// work. A machine that never activated system PAC (LoadSystemEntry returns
// nil) never calls the Runner seam at all.
//
// A declined or failed deactivate leaves the OS-side trust/PAC settings as
// they are; the CA key is still deleted right after this returns (by
// purgeLaneData), because a trusted cert with no matching key cannot mint
// anything new, even though the System keychain may still list a now-
// dangling trust entry -- the printed manual command (with its SHA-1) is how
// a developer clears that entry by hand.
//
// It reports whether the system record must be KEPT: true when the proxy
// settings or the CA trust were not restored, so the record is still the
// only description of what is left on the machine. doctor reads it, and a
// second `openbox uninstall` retries from it.
func (a *app) deactivateSystemPAC(home string) (keepRecord bool) {
	entry, err := activation.LoadSystemEntry(home)
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: could not read the system PAC record: %v\n", err)
		return true
	}
	if entry == nil {
		return false
	}
	report, runErr := deactivateSystemPACFn(context.Background(), systemPACRunner, *entry)
	switch {
	case runErr != nil:
		a.printSystemPACDeactivateFailed(entry, runErr.Error(), nil)
		return true
	case report.Class != "":
		reason := string(report.Class)
		if report.Reason != "" {
			reason += ": " + report.Reason
		}
		a.printSystemPACDeactivateFailed(entry, reason, report.Manual)
		return true
	default:
		for _, svc := range report.Restored {
			a.row("restored", "system proxy for %s", svc)
		}
		for _, svc := range report.Drift {
			a.row("drift", "%s's auto-proxy URL is not ours; left alone", svc)
		}
		if err := activation.ClearSystemEntry(home); err != nil {
			fmt.Fprintf(a.stderr, "warning: could not clear the system PAC record: %v\n", err)
		}
		return false
	}
}

// printSystemPACDeactivateFailed is the exact-manual-commands-with-SHA-1
// report line CLAUDE.md's uninstall wiring requires. manual is the library's
// own command list when it has one (declined/failed at the authorize stage);
// the fallback below (built from entry's own recorded fields) covers the one
// shape the library does not populate Manual for: a hard error partway
// through deactivation, after authorization succeeded. Both shapes are
// printed here, before purgeLaneData deletes the CA files this entry names,
// but pasted by a developer AFTER that deletion has already happened -- so
// neither may print `remove-trusted-cert -d <path>`, which needs a file that
// will already be gone; `delete-certificate -Z <sha1> <keychain>` matches by
// fingerprint alone and needs no file.
func (a *app) printSystemPACDeactivateFailed(entry *activation.SystemEntry, reason string, manual []string) {
	a.row("system PAC", "NOT restored (%s); the OS-side proxy settings and/or CA trust are left", reason)
	a.row("", "as they are. Its CA key is deleted below regardless, so a leaked trust entry")
	a.row("", "cannot mint anything new; clear the dangling entry by hand:")
	if len(manual) > 0 {
		for _, cmd := range manual {
			a.row("", "  %s", cmd)
		}
		return
	}
	if entry.CATrust != nil {
		a.row("", "  sudo security delete-certificate -Z %s %s", entry.CATrust.SHA1, entry.CATrust.Keychain)
	}
}

// reissueLegacyCAIfNeeded is step (a): called BEFORE the transport unit is
// (re)installed, so a legacy constrained CA is replaced before anything
// trusts or serves it under the old, constrained shape. Idempotent
// (transport.ReissueIfNeeded), and reports exactly once, only when it
// actually reissued something.
func (a *app) reissueLegacyCAIfNeeded() error {
	openboxHome, err := devconfig.Home()
	if err != nil {
		return err
	}
	before, err := transport.LoadOrCreateCA(openboxHome)
	if err != nil {
		return err
	}
	if !transport.CANeedsReissue(before) {
		return nil
	}
	if _, err := transport.ReissueIfNeeded(openboxHome); err != nil {
		return err
	}
	a.row("CA re-issued", "the legacy constrained CA was replaced with an unconstrained one;")
	a.row("", "restart Claude Code (and any other tool trusting it) to pick up the new certificate")
	return nil
}

// printSystemPAC is the init report's own disclosure block, called from
// laneReport.print only when the transport lane actually installed (the
// system PAC step is never attempted for a telemetry-only or Codex-only
// install). r.systemPAC.Class == "" means the step was never even reached
// (setupTransport itself failed before calling activate, or telemetry-only).
func (r laneReport) printSystemPAC(a *app) {
	if r.systemPAC.Class == "" {
		return
	}
	switch {
	case r.systemPAC.Class == activation.Activated:
		a.printSystemPACActive(r.systemPAC)
	case len(r.systemPAC.Manual) == 0:
		// The unsupported-OS arm: no manual commands exist because the shapes
		// are unmeasured on this OS, and a guessed command is worse than none.
		a.row("system PAC", "not activated (not yet supported on %s in this build); Claude Code", runtime.GOOS)
		a.row("", "stays env-routed; desktop apps and browsers are not covered")
	default:
		a.printSystemPACNotActive(r.systemPAC)
	}
}

// printSystemPACActive is the disclosure required whenever activation is
// ACTIVE: the exact wording CLAUDE.md's install-report requirement names,
// item by item (which hosts, the unconstrained-CA blast radius, `; DIRECT`,
// the sudo/trust prompt).
func (a *app) printSystemPACActive(o activation.Outcome) {
	url := ""
	if o.Entry != nil {
		url = o.Entry.PACURL
	}
	a.row("system PAC", "ACTIVE; %s", url)
	a.note(
		"Desktop apps and browser sessions on this machine's governed hosts now pass",
		"through a local relay that decrypts them with an OpenBox-held key.",
		"A claude.ai chat (browser or the Claude app) is RECORDED as its own session,",
		"one per conversation, prompt and reply included under content_capture.",
		"The CA is unconstrained: a leaked ~/.openbox key could mint a certificate for",
		"any site this Mac trusts it for. File mode 0600 is the protection;",
		"`openbox uninstall` removes it.",
		"`; DIRECT` in the PAC means a stopped relay lets that traffic through",
		"uninspected -- this is not egress control.",
		"You were asked for your sudo password once; macOS may have asked once more",
		"to confirm the trust change.",
	)
}

// printSystemPACNotActive covers NotAttempted/Declined/Failed on darwin,
// where the library DOES have manual commands to print. The extended
// disclosure paragraph only prints for Declined/Failed: NotAttempted means
// nothing was even asked (no controlling terminal), so nothing was disclosed
// or risked yet.
func (a *app) printSystemPACNotActive(o activation.Outcome) {
	reason := string(o.Class)
	if o.Reason != "" {
		reason += ": " + o.Reason
	}
	a.row("system PAC", "NOT activated (%s); Claude Code stays env-routed; run these to finish:", reason)
	for _, cmd := range o.Manual {
		a.row("", "  %s", cmd)
	}
	if o.Class == activation.NotAttempted {
		return
	}
	a.note(
		"Once activated, desktop apps and browser sessions on this machine's governed",
		"hosts would pass through a local relay that decrypts them with an",
		"OpenBox-held key. The CA is unconstrained: a leaked ~/.openbox key could mint",
		"a certificate for any site this Mac trusts it for. File mode 0600 is the",
		"protection; `openbox uninstall` removes it. `; DIRECT` in the PAC means a",
		"stopped relay lets that traffic through uninspected -- this is not egress",
		"control. You will be asked for your sudo password once; macOS may ask once",
		"more to confirm the trust change.",
	)
}

// reportSystemPAC is `openbox doctor`'s system-PAC row. LiveSystemPAC is
// read-only and asks for no elevation, but it still shells out to
// networksetup -- so this only calls it when a SystemEntry record exists,
// i.e. this machine attempted activation at least once. A fresh machine (and
// every one of this package's own test fixtures, none of which create a
// SystemEntry) never reaches the exec call.
func (a *app) reportSystemPAC() {
	home := a.homeDir()
	entry, err := activation.LoadSystemEntry(home)
	if err != nil {
		fmt.Fprintf(a.stdout, "\nSystem PAC (desktop apps and browsers, macOS only)\n")
		a.row("record", "unreadable: %v", err)
		return
	}
	if entry == nil {
		return
	}
	fmt.Fprintf(a.stdout, "\nSystem PAC (desktop apps and browsers, macOS only)\n")
	state := systemPACRecordState(*entry)
	a.row("record", "%s", state)
	traceDoctorFinding("system-pac:record", state, entry.PACURL)
	if entry.CATrust != nil {
		trusted := "unknown (no SHA-1 recorded)"
		if entry.CATrust.SHA1 != "" {
			trusted = "yes (SHA-1 " + entry.CATrust.SHA1 + ")"
		}
		a.row("cert trusted", "%s", trusted)
	}
	live, err := liveSystemPACFn(context.Background(), systemPACRunner)
	if err != nil {
		a.row("live read", "FAILED: %v", err)
		return
	}
	if len(live) == 0 {
		a.row("live read", "no enabled network services found")
		return
	}
	recorded := map[string]bool{}
	for _, sc := range entry.Scopes {
		recorded[sc.Service] = true
	}
	for _, scope := range live {
		state := "off"
		switch {
		case scope.URLPresent && entry.PACURL != "" && scope.URL == entry.PACURL:
			state = "ours"
		case scope.URLPresent && scope.Enabled:
			state = "not ours"
		}
		note := ""
		if entry.PACActivated && recorded[scope.Service] && state != "ours" {
			note = "  DRIFT: recorded as ours, live value is not"
		}
		a.row(scope.Service, "%s%s", state, note)
	}
}

// systemPACRecordState renders SystemEntry's four-outcome-plus-pending shape
// as one word (or a short phrase), for doctor's "record" row.
func systemPACRecordState(entry activation.SystemEntry) string {
	switch {
	case entry.Pending:
		return "PENDING (an interrupted run; re-run `openbox init` to reconcile)"
	case entry.PACActivated:
		return "activated"
	case entry.Declined:
		return "declined"
	case entry.Failed:
		return "failed: " + entry.Reason
	default:
		return "not attempted"
	}
}
