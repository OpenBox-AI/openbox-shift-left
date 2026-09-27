package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// This file is the wiring-level test matrix for systempac.go. It never
// exercises the real darwin authorize dance end to end: a test process
// usually has no controlling terminal (os.OpenFile("/dev/tty", ...) fails
// with "device not configured"), which is true of most CI runners, so
// activation.ActivateSystemPAC's own authorizeDarwin would return
// NotAttempted before ever touching a Runner regardless of what these tests
// script -- proving nothing about THIS package's own ordering. Instead these
// tests drive activateSystemPACFn/deactivateSystemPACFn/liveSystemPACFn
// directly with scripted fakes (a "fails the test if invoked" fake wherever
// a path must issue zero privileged argv), which is exactly the
// wiring surface this package owns. The real OS-argv shapes are the library's
// own test suite's job (internal/cli/activation), already covered there with
// its own in-package seams (geteuidFn, openControllingTTY) this package
// cannot reach.

// seedSystemPACRecord writes a Record whose System field is already
// Activated for pacURL, the shape systemPACAlreadyActive reads. Record and
// SystemEntry are exported and RecordPath is exported, so this writes
// through the same file loadRecord/LoadSystemEntry read -- no unexported
// helper needed.
//
// CATrust.SHA1 is computed from whatever is actually on disk at caPath
// (writing a stub cert there first if nothing exists yet), never a literal
// constant: systemPACAlreadyActive now compares the recorded SHA-1 against
// the CA CURRENTLY on disk, so a fixture whose recorded value never matched
// anything real would make every "already active" scenario indistinguishable
// from a CA re-issue.
func seedSystemPACRecord(t *testing.T, home, pacURL, caPath string) {
	t.Helper()
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		caPEM = []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n")
		if err := os.MkdirAll(filepath.Dir(caPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sha1, err := activation.SHA1Fingerprint(caPEM)
	if err != nil {
		t.Fatalf("computing the test CA's SHA-1: %v", err)
	}
	rec := activation.Record{
		Lanes: map[activation.Lane]*activation.Entry{},
		System: &activation.SystemEntry{
			PACActivated: true,
			PACURL:       pacURL,
			Providers:    transportSystemPACProviders,
			Scopes:       []activation.ProxyScope{{Service: "Wi-Fi", PriorURLPresent: false, PriorEnabled: false}},
			CATrust: &activation.CATrustState{
				SHA1:     sha1,
				CAPath:   caPath,
				Keychain: "/Library/Keychains/System.keychain",
			},
		},
	}
	path := activation.RecordPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// findCertificateRunner is a systemPACRunner fake that answers ONLY a
// read-only `security find-certificate` call with sha1's find-certificate
// text shape, and fails the test on anything else -- the shape
// systemPACAlreadyActive's keychain-presence check drives.
func findCertificateRunner(t *testing.T, sha1 string) activation.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "security" && len(args) > 0 && args[0] == "find-certificate" {
			return []byte("SHA-1 hash: " + sha1 + "\n"), nil
		}
		t.Fatalf("systemPACRunner was invoked with %s %v; a matching second invocation must produce zero "+
			"privileged argv, and no unscripted read either", name, args)
		return nil, nil
	}
}

// failingRunner fails the test the
// instant it is invoked, so a scenario that must produce zero privileged
// argv proves it the same way the library's own tests do.
func failingRunner(t *testing.T) activation.Runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("systemPACRunner was invoked; this scenario must produce zero privileged argv")
		return nil, nil
	}
}

// TestSystemPACNotReachedWhenLaneNeverListens is the ordering requirement:
// "lane listen failure => zero privileged argv". activateSystemPACFn itself
// is the fake here (counting), because the real library's own authorize gate
// already returns zero argv in this sandbox regardless (no controlling
// terminal) -- proving nothing about setupTransport's OWN ordering. This
// test proves runSystemPACActivation is never even CALLED when readiness
// never proved.
func TestSystemPACNotReachedWhenLaneNeverListens(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	h.listening = false // the supervisor accepts the unit; nothing ever listens

	calls := 0
	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		calls++
		return activation.Outcome{}, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = failingRunner(t)

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTransport(h.home, transport.DefaultAddr, false); err == nil {
		t.Fatal("setupTransport reported success though nothing was listening")
	}
	if calls != 0 {
		t.Errorf("activateSystemPACFn was called %d time(s); a lane that never listened must produce zero privileged argv", calls)
	}
	if a.lastSystemPACOutcome.Class != "" {
		t.Errorf("lastSystemPACOutcome = %+v; want the zero value when the activate step was never reached", a.lastSystemPACOutcome)
	}
}

// TestSystemPACReachedAfterReadinessAndEnvKeys is the contrasting positive
// case, proving the harness above is actually capable of detecting a
// violation of the ordering rule (unit -> start -> listen -> env keys ->
// THEN the privileged step).
func TestSystemPACReachedAfterReadinessAndEnvKeys(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)

	calls := 0
	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(ctx context.Context, run activation.Runner, plan activation.Plan) (activation.Outcome, error) {
		calls++
		return activation.Outcome{Class: activation.Declined, Reason: "the elevation prompt was declined",
			Manual: []string{"sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain " + plan.CAPath}}, nil
	}

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTransport(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	if calls != 1 {
		t.Errorf("activateSystemPACFn was called %d time(s), want exactly 1", calls)
	}
	if a.lastSystemPACOutcome.Class != activation.Declined {
		t.Errorf("lastSystemPACOutcome.Class = %q, want %q", a.lastSystemPACOutcome.Class, activation.Declined)
	}
}

// TestDeclinedLeavesLaneInstalledAndNamesManualCommands: init/setupLanes exit
// OK, the lane is still reported installed (env-routed), and the report
// names the manual commands.
func TestDeclinedLeavesLaneInstalledAndNamesManualCommands(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)

	const manualCmd = "sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain /fake/ca.pem"
	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		return activation.Outcome{Class: activation.Declined, Reason: "the elevation prompt was declined",
			Manual: []string{manualCmd}}, nil
	}

	a, out, _ := testApp(map[string]string{"HOME": h.home})
	report := a.setupLanes(laneRequest{
		telemetry: true, transport: true,
		telemetryAddr: "127.0.0.1:18789", transportAddr: "127.0.0.1:18790",
	})
	if len(report.failed) != 0 {
		t.Fatalf("a declined PAC step must not fail the lane itself: %+v", report)
	}
	if !contains(report.installed, "transport") {
		t.Fatalf("the transport lane must stay installed and env-routed when PAC is declined: %+v", report)
	}
	report.print(a)
	s := out.String()
	if !strings.Contains(s, "NOT activated") || !strings.Contains(s, "declined") {
		t.Errorf("the report does not name the declined system PAC step:\n%s", s)
	}
	if !strings.Contains(s, manualCmd) {
		t.Errorf("the report does not print the exact manual command:\n%s", s)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestSystemPACAlreadyActiveSkipsThePrivilegedStep is "a second init must not
// prompt again": a record already Activated for this exact PAC URL, whose
// recorded CA fingerprint matches what is actually on disk, whose live read
// confirms it, and whose fingerprint is still present in the System
// keychain, needs no privileged call at all.
func TestSystemPACAlreadyActiveSkipsThePrivilegedStep(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	pacURL := systemPACURL(transport.DefaultAddr)
	seedSystemPACRecord(t, h.home, pacURL, caPath)
	entry, err := activation.LoadSystemEntry(h.home)
	if err != nil {
		t.Fatal(err)
	}

	origLive := liveSystemPACFn
	t.Cleanup(func() { liveSystemPACFn = origLive })
	liveSystemPACFn = func(context.Context, activation.Runner) ([]activation.ScopeState, error) {
		return []activation.ScopeState{{Service: "Wi-Fi", URLPresent: true, URL: pacURL, Enabled: true}}, nil
	}
	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		t.Fatal("activateSystemPACFn was invoked; a matching second invocation must skip it entirely")
		return activation.Outcome{}, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = findCertificateRunner(t, entry.CATrust.SHA1)

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	if a.lastSystemPACOutcome.Class != activation.Activated {
		t.Errorf("lastSystemPACOutcome.Class = %q, want %q", a.lastSystemPACOutcome.Class, activation.Activated)
	}
}

// TestSystemPACAlreadyActiveRequiresALiveMatch is the negative control: a
// live read that no longer matches (drift) must NOT be treated as
// already-active. It never reaches the keychain-presence check at all, so
// systemPACRunner is left refusing any call.
func TestSystemPACAlreadyActiveRequiresALiveMatch(t *testing.T) {
	h := t.TempDir()
	t.Setenv(devconfig.EnvHome, filepath.Join(h, ".openbox"))
	pacURL := systemPACURL(transport.DefaultAddr)
	caPath := filepath.Join(h, "ca.pem")
	seedSystemPACRecord(t, h, pacURL, caPath)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}

	origLive := liveSystemPACFn
	t.Cleanup(func() { liveSystemPACFn = origLive })
	liveSystemPACFn = func(context.Context, activation.Runner) ([]activation.ScopeState, error) {
		return []activation.ScopeState{{Service: "Wi-Fi", URLPresent: true, URL: "http://someone-elses-proxy:9999", Enabled: true}}, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = failingRunner(t)

	if _, active := systemPACAlreadyActive(h, pacURL, caPEM); active {
		t.Error("systemPACAlreadyActive = true though the live read no longer matches our PAC URL")
	}
}

// TestSystemPACAlreadyActiveRequiresMatchingCASHA1 is the Critical fix: a CA
// re-issue (internal/transport's legacy-constrained-CA migration) rewrites
// the file at CAPath without updating this record, so the recorded SHA-1 no
// longer matches what is on disk. That mismatch alone must force
// re-activation, before ever reaching a live read or a keychain call.
func TestSystemPACAlreadyActiveRequiresMatchingCASHA1(t *testing.T) {
	h := t.TempDir()
	t.Setenv(devconfig.EnvHome, filepath.Join(h, ".openbox"))
	pacURL := systemPACURL(transport.DefaultAddr)
	caPath := filepath.Join(h, "ca.pem")
	seedSystemPACRecord(t, h, pacURL, caPath) // records the SHA-1 of the stub CA written here

	origLive := liveSystemPACFn
	t.Cleanup(func() { liveSystemPACFn = origLive })
	liveSystemPACFn = func(context.Context, activation.Runner) ([]activation.ScopeState, error) {
		t.Fatal("liveSystemPACFn must not be reached when the CA SHA-1 already mismatches")
		return nil, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = failingRunner(t)

	reissuedPEM := []byte("-----BEGIN CERTIFICATE-----\nreissued-content\n-----END CERTIFICATE-----\n")
	if _, active := systemPACAlreadyActive(h, pacURL, reissuedPEM); active {
		t.Error("systemPACAlreadyActive = true though the on-disk CA's SHA-1 no longer matches the record")
	}
}

// TestSystemPACAlreadyActiveRequiresTheCertPresentInTheKeychain: a matching
// SHA-1 and a matching live read are not enough if the System keychain no
// longer actually trusts that fingerprint (a developer or another tool
// removed it by hand) -- the record must not read as active either.
func TestSystemPACAlreadyActiveRequiresTheCertPresentInTheKeychain(t *testing.T) {
	h := t.TempDir()
	t.Setenv(devconfig.EnvHome, filepath.Join(h, ".openbox"))
	pacURL := systemPACURL(transport.DefaultAddr)
	caPath := filepath.Join(h, "ca.pem")
	seedSystemPACRecord(t, h, pacURL, caPath)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}

	origLive := liveSystemPACFn
	t.Cleanup(func() { liveSystemPACFn = origLive })
	liveSystemPACFn = func(context.Context, activation.Runner) ([]activation.ScopeState, error) {
		return []activation.ScopeState{{Service: "Wi-Fi", URLPresent: true, URL: pacURL, Enabled: true}}, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "security" && len(args) > 0 && args[0] == "find-certificate" {
			return []byte("no matching certificates found\n"), nil
		}
		t.Fatalf("unexpected call: %s %v", name, args)
		return nil, nil
	}

	if _, active := systemPACAlreadyActive(h, pacURL, caPEM); active {
		t.Error("systemPACAlreadyActive = true though the System keychain no longer carries the recorded SHA-1")
	}
}

// TestSystemPACReactivatesAfterCAReissueOnDisk is the end-to-end wiring
// control for the Critical fix: when the CA file on disk no longer matches
// the record (simulating reissueLegacyCAIfNeeded having just run),
// runSystemPACActivation must call into activateSystemPACFn again rather
// than skip it.
func TestSystemPACReactivatesAfterCAReissueOnDisk(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	pacURL := systemPACURL(transport.DefaultAddr)
	seedSystemPACRecord(t, h.home, pacURL, caPath)

	// Simulate a CA re-issue: the file at the SAME path now holds different
	// content, so its SHA-1 no longer matches the seeded record.
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nreissued\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		calls++
		return activation.Outcome{Class: activation.Activated, Entry: &activation.SystemEntry{PACActivated: true, PACURL: pacURL}}, nil
	}

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	if calls != 1 {
		t.Errorf("activateSystemPACFn was called %d time(s); a CA re-issue on disk must force re-activation, not skip it", calls)
	}
}

// TestUninstallDeactivatesSystemPACBeforeDeletingCAFiles is the uninstall
// ordering requirement: the system deactivate call must happen while the CA
// files still exist, and purgeLaneData deletes them only afterward.
func TestUninstallDeactivatesSystemPACBeforeDeletingCAFiles(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatal(err)
	}
	_, keyPath := transport.CAPaths(openboxHome)
	if err := os.WriteFile(keyPath, []byte("stub-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedSystemPACRecord(t, h.home, systemPACURL(transport.DefaultAddr), caPath)

	var filesExistedAtCall bool
	origDeactivate := deactivateSystemPACFn
	t.Cleanup(func() { deactivateSystemPACFn = origDeactivate })
	deactivateSystemPACFn = func(context.Context, activation.Runner, activation.SystemEntry) (activation.Report, error) {
		filesExistedAtCall = fileExists(caPath) && fileExists(keyPath)
		return activation.Report{Restored: []string{"Wi-Fi"}}, nil
	}

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	res := a.runRemovals(h.home, removalRequest{
		telemetry: true, transport: true, purge: true, uninstall: true,
	})
	if !res.ok() {
		t.Fatalf("runRemovals failed: %+v", res.failed)
	}
	if !filesExistedAtCall {
		t.Fatal("deactivateSystemPACFn ran after the CA files were already deleted, or was never called")
	}
	if fileExists(caPath) || fileExists(keyPath) {
		t.Error("the CA files survived --purge; deactivate must run BEFORE purgeLaneData, not instead of it")
	}
	entry, err := activation.LoadSystemEntry(h.home)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Errorf("the system PAC record survived a clean deactivate+purge: %+v", entry)
	}
}

// TestUninstallDeactivateFailureStillDeletesTheCAKey: a declined/failed
// deactivate must still let purgeLaneData delete the CA key (a trusted cert
// with no matching key cannot mint), and must print the manual commands with
// the SHA-1.
func TestUninstallDeactivateFailureStillDeletesTheCAKey(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatal(err)
	}
	_, keyPath := transport.CAPaths(openboxHome)
	if err := os.WriteFile(keyPath, []byte("stub-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	const sha1 = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	seedSystemPACRecord(t, h.home, systemPACURL(transport.DefaultAddr), caPath)

	origDeactivate := deactivateSystemPACFn
	t.Cleanup(func() { deactivateSystemPACFn = origDeactivate })
	deactivateSystemPACFn = func(context.Context, activation.Runner, activation.SystemEntry) (activation.Report, error) {
		return activation.Report{Class: activation.Declined, Reason: "the elevation prompt was declined",
			Manual: []string{"sudo security delete-certificate -Z " + sha1 + " /Library/Keychains/System.keychain"}}, nil
	}

	a, out, _ := testApp(map[string]string{"HOME": h.home})
	res := a.runRemovals(h.home, removalRequest{
		telemetry: true, transport: true, purge: true, uninstall: true,
	})
	if !res.ok() {
		t.Fatalf("a declined system PAC deactivate must not fail the whole removal: %+v", res.failed)
	}
	if fileExists(caPath) || fileExists(keyPath) {
		t.Error("the CA key must still be deleted even when deactivate is declined")
	}
	s := out.String()
	if !strings.Contains(s, "NOT restored") || !strings.Contains(s, sha1) {
		t.Errorf("the report does not print the exact manual commands with the SHA-1:\n%s", s)
	}
	if strings.Contains(s, "remove-trusted-cert") {
		t.Errorf("the printed recovery command must not reference a file purgeLaneData is about to delete:\n%s", s)
	}
	// The system record survives an unrestored deactivate: it is what doctor
	// reports the leftover proxy and trust from, and what a second uninstall
	// retries with.
	entry, err := activation.LoadSystemEntry(h.home)
	if err != nil || entry == nil {
		t.Errorf("the system PAC record was deleted although nothing was restored (entry=%v, err=%v)", entry, err)
	}
	if !strings.Contains(s, "re-run `openbox uninstall`") {
		t.Errorf("the report does not say a second uninstall retries:\n%s", s)
	}
}

// TestUninstallDeactivateHardErrorFallbackNeverReferencesTheDeletedFileAndNamesTheKeychain
// exercises printSystemPACDeactivateFailed's OWN fallback (manual == nil): a
// hard error after authorization succeeded, the one shape the library itself
// never populates Manual for. The fallback must, like the library's own
// Manual, print only a self-contained delete-certificate naming the System
// keychain -- never remove-trusted-cert against a file purgeLaneData is
// about to delete.
func TestUninstallDeactivateHardErrorFallbackNeverReferencesTheDeletedFileAndNamesTheKeychain(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatal(err)
	}
	_, keyPath := transport.CAPaths(openboxHome)
	if err := os.WriteFile(keyPath, []byte("stub-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedSystemPACRecord(t, h.home, systemPACURL(transport.DefaultAddr), caPath)
	entry, err := activation.LoadSystemEntry(h.home)
	if err != nil {
		t.Fatal(err)
	}

	origDeactivate := deactivateSystemPACFn
	t.Cleanup(func() { deactivateSystemPACFn = origDeactivate })
	deactivateSystemPACFn = func(context.Context, activation.Runner, activation.SystemEntry) (activation.Report, error) {
		return activation.Report{}, errors.New("boom: a hard error after authorization succeeded")
	}

	a, out, _ := testApp(map[string]string{"HOME": h.home})
	res := a.runRemovals(h.home, removalRequest{
		telemetry: true, transport: true, purge: true, uninstall: true,
	})
	if !res.ok() {
		t.Fatalf("a hard deactivate error must not fail the whole removal: %+v", res.failed)
	}
	s := out.String()
	if strings.Contains(s, "remove-trusted-cert") {
		t.Errorf("the fallback must not print remove-trusted-cert for a file purgeLaneData is about to delete:\n%s", s)
	}
	want := "sudo security delete-certificate -Z " + entry.CATrust.SHA1 + " " + entry.CATrust.Keychain
	if !strings.Contains(s, want) {
		t.Errorf("missing the exact fallback command %q:\n%s", want, s)
	}
}

// TestNonDarwinReportLine is the exact sentence printed for an OS this build does not support: no manual commands, and no claim
// that anything was attempted.
func TestNonDarwinReportLine(t *testing.T) {
	a, out, _ := testApp(nil)
	report := laneReport{
		installed: []string{"transport"},
		keys:      1,
		settings:  "/somewhere/settings.json",
		addrs:     map[string]string{"transport": "127.0.0.1:8790"},
		systemPAC: activation.Outcome{Class: activation.NotAttempted,
			Reason: "system PAC/CA activation is not implemented for this OS in this build"},
	}
	report.print(a)
	s := out.String()
	want := "not activated (not yet supported on " + runtime.GOOS + " in this build)"
	if !strings.Contains(s, want) {
		t.Errorf("missing the exact non-darwin sentence (%q):\n%s", want, s)
	}
	if !strings.Contains(s, "desktop apps and browsers are not covered") {
		t.Errorf("missing the coverage disclaimer:\n%s", s)
	}
	for _, banned := range []string{"sudo", "networksetup", "security add-trusted-cert"} {
		if strings.Contains(s, banned) {
			t.Errorf("the unsupported-OS line must print no commands (found %q):\n%s", banned, s)
		}
	}
}

// TestTransportUnitCarriesProvidersClaudeCode: the transport lane unit must
// carry --providers claude-code, the union this init enables.
func TestTransportUnitCarriesProvidersClaudeCode(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if _, err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	unitPath := laneservice.Transport("", "", false).UnitPath(runtime.GOOS, h.home)
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the written unit: %v", err)
	}
	if !strings.Contains(string(raw), laneservice.ProvidersFlag) || !strings.Contains(string(raw), "claude-code") {
		t.Errorf("the transport unit does not carry %s claude-code:\n%s", laneservice.ProvidersFlag, raw)
	}
}

// TestTelemetryAndCodexInitNeverInvokeSystemPAC: the system PAC step belongs
// to the transport lane only. Neither the shared telemetry lane nor Codex's
// own telemetry setup may ever reach it.
func TestTelemetryAndCodexInitNeverInvokeSystemPAC(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)

	origActivate := activateSystemPACFn
	t.Cleanup(func() { activateSystemPACFn = origActivate })
	activateSystemPACFn = func(context.Context, activation.Runner, activation.Plan) (activation.Outcome, error) {
		t.Fatal("activateSystemPACFn was invoked from a telemetry-only path")
		return activation.Outcome{}, nil
	}
	origRunner := systemPACRunner
	t.Cleanup(func() { systemPACRunner = origRunner })
	systemPACRunner = failingRunner(t)

	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTelemetry(h.home, telemetry.DefaultAddr, false); err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
	if _, err := a.setupCodexTelemetry(h.home, telemetry.DefaultAddr, false); err != nil {
		t.Fatalf("setupCodexTelemetry: %v", err)
	}
}

// TestReissueLegacyCAIfNeededIsIdempotent: a legacy constrained CA is
// re-issued exactly once, reported when it happens, and a second call finds
// nothing left to do.
func TestReissueLegacyCAIfNeededIsIdempotent(t *testing.T) {
	home := isolateHomeUnbound(t)
	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatal(err)
	}
	_ = home
	writeLegacyConstrainedCA(t, openboxHome)

	a, out, _ := testApp(nil)
	if err := a.reissueLegacyCAIfNeeded(); err != nil {
		t.Fatalf("reissueLegacyCAIfNeeded: %v", err)
	}
	if !strings.Contains(out.String(), "CA re-issued") {
		t.Errorf("the first call did not report the re-issue:\n%s", out.String())
	}
	ca, err := transport.LoadOrCreateCA(openboxHome)
	if err != nil {
		t.Fatal(err)
	}
	if transport.CANeedsReissue(ca) {
		t.Fatal("the CA still needs a re-issue after reissueLegacyCAIfNeeded ran")
	}

	out.Reset()
	if err := a.reissueLegacyCAIfNeeded(); err != nil {
		t.Fatalf("second reissueLegacyCAIfNeeded: %v", err)
	}
	if strings.Contains(out.String(), "CA re-issued") {
		t.Errorf("a second call re-issued again, or reported as if it had:\n%s", out.String())
	}
}
