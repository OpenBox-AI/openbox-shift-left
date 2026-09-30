package activation

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// traceActivationStep is this package's own copy of cmd/openbox's
// stepsrecord.go shape: internal/trace has zero repo imports on purpose (see
// its package doc), so a caller injects rather than this package importing
// anything back into cmd/openbox. Kept minimal -- Stage/Outcome/Detail is all
// a reconciler needs -- and named distinctly from cmd/openbox's traceActivation
// so a reader never mistakes this for a shared symbol across packages.
func traceActivationStep(step string, err error, prior any, detail map[string]any) {
	d := map[string]any{"step": step}
	for k, v := range detail {
		d[k] = v
	}
	if prior != nil {
		d["prior"] = prior
	}
	rec := trace.Record{Stage: trace.StageActivation, Detail: d}
	if err != nil {
		rec.Outcome = "failed"
		rec.Err = err.Error()
	} else {
		rec.Outcome = "ok"
	}
	trace.Emit(rec)
}

// systemKeychainPath is where every write and read-back in this file targets
// trust: a root shell with no GUI session cannot change trust settings
// there, but `sudo` from the TTY `openbox init` already runs in can -- which
// is why every privileged call here goes through `sudo -n`, never a re-exec
// as root outright.
const systemKeychainPath = "/Library/Keychains/System.keychain"

// caCommonName is this CA's Subject CommonName (internal/transport/ca.go),
// used only to narrow `find-certificate -c` before this file re-checks the
// SHA-1 itself -- never the sole match, because another tool's root cert
// (mitmproxy's, on at least one machine this was checked against) can share
// a keychain and the same-ish Common Name convention.
const caCommonName = "OpenBox Transport CA (local)"

// geteuidFn and openControllingTTY are seams: production is os.Geteuid and a
// real `/dev/tty` open, and a test overrides either to reach a class this
// package cannot otherwise force from CI (root, or no controlling terminal)
// without actually being root or detached from one.
var geteuidFn = os.Geteuid

var openControllingTTY = func() error {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// activateSystemPACDarwin is the macOS arm. Order matters and is asserted by
// tests: preflight -> authorize (one prompt) -> record priors as Pending ->
// trust the CA -> read back the trust -> per scope, PAC url then state then
// read-back, rolling back everything already applied (in reverse) and
// untrusting the CA the moment any step fails.
func activateSystemPACDarwin(ctx context.Context, run Runner, plan Plan) (Outcome, error) {
	if err := validateActivatePlan(plan); err != nil {
		return Outcome{}, err
	}

	class, reason, ok := authorizeDarwin(ctx, run)
	if !ok {
		return recordPreflightOutcome(plan, class, reason)
	}

	services, err := listEnabledServicesDarwin(ctx, run)
	if err != nil {
		return recordPreflightOutcome(plan, Failed, fmt.Sprintf("listing network services: %v", err))
	}
	live := map[string]ScopeState{}
	for _, svc := range services {
		state, err := getAutoProxyDarwin(ctx, run, svc)
		if err != nil {
			return recordPreflightOutcome(plan, Failed,
				fmt.Sprintf("reading current auto-proxy state for %s: %v", svc, err))
		}
		live[svc] = state
	}

	existing, err := LoadSystemEntry(plan.HomeDir)
	if err != nil {
		return Outcome{}, err
	}
	scopes := reconcileScopes(existing, services, live, plan.PACURL)

	sha1, err := sha1Fingerprint(plan.CAPEM)
	if err != nil {
		return Outcome{}, err
	}

	entry := &SystemEntry{
		Schema:    systemEntrySchema,
		Pending:   true,
		PACURL:    plan.PACURL,
		Providers: plan.Providers,
		Scopes:    scopes,
		CATrust:   &CATrustState{SHA1: sha1, CAPath: plan.CAPath, Keychain: systemKeychainPath},
	}
	// Record before write: this is on disk before the very first privileged
	// command below runs, so a process killed mid-run always leaves a trail a
	// later run can reconcile from instead of a silent gap.
	if err := persistSystemEntry(plan.HomeDir, entry); err != nil {
		return Outcome{}, err
	}

	// A prior run's own CA may have been re-issued (internal/transport's
	// legacy-constrained-CA migration) since it was trusted: same PACURL, same
	// filenames, different key and SHA-1. Re-issuing never touches system
	// trust itself, so a superseded fingerprint would otherwise sit in the
	// System keychain forever, untrackable once its own file is gone. Drop it
	// under this same authorization, before the new CA is trusted.
	if existing != nil && existing.CATrust != nil && existing.CATrust.SHA1 != "" && existing.CATrust.SHA1 != sha1 {
		if err := untrustOldCADarwin(ctx, run, *existing.CATrust); err != nil {
			return finalizeFailedDarwin(plan, entry,
				fmt.Sprintf("removing the superseded CA's trust (SHA-1 %s): %v", existing.CATrust.SHA1, err))
		}
	}

	var priorCASHA1 any
	if existing != nil && existing.CATrust != nil {
		priorCASHA1 = existing.CATrust.SHA1
	}
	if _, err := runWrite(ctx, run, "security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", systemKeychainPath, plan.CAPath); err != nil {
		traceActivationStep("ca-trust", err, priorCASHA1, map[string]any{"sha1": sha1})
		return finalizeFailedDarwin(plan, entry, fmt.Sprintf("trusting the CA: %v", err))
	}
	traceActivationStep("ca-trust", nil, priorCASHA1, map[string]any{"sha1": sha1})

	out, err := run(ctx, "security", "find-certificate", "-a", "-Z", "-c", caCommonName, systemKeychainPath)
	if err != nil || !findCertificateHasSHA1(out, sha1) {
		verifyErr := err
		if verifyErr == nil {
			verifyErr = fmt.Errorf("trust did not verify: SHA-1 %s not found in the System keychain", sha1)
		}
		traceActivationStep("ca-trust-readback", verifyErr, nil, map[string]any{"sha1": sha1})
		_, _ = runWrite(ctx, run, "security", "remove-trusted-cert", "-d", plan.CAPath)
		return finalizeFailedDarwin(plan, entry,
			"trust did not verify: the System keychain's SHA-1 does not match the CA just trusted")
	}
	traceActivationStep("ca-trust-readback", nil, nil, map[string]any{"sha1": sha1})

	applied := make([]ProxyScope, 0, len(scopes))
	for _, sc := range scopes {
		var priorURL any
		if sc.PriorURLPresent {
			priorURL = sc.PriorURL
		}
		if err := applyScopeDarwin(ctx, run, sc, plan.PACURL); err != nil {
			traceActivationStep("pac-write", err, priorURL, map[string]any{"service": sc.Service, "pac_url": plan.PACURL})
			rollbackScopesDarwin(ctx, run, applied)
			_, _ = runWrite(ctx, run, "security", "remove-trusted-cert", "-d", plan.CAPath)
			return finalizeFailedDarwin(plan, entry,
				fmt.Sprintf("activating the PAC for %s: %v", sc.Service, err))
		}
		traceActivationStep("pac-write", nil, priorURL, map[string]any{"service": sc.Service, "pac_url": plan.PACURL})
		applied = append(applied, sc)
	}

	entry.Pending = false
	entry.PACActivated = true
	now := time.Now().UTC()
	entry.ActivatedAt = now.Format(time.RFC3339)
	entry.ActivationID = now.Format(time.RFC3339Nano)
	if err := persistSystemEntry(plan.HomeDir, entry); err != nil {
		return Outcome{}, err
	}
	return Outcome{Class: Activated, Entry: entry}, nil
}

// deactivateSystemPACDarwin restores every scope not showing drift, then
// untrusts the CA, in that order -- so a caller may delete the CA files the
// instant this returns without error.
func deactivateSystemPACDarwin(ctx context.Context, run Runner, entry SystemEntry) (Report, error) {
	class, reason, ok := authorizeDarwin(ctx, run)
	if !ok {
		return Report{Class: class, Reason: reason, Manual: manualDeactivateCommandsDarwin(entry)}, nil
	}

	var restored, drift []string
	for _, sc := range entry.Scopes {
		if err := validateArgvSafe(sc.Service); err != nil {
			return Report{Restored: restored, Drift: drift}, err
		}
		liveState, err := getAutoProxyDarwin(ctx, run, sc.Service)
		if err != nil {
			return Report{Restored: restored, Drift: drift}, fmt.Errorf("activation: reading live state for %s: %w", sc.Service, err)
		}
		if entry.PACURL != "" && (!liveState.URLPresent || liveState.URL != entry.PACURL) {
			drift = append(drift, sc.Service)
			continue
		}
		var priorURL any
		if sc.PriorURLPresent {
			priorURL = sc.PriorURL
		}
		if err := restoreScopeDarwin(ctx, run, sc); err != nil {
			traceActivationStep("pac-restore", err, priorURL, map[string]any{"service": sc.Service})
			return Report{Restored: restored, Drift: drift}, fmt.Errorf("activation: restoring %s: %w", sc.Service, err)
		}
		traceActivationStep("pac-restore", nil, priorURL, map[string]any{"service": sc.Service})
		restored = append(restored, sc.Service)
	}

	if entry.CATrust != nil {
		if err := untrustOldCADarwin(ctx, run, *entry.CATrust); err != nil {
			traceActivationStep("ca-untrust", err, entry.CATrust.SHA1, nil)
			return Report{Restored: restored, Drift: drift}, fmt.Errorf("activation: removing the CA trust: %w", err)
		}
		traceActivationStep("ca-untrust", nil, entry.CATrust.SHA1, nil)
	}

	return Report{Restored: restored, Drift: drift}, nil
}

// untrustOldCADarwin drops one CA's trust from the System keychain, in the
// same authorized run that is about to trust its replacement (activate) or
// finish removing it for good (deactivate). A CA re-issue deletes the old
// certificate and key files but never touches system trust, so the file
// remove-trusted-cert needs to identify its target can already be gone by
// the time this runs; delete-certificate matches by SHA-1 alone and needs no
// file; on this machine it was confirmed to also drop the certificate's
// trust entry. So remove-trusted-cert only runs first when the file is still
// there, and delete-certificate always runs.
//
// Two more guards. A certificate that is not in the System keychain (a run
// killed before its trust write landed) has nothing to untrust, so nothing
// runs. And the CA filenames are constants, so after a re-issue the file at
// old.CAPath is the NEW, not-yet-trusted certificate: remove-trusted-cert on
// it fails, so it only runs when the file's own SHA-1 is the one being
// removed.
func untrustOldCADarwin(ctx context.Context, run Runner, old CATrustState) error {
	if err := validateArgvSafe(old.SHA1); err != nil {
		return err
	}
	present, err := catTrustPresentDarwin(ctx, run, old.SHA1)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if old.CAPath != "" {
		if err := validateArgvSafe(old.CAPath); err != nil {
			return err
		}
		if pem, readErr := os.ReadFile(old.CAPath); readErr == nil {
			if fileSHA1, fpErr := sha1Fingerprint(pem); fpErr == nil && fileSHA1 == old.SHA1 {
				if _, err := runWrite(ctx, run, "security", "remove-trusted-cert", "-d", old.CAPath); err != nil {
					return err
				}
			}
		}
	}
	_, err = runWrite(ctx, run, "security", "delete-certificate", "-Z", old.SHA1, systemKeychainPath)
	return err
}

// catTrustPresentDarwin is CATrustPresent's darwin arm: a read-only check of
// whether sha1 is currently trusted in the System keychain, independent of
// what any activation record claims.
func catTrustPresentDarwin(ctx context.Context, run Runner, sha1 string) (bool, error) {
	if err := validateArgvSafe(sha1); err != nil {
		return false, err
	}
	out, err := run(ctx, "security", "find-certificate", "-a", "-Z", "-c", caCommonName, systemKeychainPath)
	if err != nil {
		return false, err
	}
	return findCertificateHasSHA1(out, sha1), nil
}

func liveSystemPACDarwin(ctx context.Context, run Runner) ([]ScopeState, error) {
	services, err := listEnabledServicesDarwin(ctx, run)
	if err != nil {
		return nil, err
	}
	states := make([]ScopeState, 0, len(services))
	for _, svc := range services {
		state, err := getAutoProxyDarwin(ctx, run, svc)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

// authorizeDarwin is the "sudo once" sequence: euid 0 needs neither a
// terminal nor sudo; otherwise a controlling terminal must be reachable
// before anything is asked, one `sudo -v` covers the whole run (skipped
// when `-n -v` shows a cached credential already), and `-n true` right after
// confirms the cache actually works rather than having just prompted once
// for a policy that never caches.
func authorizeDarwin(ctx context.Context, run Runner) (class OutcomeClass, reason string, ok bool) {
	if geteuidFn() == 0 {
		return "", "", true
	}
	if err := openControllingTTY(); err != nil {
		return NotAttempted, "no controlling terminal available (/dev/tty); cannot prompt for elevation", false
	}
	if _, err := run(ctx, "sudo", "-n", "-v"); err != nil {
		if _, err := run(ctx, "sudo", "-v"); err != nil {
			return Declined, "the elevation prompt was declined", false
		}
	}
	if _, err := run(ctx, "sudo", "-n", "true"); err != nil {
		return Failed, "sudoers does not cache credentials", false
	}
	return "", "", true
}

// runWrite is every privileged write in this file: `sudo -n` in front of the
// real command, or the command run directly when already root. Reads
// (`-listallnetworkservices`, `-getautoproxyurl`, `find-certificate`) never
// go through this -- they need no privilege, and LiveSystemPAC's contract is
// that it never asks for any.
func runWrite(ctx context.Context, run Runner, name string, args ...string) ([]byte, error) {
	if geteuidFn() == 0 {
		return run(ctx, name, args...)
	}
	full := append([]string{"-n", name}, args...)
	return run(ctx, "sudo", full...)
}

// recordPreflightOutcome records an attempt that stopped before any
// privileged write. It never replaces a record that still owns live state
// (scopes written or a CA trusted by an earlier run): that record is the
// only thing uninstall can restore from, so a declined or failed re-run
// leaves it exactly as it was.
func recordPreflightOutcome(plan Plan, class OutcomeClass, reason string) (Outcome, error) {
	existing, err := LoadSystemEntry(plan.HomeDir)
	if err != nil {
		return Outcome{}, err
	}
	if existing != nil && (len(existing.Scopes) > 0 || existing.CATrust != nil) {
		return Outcome{Class: class, Reason: reason, Manual: manualCommandsDarwin(plan), Entry: existing}, nil
	}
	entry := minimalEntry(plan)
	entry.Declined = class == Declined
	entry.Failed = class == Failed
	entry.Reason = reason
	if err := persistSystemEntry(plan.HomeDir, entry); err != nil {
		return Outcome{}, err
	}
	return Outcome{Class: class, Reason: reason, Manual: manualCommandsDarwin(plan), Entry: entry}, nil
}

// finalizeFailedDarwin records a Failed outcome past the preflight stage
// (a trust write, its read-back, or a scope's PAC write). Manual carries the
// same completion recipe as a preflight Failed/Declined: whatever this run
// touched, it either rolled back before reaching here or never applied, so
// finishing activation from scratch is always the correct recovery.
func finalizeFailedDarwin(plan Plan, entry *SystemEntry, reason string) (Outcome, error) {
	entry.Pending = false
	entry.Failed = true
	entry.PACActivated = false
	entry.Reason = reason
	entry.ActivatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := persistSystemEntry(plan.HomeDir, entry); err != nil {
		return Outcome{}, err
	}
	return Outcome{Class: Failed, Reason: reason, Manual: manualCommandsDarwin(plan), Entry: entry}, nil
}

func minimalEntry(plan Plan) *SystemEntry {
	return &SystemEntry{
		Schema:      systemEntrySchema,
		PACURL:      plan.PACURL,
		Providers:   plan.Providers,
		ActivatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func validateActivatePlan(plan Plan) error {
	if plan.HomeDir == "" {
		return fmt.Errorf("activation: Plan.HomeDir is empty")
	}
	if plan.PACURL == "" {
		return fmt.Errorf("activation: Plan.PACURL is empty")
	}
	if plan.CAPath == "" {
		return fmt.Errorf("activation: Plan.CAPath is empty")
	}
	if len(plan.CAPEM) == 0 {
		return fmt.Errorf("activation: Plan.CAPEM is empty")
	}
	if err := validateArgvSafe(plan.PACURL); err != nil {
		return err
	}
	return validateArgvSafe(plan.CAPath)
}

// applyScopeDarwin sets, then confirms, one service's PAC url and state.
func applyScopeDarwin(ctx context.Context, run Runner, sc ProxyScope, pacURL string) error {
	if err := validateArgvSafe(sc.Service); err != nil {
		return err
	}
	if _, err := runWrite(ctx, run, "networksetup", "-setautoproxyurl", sc.Service, pacURL); err != nil {
		return err
	}
	if _, err := runWrite(ctx, run, "networksetup", "-setautoproxystate", sc.Service, "on"); err != nil {
		return err
	}
	state, err := getAutoProxyDarwin(ctx, run, sc.Service)
	if err != nil {
		return err
	}
	if !state.URLPresent || state.URL != pacURL || !state.Enabled {
		return fmt.Errorf("read-back did not confirm the PAC URL (got url=%q present=%v enabled=%v)",
			state.URL, state.URLPresent, state.Enabled)
	}
	return nil
}

// restoreScopeDarwin puts one service back as closely as networksetup
// allows: a prior `(null)` URL can only be turned off, because networksetup
// refuses to write an empty URL; a real prior URL is written back and then
// its own enabled state restored, on or off.
func restoreScopeDarwin(ctx context.Context, run Runner, sc ProxyScope) error {
	if err := validateArgvSafe(sc.Service); err != nil {
		return err
	}
	if !sc.PriorURLPresent {
		_, err := runWrite(ctx, run, "networksetup", "-setautoproxystate", sc.Service, "off")
		return err
	}
	if _, err := runWrite(ctx, run, "networksetup", "-setautoproxyurl", sc.Service, sc.PriorURL); err != nil {
		return err
	}
	_, err := runWrite(ctx, run, "networksetup", "-setautoproxystate", sc.Service, onOff(sc.PriorEnabled))
	return err
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func rollbackScopesDarwin(ctx context.Context, run Runner, applied []ProxyScope) {
	for i := len(applied) - 1; i >= 0; i-- {
		// Best-effort: a rollback failure cannot un-fail an already-Failed
		// outcome, and there is nowhere further to escalate to here. The
		// manual commands in the Failed outcome are the developer's recourse.
		_ = restoreScopeDarwin(ctx, run, applied[i])
	}
}

// reconcileScopes builds this run's prior-state snapshot. When a service's
// LIVE url already equals ours -- a previous run crashed after writing it
// but before finishing -- its ORIGINAL prior (from that run's own record) is
// reused instead of recording our own PAC URL as if it had been there before
// us; the requirement that made a difference the day this was written.
func reconcileScopes(existing *SystemEntry, services []string, live map[string]ScopeState, pacURL string) []ProxyScope {
	priorByService := map[string]ProxyScope{}
	if existing != nil {
		for _, sc := range existing.Scopes {
			priorByService[sc.Service] = sc
		}
	}
	scopes := make([]ProxyScope, 0, len(services))
	for _, svc := range services {
		state := live[svc]
		if state.URLPresent && state.URL == pacURL {
			if prior, ok := priorByService[svc]; ok {
				scopes = append(scopes, prior)
				continue
			}
			// No recorded prior to reconcile from (a live match with no
			// history at all): the safest guess is "nothing was there",
			// never our own URL.
			scopes = append(scopes, ProxyScope{Service: svc})
			continue
		}
		scopes = append(scopes, ProxyScope{
			Service:         svc,
			PriorURLPresent: state.URLPresent,
			PriorURL:        state.URL,
			PriorEnabled:    state.Enabled,
		})
	}
	return scopes
}

func listEnabledServicesDarwin(ctx context.Context, run Runner) ([]string, error) {
	out, err := run(ctx, "networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	return parseListAllNetworkServices(out)
}

// parseListAllNetworkServices is written against `networksetup
// -listallnetworkservices`'s measured output shape: a header line ("An
// asterisk (*) denotes..."), then one service per line, spaces and all, a
// disabled one prefixed '*'.
func parseListAllNetworkServices(raw []byte) ([]string, error) {
	lines := splitLines(raw)
	if len(lines) > 0 && strings.Contains(lines[0], "An asterisk") {
		lines = lines[1:]
	}
	var services []string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "*") {
			continue
		}
		if err := validateArgvSafe(line); err != nil {
			return nil, err
		}
		services = append(services, line)
	}
	return services, nil
}

func getAutoProxyDarwin(ctx context.Context, run Runner, service string) (ScopeState, error) {
	if err := validateArgvSafe(service); err != nil {
		return ScopeState{}, err
	}
	out, err := run(ctx, "networksetup", "-getautoproxyurl", service)
	if err != nil {
		return ScopeState{}, err
	}
	return parseGetAutoProxyURL(service, out), nil
}

// parseGetAutoProxyURL is written against `networksetup -getautoproxyurl`'s
// measured two-line shape: "URL: <url|(null)>" and "Enabled: <Yes|No>".
func parseGetAutoProxyURL(service string, raw []byte) ScopeState {
	state := ScopeState{Service: service}
	for _, line := range splitLines(raw) {
		switch {
		case strings.HasPrefix(line, "URL: "):
			v := strings.TrimPrefix(line, "URL: ")
			if v != "(null)" {
				state.URLPresent = true
				state.URL = v
			}
		case strings.HasPrefix(line, "Enabled: "):
			state.Enabled = strings.TrimPrefix(line, "Enabled: ") == "Yes"
		}
	}
	return state
}

func splitLines(raw []byte) []string {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// sha1LabelRE and sha1TokenRE parse `security find-certificate -Z`'s
// SHA-1 line, observed on macOS 26 as `SHA-1 hash: <40 uppercase hex>`, one
// per certificate. The token-scan fallback keeps a hash matching if a later
// macOS reformats the label.
var (
	sha1LabelRE = regexp.MustCompile(`(?i)SHA-1\s*hash:\s*([0-9A-Fa-f:]+)`)
	sha1TokenRE = regexp.MustCompile(`\b[0-9A-Fa-f]{40}\b`)
)

func findCertificateHasSHA1(raw []byte, want string) bool {
	text := string(raw)
	if m := sha1LabelRE.FindStringSubmatch(text); m != nil {
		if strings.ToUpper(strings.ReplaceAll(m[1], ":", "")) == want {
			return true
		}
	}
	for _, m := range sha1TokenRE.FindAllString(text, -1) {
		if strings.ToUpper(m) == want {
			return true
		}
	}
	return false
}

func manualCommandsDarwin(plan Plan) []string {
	return []string{
		fmt.Sprintf("sudo security add-trusted-cert -d -r trustRoot -k %s %s", systemKeychainPath, plan.CAPath),
		"networksetup -listallnetworkservices   # then, for each line NOT prefixed '*':",
		fmt.Sprintf("sudo networksetup -setautoproxyurl <service> %s", plan.PACURL),
		"sudo networksetup -setautoproxystate <service> on",
	}
}

// manualDeactivateCommandsDarwin is printed for a developer to run BY HAND,
// after this run returns -- and, on the uninstall path, after the caller has
// already deleted the CA files this record names. So every line here must be
// self-contained: never `remove-trusted-cert -d <path>`, which needs a file
// that may already be gone by the time it is pasted; delete-certificate
// matches by SHA-1 alone and needs no file, and was confirmed on this
// machine to also drop the certificate's trust entry.
func manualDeactivateCommandsDarwin(entry SystemEntry) []string {
	var lines []string
	for _, sc := range entry.Scopes {
		if !sc.PriorURLPresent {
			lines = append(lines, fmt.Sprintf("sudo networksetup -setautoproxystate %q off", sc.Service))
			continue
		}
		lines = append(lines, fmt.Sprintf("sudo networksetup -setautoproxyurl %q %s", sc.Service, sc.PriorURL))
		lines = append(lines, fmt.Sprintf("sudo networksetup -setautoproxystate %q %s", sc.Service, onOff(sc.PriorEnabled)))
	}
	if entry.CATrust != nil {
		lines = append(lines, fmt.Sprintf("sudo security delete-certificate -Z %s %s", entry.CATrust.SHA1, systemKeychainPath))
	}
	return lines
}
