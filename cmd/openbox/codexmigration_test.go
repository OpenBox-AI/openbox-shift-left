package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

const (
	transportUnitLabel = "ai.openbox.transport"
	telemetryUnitLabel = "ai.openbox.telemetry"
)

// codexInstallRig is an install harness for `init --provider codex` with the
// system PAC stood up as a scripted fake. It records the order of every
// privileged-ish step, and calls check after each so a test can assert what
// the two daemons would do at that exact point.
type codexInstallRig struct {
	t      *testing.T
	h      *laneHarness
	config string
	events []string
	// check runs at every step with its name.
	check func(step string)
	// commitAt is the activation's commit time the fake PAC writes.
	commitAt string
	// activations counts how many times the PAC step ran.
	activations int
	// failActivation makes the PAC step fail after writing the pending record.
	failActivation bool
}

func newCodexInstallRig(t *testing.T) *codexInstallRig {
	t.Helper()
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	withSystemPACSupport(t, true)
	codexHome := filepath.Join(h.home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("HOME", h.home)
	// The spool the relay's evidence marker lives under must be this test's.
	t.Setenv("OPENBOX_SPOOL_ROOT", filepath.Join(h.home, ".openbox"))

	r := &codexInstallRig{t: t, h: h, config: providers.CodexConfigTOMLPath(), commitAt: "2026-09-30T10:00:00Z"}

	baseInstall := installLaneUnitFn
	installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
		err := baseInstall(spec, goos, homeDir, binPath)
		r.step("unit:" + spec.Label)
		return err
	}
	baseUninstall := uninstallLaneUnitFn
	uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
		r.events = append(r.events, "unit-removed:"+spec.Label)
		return baseUninstall(spec, goos, homeDir)
	}
	activateSystemPACFn = func(_ context.Context, _ activation.Runner, plan activation.Plan) (activation.Outcome, error) {
		r.activations++
		// The library's own order: the record goes down Pending before any
		// privileged write, and is committed last.
		r.writeSystemEntry(&activation.SystemEntry{Pending: true, PACURL: plan.PACURL, Providers: plan.Providers})
		r.step("pac:pending")
		if r.failActivation {
			return activation.Outcome{Class: activation.Failed, Reason: "the trust did not verify"}, nil
		}
		entry := &activation.SystemEntry{PACActivated: true, ActivatedAt: r.commitAt, PACURL: plan.PACURL, Providers: plan.Providers}
		r.writeSystemEntry(entry)
		r.step("pac:committed")
		return activation.Outcome{Class: activation.Activated, Entry: entry}, nil
	}
	return r
}

func (r *codexInstallRig) step(name string) {
	r.events = append(r.events, name)
	if r.check != nil {
		r.check(name)
	}
}

func (r *codexInstallRig) recordPath() string { return activation.RecordPath(r.h.home) }

func (r *codexInstallRig) writeSystemEntry(entry *activation.SystemEntry) {
	r.t.Helper()
	raw, err := json.Marshal(activation.Record{Lanes: map[activation.Lane]*activation.Entry{}, System: entry})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.recordPath()), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.recordPath(), raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// paths is what the daemons of a NEW install are handed.
func (r *codexInstallRig) paths() codexElectionPaths {
	return newCodexElectionPaths(r.config, r.recordPath(), nil)
}

// seedTelemetryOnlyInstall is a machine as a previous release left it: the
// owned [otel] block, and a telemetry unit carrying --codex-settings but no
// record path.
func (r *codexInstallRig) seedTelemetryOnlyInstall() {
	r.t.Helper()
	if err := providers.WriteCodexOtel(r.config, "http://"+telemetry.DefaultAddr+"/v1/logs"); err != nil {
		r.t.Fatal(err)
	}
	spec := laneservice.Telemetry(telemetry.DefaultAddr, "", false).WithCodexSettings(r.config)
	if _, err := spec.WriteUnit(osName(), r.h.home, "/usr/local/bin/openbox"); err != nil {
		r.t.Fatal(err)
	}
	r.events = nil
}

func (r *codexInstallRig) init() laneReport {
	r.t.Helper()
	a, _, _ := testApp(map[string]string{"HOME": r.h.home})
	return a.setupLanes(laneRequest{
		telemetry: true, transport: true,
		telemetryAddr: telemetry.DefaultAddr, transportAddr: transport.DefaultAddr,
		provider: "codex",
	})
}

// producers is who would record a Codex call right now, from the files on
// disk and the paths a daemon of the given shape was handed.
func (r *codexInstallRig) producers(paths codexElectionPaths) codexProducers {
	r.t.Helper()
	return feedCodexCallToBothDaemons(r.t, paths)
}

func (r *codexInstallRig) unitBody(label string) string {
	r.t.Helper()
	for path := range r.h.units {
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), strings.TrimPrefix(label, "ai.openbox.")) {
			return string(raw)
		}
	}
	r.t.Fatalf("no unit for %s among %v", label, r.h.units)
	return ""
}

func osName() string { return runtime.GOOS }

func exactlyOne(t *testing.T, step string, got codexProducers) string {
	t.Helper()
	relay, tel := got.relayRecords > 0, got.telemetryRecords > 0
	switch {
	case relay && tel:
		t.Fatalf("at %s one Codex call was recorded by BOTH daemons", step)
	case !relay && !tel:
		t.Fatalf("at %s one Codex call was recorded by NEITHER daemon though telemetry is routed", step)
	case relay:
		return "relay"
	}
	return "telemetry"
}

// TestCodexUpgradeFromTelemetryOnlyNeverDoubleRecords is the migration: a
// machine that only ever had Codex's telemetry lane is upgraded by `init`,
// twice. At every intermediate step exactly one daemon records a Codex call;
// only once the PAC is committed AND the relay has seen a Codex call does the
// relay take over, and telemetry goes silent.
func TestCodexUpgradeFromTelemetryOnlyNeverDoubleRecords(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()

	// The old install: its daemons carry no record path, so the relay (none yet)
	// has no proxy arm and telemetry records.
	if got := exactlyOne(t, "the old install", r.producers(codexElectionPaths{config: r.config})); got != "telemetry" {
		t.Fatalf("the old install's producer is %s, want telemetry", got)
	}

	var seen []string
	var at map[string]string
	r.check = func(step string) {
		// A daemon started from the new units reads the new paths.
		at[step] = exactlyOne(t, step, r.producers(r.paths()))
		seen = append(seen, step)
	}

	for _, run := range []string{"first", "second"} {
		func() {
			seen, at = nil, map[string]string{}
			r.events = nil
			r.commitAt = map[string]string{"first": "2026-09-30T10:00:00Z", "second": "2026-09-30T11:00:00Z"}[run]
			report := r.init()
			if len(report.failed) != 0 || report.systemPAC.Class != activation.Activated {
				t.Fatalf("%s init: %+v", run, report)
			}
			wantOrder := []string{"unit:" + transportUnitLabel, "unit:" + telemetryUnitLabel, "pac:pending", "pac:committed"}
			if !slices.Equal(seen, wantOrder) {
				t.Fatalf("%s init steps = %v, want the relay's unit, then telemetry's, then the PAC: %v", run, seen, wantOrder)
			}
			// Until the relay has seen Codex under THIS activation, telemetry is the
			// producer, including right after the commit. The second run starts from
			// a committed activation with evidence, so the relay is the producer
			// while the units are rewritten; the moment the PAC goes Pending it
			// hands back to telemetry, and both daemons read the same file.
			for _, step := range wantOrder {
				want := "telemetry"
				if run == "second" && strings.HasPrefix(step, "unit:") {
					want = "relay"
				}
				if at[step] != want {
					t.Errorf("%s init: at %s the producer is %s, want %s", run, step, at[step], want)
				}
			}

			// The relay's gate sees a Codex model call: evidence is on disk, the
			// relay takes over, telemetry is silent.
			wrote, err := activation.MarkCodexProxyObserved(r.recordPath(), r.paths().marker)
			if err != nil || !wrote {
				t.Fatalf("recording the relay's evidence = %v, %v", wrote, err)
			}
			if got := exactlyOne(t, "after the evidence", r.producers(r.paths())); got != "relay" {
				t.Errorf("%s init: after the commit and the evidence the producer is %s, want the relay", run, got)
			}
		}()
	}

	transportUnit := r.unitBody("transport")
	for _, want := range []string{"--providers", "codex", "--codex-settings", "--pac-record"} {
		if !strings.Contains(transportUnit, want) {
			t.Errorf("the transport unit lacks %s:\n%s", want, transportUnit)
		}
	}
	if telemetryUnit := r.unitBody("telemetry"); !strings.Contains(telemetryUnit, "--pac-record") {
		t.Errorf("the telemetry unit was not rewritten with the record path:\n%s", telemetryUnit)
	}
}

// TestCodexSecondInitNeedsFreshEvidenceAfterAReactivation: re-activating the
// PAC (a new commit time) makes the earlier evidence stale, so the relay
// must see Codex again before it outranks telemetry.
func TestCodexSecondInitNeedsFreshEvidenceAfterAReactivation(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()
	if report := r.init(); report.systemPAC.Class != activation.Activated {
		t.Fatalf("first init: %+v", report)
	}
	if _, err := activation.MarkCodexProxyObserved(r.recordPath(), r.paths().marker); err != nil {
		t.Fatal(err)
	}
	if got := exactlyOne(t, "first install with evidence", r.producers(r.paths())); got != "relay" {
		t.Fatalf("producer = %s, want relay", got)
	}

	r.commitAt = "2026-09-30T12:00:00Z"
	if report := r.init(); report.systemPAC.Class != activation.Activated {
		t.Fatalf("second init: %+v", report)
	}
	if got := exactlyOne(t, "a re-activation", r.producers(r.paths())); got != "telemetry" {
		t.Errorf("after a re-activation the producer is %s, want telemetry until the relay sees Codex again", got)
	}
}

// TestCodexInstallFailureAfterTheUnitIsWrittenRemovesItAndWritesNoPAC: the
// relay never comes up, so its unit is removed again, the system is never
// pointed at it, and no record is written.
func TestCodexInstallFailureAfterTheUnitIsWrittenRemovesItAndWritesNoPAC(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()
	waitForListenerFn = func(addr string, _ time.Duration) bool { return addr != transport.DefaultAddr }

	report := r.init()
	if !slices.Contains(report.failed, "transport") {
		t.Fatalf("a relay that never listened was not reported failed: %+v", report)
	}
	if r.activations != 0 {
		t.Errorf("the system PAC step ran %d time(s) for a relay that never listened", r.activations)
	}
	if fileExists(r.recordPath()) {
		t.Error("an activation record was written for a relay that never listened")
	}
	if !slices.Contains(r.events, "unit-removed:"+transportUnitLabel) {
		t.Errorf("the relay's unit was not removed after the failure; steps: %v", r.events)
	}
	if got := exactlyOne(t, "after the failed install", r.producers(r.paths())); got != "telemetry" {
		t.Errorf("producer = %s, want telemetry", got)
	}
}

// TestCodexSystemPACIsNotActivatedWhenTelemetryDidNotComeUpOnThisBinary: an
// older telemetry daemon left running past the commit would keep recording
// the calls the relay also records, so the PAC waits for telemetry.
func TestCodexSystemPACIsNotActivatedWhenTelemetryDidNotComeUpOnThisBinary(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()
	waitForListenerFn = func(addr string, _ time.Duration) bool { return addr != telemetry.DefaultAddr }

	report := r.init()
	if !slices.Contains(report.failed, "telemetry") || !slices.Contains(report.installed, "transport") {
		t.Fatalf("report = %+v", report)
	}
	if r.activations != 0 || fileExists(r.recordPath()) {
		t.Errorf("the PAC was activated (%d) or recorded with telemetry still on the old binary", r.activations)
	}
}

// TestCodexPACFailureLeavesTelemetryElected: a PAC step that fails after the
// pending record went down leaves the relay unelected.
func TestCodexPACFailureLeavesTelemetryElected(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()
	r.failActivation = true
	report := r.init()
	if report.systemPAC.Class != activation.Failed {
		t.Fatalf("systemPAC = %+v", report.systemPAC)
	}
	if _, err := activation.MarkCodexProxyObserved(r.recordPath(), r.paths().marker); err != nil {
		t.Fatal(err)
	}
	if got := exactlyOne(t, "after a failed PAC", r.producers(r.paths())); got != "telemetry" {
		t.Errorf("producer = %s, want telemetry", got)
	}
}

// TestUninstallCommitsCodexRemovalBeforeTheUnitAndThePAC: the election falls
// back to telemetry before anything is restored or stopped, so Codex's
// calls always have a recorder while the machine comes apart.
func TestUninstallCommitsCodexRemovalBeforeTheUnitAndThePAC(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall()
	if report := r.init(); report.systemPAC.Class != activation.Activated {
		t.Fatalf("init: %+v", report)
	}
	if _, err := activation.MarkCodexProxyObserved(r.recordPath(), r.paths().marker); err != nil {
		t.Fatal(err)
	}
	paths := r.paths()
	if got := exactlyOne(t, "before uninstall", r.producers(paths)); got != "relay" {
		t.Fatalf("producer before uninstall = %s, want relay", got)
	}

	var atFirst []string
	observe := func(step string) {
		r.events = append(r.events, step)
		entry, err := activation.LoadSystemEntryAt(r.recordPath())
		if err != nil {
			t.Fatal(err)
		}
		listsCodex := entry != nil && slices.Contains(entry.Providers, "codex")
		atFirst = append(atFirst, step)
		if listsCodex {
			t.Errorf("at %s the record still lists Codex", step)
		}
		if got := exactlyOne(t, step, r.producers(paths)); got != "telemetry" {
			t.Errorf("at %s the producer is %s, want telemetry", step, got)
		}
	}
	r.events = nil
	baseUninstall := uninstallLaneUnitFn
	uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
		if spec.Label == transportUnitLabel {
			observe("unit-removed:" + spec.Label)
		}
		return baseUninstall(spec, goos, homeDir)
	}
	deactivateSystemPACFn = func(context.Context, activation.Runner, activation.SystemEntry) (activation.Report, error) {
		observe("pac-restored")
		return activation.Report{}, nil
	}

	a, _, _ := testApp(map[string]string{"HOME": r.h.home})
	res := a.runRemovals(r.h.home, removalRequest{telemetry: true, transport: true, purge: true, uninstall: true})
	if !res.ok() {
		t.Fatalf("removal failed: %+v", res)
	}
	if len(atFirst) != 2 {
		t.Fatalf("the unit removal and the PAC restore were not both observed: %v", atFirst)
	}
}

// TestClaudeCodeInitWithCodexInstalledWaitsForTelemetryBeforeCommittingThePAC:
// the derived set lists Codex whichever provider is being installed, so a
// Claude Code install that cannot bring telemetry up on this binary must not
// commit a record naming Codex: an older telemetry daemon would keep recording
// what the relay then records too.
func TestClaudeCodeInitWithCodexInstalledWaitsForTelemetryBeforeCommittingThePAC(t *testing.T) {
	r := newCodexInstallRig(t)
	r.seedTelemetryOnlyInstall() // Codex is installed
	waitForListenerFn = func(addr string, _ time.Duration) bool { return addr != telemetry.DefaultAddr }

	a, _, errb := testApp(map[string]string{"HOME": r.h.home})
	report := a.setupLanes(laneRequest{
		telemetry: true, transport: true,
		telemetryAddr: telemetry.DefaultAddr, transportAddr: transport.DefaultAddr,
		provider: "claude-code",
	})
	if !slices.Contains(report.installed, "transport") || !slices.Contains(report.failed, "telemetry") {
		t.Fatalf("report = %+v", report)
	}
	if entry, err := activation.LoadSystemEntryAt(r.recordPath()); r.activations != 0 || err != nil || entry != nil {
		t.Errorf("the PAC was activated (%d) or recorded (%v, %v) with telemetry still on the old binary", r.activations, entry, err)
	}
	if !strings.Contains(errb.String(), "system PAC was not activated") {
		t.Errorf("the withheld PAC was not reported:\n%s", errb.String())
	}

	// With telemetry up, the same install commits it.
	waitForListenerFn = func(string, time.Duration) bool { return true }
	a, _, _ = testApp(map[string]string{"HOME": r.h.home})
	report = a.setupLanes(laneRequest{
		telemetry: true, transport: true,
		telemetryAddr: telemetry.DefaultAddr, transportAddr: transport.DefaultAddr,
		provider: "claude-code",
	})
	if r.activations != 1 || report.systemPAC.Class != activation.Activated {
		t.Errorf("with telemetry up the PAC was not committed: activations=%d %+v", r.activations, report.systemPAC)
	}
}
