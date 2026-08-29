package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/activation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/laneservice"
	"github.com/openbox-ai/openbox-shift-left/transport"
)

// initlane_test.go covers the install and removal choreography WITHOUT a socket.
//
// That is deliberate rather than a compromise. The nine tests that cover the
// gateway's own install all carry RequireBind, so on a host that denies bind they
// SKIP — and this repo has already learned what that costs: over two days of
// bind-guarded runs, five of six failures were bugs in the tests rather than in
// the code, because the guard makes a test honest about what it needs and
// simultaneously blinds whoever cannot meet it. Every assertion below is about
// ORDER, IDENTITY and STATE, none of which needs a listener: the readiness probe
// is already a seam, and a dial to a port nobody holds answers "not occupied"
// without binding anything.

// laneHarness stubs the supervisor and the readiness probe, and records every
// command the install path would have run.
type laneHarness struct {
	home      string
	commands  []string
	units     map[string]bool
	listening bool
}

func newLaneHarness(t *testing.T) *laneHarness {
	t.Helper()
	h := &laneHarness{home: t.TempDir(), units: map[string]bool{}, listening: true}

	origRun, origUID := run, currentUID
	origListen, origFree := waitForListenerFn, waitForPortFreeFn
	origInstall, origUninstall := installLaneUnitFn, uninstallLaneUnitFn
	origGwInstall, origGwUninstall := installUnitFn, uninstallUnitFn
	t.Cleanup(func() {
		run, currentUID = origRun, origUID
		waitForListenerFn, waitForPortFreeFn = origListen, origFree
		installLaneUnitFn, uninstallLaneUnitFn = origInstall, origUninstall
		installUnitFn, uninstallUnitFn = origGwInstall, origGwUninstall
	})

	currentUID = func() string { return "501" }
	run = func(name string, args ...string) error {
		h.commands = append(h.commands, name+" "+strings.Join(args, " "))
		return nil
	}
	waitForListenerFn = func(string, time.Duration) bool { return h.listening }
	waitForPortFreeFn = func(string, time.Duration) bool { return true }

	// Route the unit write through the PATH-EXPLICIT writer. Production uses
	// kardianos/service, which ignores $HOME on darwin and would install a real
	// launchd unit into the home of whoever ran `go test`.
	installLaneUnitFn = func(spec laneservice.Spec, goos, homeDir, binPath string) error {
		path, err := spec.WriteUnit(goos, homeDir, binPath)
		if err == nil {
			h.units[path] = true
		}
		return err
	}
	uninstallLaneUnitFn = func(spec laneservice.Spec, goos, homeDir string) error {
		path, err := spec.RemoveUnit(goos, homeDir)
		if err == nil && path != "" {
			delete(h.units, path)
		}
		return err
	}
	installUnitFn = func(goos, homeDir, binPath, addr, upstream string, verbose bool) error {
		_, err := gatewayservice.WriteUnit(goos, homeDir, binPath, addr, upstream, verbose)
		return err
	}
	uninstallUnitFn = func(goos, homeDir string) error {
		_, err := gatewayservice.RemoveUnit(goos, homeDir)
		return err
	}

	// A private OPENBOX_HOME so the CA and spool paths never reach the real one.
	t.Setenv(devconfig.EnvHome, filepath.Join(h.home, ".openbox"))
	if err := os.MkdirAll(filepath.Join(h.home, ".openbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

// seedCA writes the certificate the transport lane refuses to name unless it
// exists. The daemon generates the real one before it listens; here the daemon
// is stubbed, so the file stands in for it.
func (h *laneHarness) seedCA(t *testing.T) string {
	t.Helper()
	openboxHome, err := devconfig.Home()
	if err != nil {
		t.Fatal(err)
	}
	caPath, _ := transport.CAPaths(openboxHome)
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return caPath
}

func laneSettings(t *testing.T, home string) map[string]string {
	t.Helper()
	return activation.CurrentEnv(gatewayservice.SettingsPath(home))
}

// TestLaneEnvIsWrittenOnlyAfterTheDaemonIsProvenUp is the safety property, for
// the two lanes that did not have it covered.
//
// A dead loopback proxy fails CLOSED, so writing HTTPS_PROXY before the relay is
// proven up breaks every model call on the machine while `init` prints success.
func TestLaneEnvIsWrittenOnlyAfterTheDaemonIsProvenUp(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	h.listening = false // the supervisor accepts the unit; nothing serves
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	err := a.setupTransport(h.home, "127.0.0.1:18790", false)
	if err == nil {
		t.Fatal("setupTransport reported success though nothing was listening")
	}
	if env := laneSettings(t, h.home); env["HTTPS_PROXY"] != "" {
		t.Errorf("HTTPS_PROXY was written with no relay listening (%q) — every model call on this machine would now fail", env["HTTPS_PROXY"])
	}
	// And no unit is left behind: KeepAlive would restart-loop a daemon the
	// developer was never told about, and init exits 0 because the caller
	// downgrades this to a warning.
	if len(h.units) != 0 {
		t.Errorf("a failed install left units behind: %v", h.units)
	}
	if !strings.Contains(out.String(), "rolled back") {
		t.Errorf("the rollback was silent:\n%s", out.String())
	}
}

// TestLaneEnvIsWrittenAfterReadiness is the happy path, and it asserts the ORDER
// rather than only the outcome.
func TestLaneEnvIsWrittenAfterReadiness(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	if err := a.setupTransport(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	env := laneSettings(t, h.home)
	if env["HTTPS_PROXY"] != "http://127.0.0.1:18790" {
		t.Errorf("HTTPS_PROXY = %q", env["HTTPS_PROXY"])
	}
	if env["NODE_EXTRA_CA_CERTS"] != caPath {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q", env["NODE_EXTRA_CA_CERTS"], caPath)
	}
	s := out.String()
	iListen, iEnv := strings.Index(s, "listening on"), strings.Index(s, "transport env")
	if iListen < 0 || iEnv < 0 {
		t.Fatalf("output missing one of the two steps:\n%s", s)
	}
	if iListen > iEnv {
		t.Errorf("env was reported before readiness — the order is the safety property:\n%s", s)
	}
}

// TestTransportRefusesToNameACAThatIsNotThere. NODE_EXTRA_CA_CERTS pointing at a
// missing file fails every intercepted handshake, which a developer experiences
// as the provider being down rather than as a config error.
func TestTransportRefusesToNameACAThatIsNotThere(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t) // no seedCA
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	err := a.setupTransport(h.home, "127.0.0.1:18790", false)
	if err == nil {
		t.Fatal("setupTransport pointed the tool at a CA that does not exist")
	}
	if env := laneSettings(t, h.home); env["HTTPS_PROXY"] != "" {
		t.Error("the proxy keys were written even though the CA was missing")
	}
	if len(h.units) != 0 {
		t.Errorf("the failed install left units behind: %v", h.units)
	}
}

// TestEachLaneIsAddressedByItsOwnSupervisorIdentity is the defect this
// generalization could most easily have introduced, and it is invisible from the
// outside: `systemctl --user enable --now` names a UNIT and `launchctl bootout`
// names a LABEL, so a hardcoded gateway identity would start or stop the gateway
// when asked to start telemetry — and report success either way.
//
// It drives INSTALL AND REMOVAL, and that is load-bearing rather than thorough.
// On darwin the install path only ever reaches `launchctl bootstrap`, which
// names the unit PATH and is therefore per-lane whatever the code does; the
// label is used solely by `bootout`, which nothing but a removal or a rollback
// calls. A version of this test that stopped after the install passed with the
// gateway's label hardcoded — measured, not assumed.
func TestEachLaneIsAddressedByItsOwnSupervisorIdentity(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if err := a.setupTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}
	if err := a.removeTelemetry(h.home, false); err != nil {
		t.Fatalf("removeTelemetry: %v", err)
	}

	joined := strings.Join(h.commands, "\n")
	var mine, theirs string
	switch runtime.GOOS {
	case "linux":
		mine, theirs = "openbox-telemetry.service", "openbox-gateway.service"
	case "darwin":
		// The LABEL as launchctl is given it, not as it appears inside a plist
		// path — the path contains the label too, so matching the bare string
		// would pass on the install's `bootstrap <path>` alone.
		mine, theirs = "gui/501/ai.openbox.telemetry", "gui/501/ai.openbox.gateway"
	}
	if !strings.Contains(joined, mine) {
		t.Errorf("the telemetry lane was never addressed by its own identity (%s):\n%s", mine, joined)
	}
	if strings.Contains(joined, theirs) {
		t.Errorf("acting on the telemetry lane addressed the GATEWAY (%s):\n%s", theirs, joined)
	}

	// And the mapping itself, on every platform: the two OS branches above can
	// only ever exercise one of them from a single test run.
	for _, tc := range []struct {
		spec  laneservice.Spec
		label string
		unit  string
	}{
		{laneservice.Telemetry("127.0.0.1:8789", false), "ai.openbox.telemetry", "openbox-telemetry.service"},
		{laneservice.Transport("127.0.0.1:8790", false), "ai.openbox.transport", "openbox-transport.service"},
	} {
		id := identityOf(tc.spec, h.home)
		if id.launchdLabel != tc.label || id.systemdUnit != tc.unit {
			t.Errorf("identityOf(%s) = {%s, %s}, want {%s, %s}", tc.spec.Label, id.launchdLabel, id.systemdUnit, tc.label, tc.unit)
		}
	}
}

// TestRemovalRestoresAForeignValueByteIdentically is the state-diff the phase's
// acceptance criterion names.
//
// The org's own relay and proxy settings must come back EXACTLY, and the keys
// OpenBox added must be gone — with everything else in the file untouched.
func TestRemovalRestoresAForeignValueByteIdentically(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	// A machine that already has an org relay, a corporate proxy and an unrelated
	// key inside the same env block.
	settingsPath := gatewayservice.SettingsPath(h.home)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const before = `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://llm-proxy.corp.internal",
    "HTTPS_PROXY": "http://proxy.corp.internal:3128",
    "NO_PROXY": "internal.corp",
    "CORP_TOKEN_PATH": "/etc/corp/token"
  },
  "permissions": {"allow": ["Bash"]}
}`
	if err := os.WriteFile(settingsPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.setupTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}
	if err := a.setupTransport(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	if got := laneSettings(t, h.home)["HTTPS_PROXY"]; got != "http://127.0.0.1:18790" {
		t.Fatalf("the install did not take: HTTPS_PROXY = %q", got)
	}

	if code := a.runRemovals(h.home, removalRequest{telemetry: true, transport: true}); code != exitOK {
		t.Fatalf("runRemovals exited %d", code)
	}

	env := laneSettings(t, h.home)
	for key, want := range map[string]string{
		"HTTPS_PROXY":        "http://proxy.corp.internal:3128",
		"NO_PROXY":           "internal.corp",
		"CORP_TOKEN_PATH":    "/etc/corp/token",
		"ANTHROPIC_BASE_URL": "https://llm-proxy.corp.internal",
	} {
		if env[key] != want {
			t.Errorf("%s = %q after removal, want the pre-OpenBox %q", key, env[key], want)
		}
	}
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "CLAUDE_CODE_CERT_STORE", "CLAUDE_CODE_ENABLE_TELEMETRY", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if _, present := env[key]; present {
			t.Errorf("%s survived removal", key)
		}
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"permissions"`) {
		t.Errorf("a top-level key outside env was lost:\n%s", raw)
	}

	// The other half of the state diff, and the one with the worse failure: a unit
	// left on disk carries KeepAlive/Restart=always, so the supervisor restart-loops
	// a daemon the developer believes they removed.
	if len(h.units) != 0 {
		t.Errorf("units survived removal: %v", h.units)
	}
	for _, spec := range []laneservice.Spec{
		laneservice.Telemetry("", false),
		laneservice.Transport("", false),
	} {
		if path := spec.UnitPath(runtime.GOOS, h.home); path != "" && fileExists(path) {
			t.Errorf("%s survived removal", path)
		}
	}
	// And the record forgot both lanes, so a later --remove-all has nothing to
	// restore over.
	if lanes := activation.ActiveLanes(h.home); len(lanes) != 0 {
		t.Errorf("the activation record still claims %v as installed", lanes)
	}
}

// TestASecondFullInstallDoesNotOverwriteTheRememberedOriginals is the
// second-invocation rule this repo states in its own words: check reads and
// writes separately, and test the SECOND invocation. Fifteen green tests once
// missed a reverted opt-out because each ran init exactly once.
func TestASecondFullInstallDoesNotOverwriteTheRememberedOriginals(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	settingsPath := gatewayservice.SettingsPath(h.home)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"HTTPS_PROXY":"http://proxy.corp.internal:3128"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two installs, the second on a different port — the exact shape that made
	// the gateway record its OWN previous URL as the org's.
	if err := a.setupTransport(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := a.setupTransport(h.home, "127.0.0.1:18791", false); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if code := a.runRemovals(h.home, removalRequest{transport: true}); code != exitOK {
		t.Fatalf("runRemovals exited %d", code)
	}
	if got := laneSettings(t, h.home)["HTTPS_PROXY"]; got != "http://proxy.corp.internal:3128" {
		t.Errorf("after two installs the restore produced %q; a re-install overwrote the remembered original", got)
	}
}

// TestRemovalIsSafeOnAMachineThatNeverInstalledAnything. `--remove-all` runs on
// partial state by design, and before the credential gate — removal must not
// require the thing being removed to still be usable.
func TestRemovalIsSafeOnAMachineThatNeverInstalledAnything(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	a, _, errb := testApp(map[string]string{"HOME": h.home})

	if code := a.runRemovals(h.home, removalRequest{gateway: true, telemetry: true, transport: true, purge: true}); code != exitOK {
		t.Fatalf("runRemovals exited %d on an untouched machine: %s", code, errb.String())
	}
}

// TestRemovalRefusesToOverwriteAChangedValueButStillRemovesTheUnit.
//
// Restoring over a value somebody edited destroys their edit. Refusing is right
// — but a refusal that also abandoned the daemon would leave a KeepAlive job
// running with nothing pointing at it, which is the worse outcome of the two.
func TestRemovalRefusesToOverwriteAChangedValueButStillRemovesTheUnit(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	if err := a.setupTransport(h.home, "127.0.0.1:18790", false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	// Somebody edits the value we set.
	settingsPath := gatewayservice.SettingsPath(h.home)
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"HTTPS_PROXY":"http://someone-elses-proxy:9999"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := a.runRemovals(h.home, removalRequest{transport: true}); code == exitOK {
		t.Fatal("removal silently overwrote a value that changed after OpenBox set it")
	}
	if got := laneSettings(t, h.home)["HTTPS_PROXY"]; got != "http://someone-elses-proxy:9999" {
		t.Errorf("the refusal still rewrote the value: %q", got)
	}

	// And --force-restore completes it.
	if code := a.runRemovals(h.home, removalRequest{transport: true, force: true}); code != exitOK {
		t.Fatalf("--force-restore did not complete the removal (exit %d)", code)
	}
	if _, present := laneSettings(t, h.home)["HTTPS_PROXY"]; present {
		t.Error("--force-restore left the key behind")
	}
}

// TestPurgeDeletesTheCAAndTheRecord. Leaving a trusted signing key on the
// machine after the relay that used it is gone is a strictly worse posture than
// removing it; it is regenerated on the next install.
func TestPurgeDeletesTheCAAndTheRecord(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	caPath := h.seedCA(t)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	if err := a.setupTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
		t.Fatalf("setupTelemetry: %v", err)
	}
	if _, err := os.Stat(activation.RecordPath(h.home)); err != nil {
		t.Fatalf("no activation record was written: %v", err)
	}

	if code := a.runRemovals(h.home, removalRequest{telemetry: true, purge: true}); code != exitOK {
		t.Fatalf("runRemovals exited %d", code)
	}
	if fileExists(caPath) {
		t.Error("the CA survived --remove-all; a trusted signing key with no relay behind it is a worse posture than none")
	}
	if fileExists(activation.RecordPath(h.home)) {
		t.Error("the activation record survived --remove-all")
	}
	// Data destruction is announced, never silent.
	if !strings.Contains(out.String(), "deleted") {
		t.Errorf("nothing was reported as deleted:\n%s", out.String())
	}
}

// TestLaneFlagsAreMutuallyExclusiveAndClaudeCodeOnly.
//
// The provider guard is the defect --gateway already shipped and fixed:
// `--provider codex --full` would install two supervised daemons and rewrite
// ~/.claude/settings.json on a machine whose tool reads neither.
func TestLaneFlagsAreMutuallyExclusiveAndClaudeCodeOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"full and remove-all", []string{"--provider", "claude-code", "--full", "--remove-all"}, "mutually exclusive"},
		{"transport both ways", []string{"--provider", "claude-code", "--transport", "--remove-transport"}, "mutually exclusive"},
		{"telemetry both ways", []string{"--provider", "claude-code", "--telemetry", "--remove-telemetry"}, "mutually exclusive"},
		{"full on codex", []string{"--provider", "codex", "--full"}, "claude-code only"},
		{"transport on codex", []string{"--provider", "codex", "--transport"}, "claude-code only"},
		{"remove-all on codex", []string{"--provider", "codex", "--remove-all"}, "claude-code only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			a, _, errb := testApp(nil)
			if code := a.runInit(tc.args); code == exitOK {
				t.Fatalf("accepted %v", tc.args)
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("error does not say %q: %s", tc.want, errb.String())
			}
		})
	}
}

// TestLaneRemovalRunsBeforeTheCredentialGate.
//
// A machine whose credentials were deleted — an offboarding, a rotation, a wiped
// ~/.openbox — must still be able to back the lanes out. When `--remove-gateway`
// sat below that gate, every model call kept failing closed against a dead
// loopback port and the only remaining fix was hand-editing the settings file.
func TestLaneRemovalRunsBeforeTheCredentialGate(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	isolateHome(t) // no credentials anywhere
	t.Setenv("HOME", h.home)
	a, out, errb := testApp(map[string]string{"HOME": h.home})

	code := a.runInit([]string{"--provider", "claude-code", "--remove-all"})
	if code != exitOK {
		t.Fatalf("--remove-all exited %d on a machine with no credentials: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "openbox auth") {
		t.Errorf("removal was blocked by the credential gate:\n%s", errb.String())
	}
	if !strings.Contains(out.String(), "Removing OpenBox lane configuration") {
		t.Errorf("removal never ran:\n%s", out.String())
	}
}

// TestDryRunNamesTheLanesItWouldInstall. An operator vetting a fleet script has
// to be told about a supervised daemon and a CA on the machine, not shown a
// count.
func TestDryRunNamesTheLanesItWouldInstall(t *testing.T) {
	isolateHome(t)
	a, out, _ := testApp(nil)
	if code := a.runInit([]string{"--provider", "claude-code", "--full", "--dry-run"}); code != exitOK {
		t.Fatalf("dry run exited %d", code)
	}
	s := out.String()
	for _, want := range []string{
		"Telemetry receiver — PLANNED",
		"Transport relay — PLANNED",
		"CLAUDE_CODE_ENABLE_TELEMETRY",
		"NODE_EXTRA_CA_CERTS",
		"INTERCEPTS",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the plan does not mention %q:\n%s", want, s)
		}
	}
}

// TestUnsupportedPlatformIsReportedNotSkipped keeps the refusal reachable from
// the install path rather than only from the renderer.
func TestUnsupportedPlatformIsReportedNotSkipped(t *testing.T) {
	spec := laneservice.Telemetry("127.0.0.1:8789", false)
	if _, err := spec.WriteUnit("windows", t.TempDir(), "openbox.exe"); err == nil {
		t.Fatal("windows reported a successful unit write")
	} else if !errors.Is(err, err) || !strings.Contains(err.Error(), "openbox telemetry") {
		t.Errorf("the error does not name the foreground command: %v", err)
	}
}

// TestDoctorNamesAnElectedLaneThatIsNotThere is the election's own worst failure
// mode, and the only place it is visible.
//
// The election reads where the tool is ROUTED, not what OpenBox installed — so a
// developer's own loopback proxy, or a stale key left by hand, elects a lane that
// does not exist. Every other lane then correctly stands down and the machine
// emits nothing at all, while each individual doctor line still reads as fine.
// Only putting "elected" and "nothing listening" on one line makes it findable.
func TestDoctorNamesAnElectedLaneThatIsNotThere(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	isolateHome(t)
	t.Setenv("HOME", h.home)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	// Somebody else's loopback proxy, on the port nothing of ours holds.
	settingsPath := gatewayservice.SettingsPath(h.home)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := a.runDoctor(nil); code != exitOK {
		t.Fatalf("doctor exited %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "elected      transport") {
		t.Fatalf("doctor did not report the elected lane:\n%s", s)
	}
	if !strings.Contains(s, "ELECTED but nothing is listening") {
		t.Errorf("doctor reported an elected lane with no daemon behind it as healthy — "+
			"this machine emits NO model-call turns and every line above reads as fine:\n%s", s)
	}
}

// TestRemoveAllKeepsTheSharedSpool is a deliberate deviation from this phase's
// requirement text, and it is the phase's own security constraint that decides it:
// "never delete anything outside ~/.openbox/ and the managed keys".
//
// Two independent reasons, either alone sufficient. The spool resolves from
// os.UserConfigDir(), which is outside ~/.openbox. And it is SHARED with the hook
// path — `--remove-all` removes lanes, not hooks — so deleting it destroys
// undelivered governed tool-call evidence belonging to a component that is still
// installed and still running.
func TestRemoveAllKeepsTheSharedSpool(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	spool := filepath.Join(t.TempDir(), "cc-spool")
	t.Setenv(devconfig.EnvSpoolDir, spool)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(spool, "session-abc.jsonl")
	if err := os.WriteFile(pending, []byte("{\"event_type\":\"ToolCall\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := a.runRemovals(h.home, removalRequest{telemetry: true, transport: true, purge: true}); code != exitOK {
		t.Fatalf("runRemovals exited %d", code)
	}
	if !fileExists(pending) {
		t.Error("--remove-all destroyed undelivered hook evidence in the shared spool")
	}
	// And it says so: a developer expecting a full teardown has to know what
	// survived and where it is.
	if !strings.Contains(out.String(), spool) {
		t.Errorf("the spool was kept silently:\n%s", out.String())
	}
}

// TestDoctorReportsAConfiguredLaneThatIsNotInPath keeps two different facts
// apart, because exactly one state separates them and collapsing it loses
// information in either direction.
//
// ROUTED is what the tool's settings point at. IN PATH is what can actually see
// the call. A base URL sends the call past the relay — loopback bypasses the
// proxy, and any other host is blind-tunnelled because it is not the provider's
// — so a transport lane can be perfectly configured and observe nothing.
// Reporting it as unconfigured would hide a lane the developer installed;
// reporting it as in force would promise observation that is not happening.
func TestDoctorReportsAConfiguredLaneThatIsNotInPath(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	isolateHome(t)
	t.Setenv("HOME", h.home)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	settingsPath := gatewayservice.SettingsPath(h.home)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Both in-path lanes configured: the relay AND a loopback base URL.
	if err := os.WriteFile(settingsPath, []byte(
		`{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790","ANTHROPIC_BASE_URL":"http://127.0.0.1:8788"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := a.runDoctor(nil); code != exitOK {
		t.Fatalf("doctor exited %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "elected      gateway") {
		t.Errorf("doctor named the wrong producer: with a loopback base URL the call never reaches the relay:\n%s", s)
	}
	if !strings.Contains(s, "NOT IN PATH  transport") {
		t.Errorf("doctor did not say the transport lane cannot see this machine's calls:\n%s", s)
	}
	// And it is still reported as CONFIGURED, because it is. Scoped to the
	// transport block: the telemetry lane legitimately reports "not pointed at
	// it" in this fixture, and a whole-output match would pass on that instead.
	block := s[strings.Index(s, "Transport lane"):]
	if strings.Contains(block, "configured   no") {
		t.Errorf("doctor reported a configured transport lane as unconfigured:\n%s", block)
	}
	if !strings.Contains(block, "configured   yes") {
		t.Errorf("the transport block does not report it as configured:\n%s", block)
	}
}

// TestFullRetiresARoutedGateway. Both in-path lanes routed leaves the developer
// with two supervised daemons and an outcome they did not choose: the loopback
// base URL wins, so the relay they just installed observes nothing.
//
// `--full` therefore retires the gateway and SAYS so. The check reads the
// ROUTING rather than the election, because the election is itself a function of
// what is routed — they agree today, and this is about the machine's state.
func TestFullRetiresARoutedGateway(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	a, out, _ := testApp(map[string]string{"HOME": h.home})

	// A machine with the gateway already installed.
	if err := a.setupGateway(h.home, "127.0.0.1:18788", "https://api.anthropic.com", false); err != nil {
		t.Fatalf("setupGateway: %v", err)
	}
	if _, present := gatewayservice.CurrentEnv(h.home); !present {
		t.Fatal("the gateway install did not take")
	}
	out.Reset()

	report := a.setupLanes(laneRequest{
		telemetry: true, transport: true,
		telemetryAddr: "127.0.0.1:18789", transportAddr: "127.0.0.1:18790",
	})
	if len(report.retired) != 1 {
		t.Fatalf("the gateway was not retired: %+v\n%s", report, out.String())
	}
	if _, present := gatewayservice.CurrentEnv(h.home); present {
		t.Error("the gateway's base URL survived, so the relay just installed observes nothing")
	}
	if !strings.Contains(out.String(), "Retiring the local gateway") {
		t.Errorf("the swap was silent; retiring a daemon the developer installed has to be said:\n%s", out.String())
	}
	if got := activation.ResolveElection(gatewayservice.SettingsPath(h.home)).Elected; got != activation.LaneTransport {
		t.Errorf("after the swap the elected producer is %q, want the in-path relay", got)
	}
}
