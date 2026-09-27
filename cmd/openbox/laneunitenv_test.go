package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// TestLaneUnitEnvCarriesTraceDirAndLeavesArgsUnchanged is phase 3's own
// contract: the trace directory rides the unit's Env (rendered separately by
// LaunchdPlist/SystemdUnit), never Args -- so a machine with tracing off
// (trace.Dir() == "") gets byte-identical argv to one with it on, and every
// existing Args-shaped assertion (laneunitargv_test.go) stays true unchanged.
func TestLaneUnitEnvCarriesTraceDirAndLeavesArgsUnchanged(t *testing.T) {
	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()

	a, _, _ := testApp(nil)
	bare := laneservice.Telemetry(telemetry.DefaultAddr, "settings.json", true)
	traced := bare.WithEnv(a.laneUnitEnv())

	if len(traced.Args) != len(bare.Args) {
		t.Fatalf("WithEnv changed Args length: %v vs %v", traced.Args, bare.Args)
	}
	for i := range bare.Args {
		if traced.Args[i] != bare.Args[i] {
			t.Fatalf("WithEnv changed Args[%d]: %v vs %v", i, traced.Args[i], bare.Args[i])
		}
	}

	plist := traced.LaunchdPlist("/home/dev", "/usr/local/bin/openbox")
	if !strings.Contains(plist, trace.EnvDir) || !strings.Contains(plist, traceDir) {
		t.Fatalf("launchd plist does not carry %s=%s:\n%s", trace.EnvDir, traceDir, plist)
	}
	unit := traced.SystemdUnit("/usr/local/bin/openbox")
	if !strings.Contains(unit, trace.EnvDir) || !strings.Contains(unit, traceDir) {
		t.Fatalf("systemd unit does not carry %s=%s:\n%s", trace.EnvDir, traceDir, unit)
	}
}

// TestLaneUnitsCarryTheInstallersCoordinates is the regression test for a
// shipped defect: a developer who set OPENBOX_HOME — which README.md documents
// — could not install transport at all.
//
// The install process resolves and validates the CA under its own
// $OPENBOX_HOME, but the unit carried no environment, so the daemon launchd
// started resolved the real ~/.openbox and minted its CA there instead. The
// activate closure then refused to point NODE_EXTRA_CA_CERTS at a file that
// was not where it checked, and the whole install rolled back. Asserting the
// unit is the only place this is visible: every in-process check agrees with
// itself.
func TestLaneUnitsCarryTheInstallersCoordinates(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	nothingIsListening(t)

	openboxHome := filepath.Join(h.home, ".openbox")
	spool := filepath.Join(h.home, "spool")
	sessionDir := filepath.Join(h.home, "sessions")
	haltDir := filepath.Join(h.home, "halted-sessions")
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv(obgit.EnvSessionDir, sessionDir)
	t.Setenv(devconfig.EnvHaltDir, haltDir)
	enforcementFile := filepath.Join(h.home, "enforcements.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, enforcementFile)

	for _, tc := range []struct {
		lane  string
		setup func(*app) error
		spec  laneservice.Spec
	}{
		{
			lane:  "telemetry",
			setup: func(a *app) error { _, err := a.setupTelemetry(h.home, telemetry.DefaultAddr, false); return err },
			spec:  laneservice.Telemetry("", "", false),
		},
		{
			lane:  "transport",
			setup: func(a *app) error { _, err := a.setupTransport(h.home, transport.DefaultAddr, false); return err },
			spec:  laneservice.Transport("", "", false),
		},
	} {
		t.Run(tc.lane, func(t *testing.T) {
			a, _, _ := testApp(map[string]string{
				"HOME":                h.home,
				devconfig.EnvHome:     openboxHome,
				devconfig.EnvSpoolDir: spool,
			})
			if err := tc.setup(a); err != nil {
				t.Fatalf("setup%s: %v", tc.lane, err)
			}
			unitPath := tc.spec.UnitPath(runtime.GOOS, h.home)
			body, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatalf("reading the written unit: %v", err)
			}
			for key, want := range map[string]string{
				devconfig.EnvHome:            openboxHome,
				devconfig.EnvSpoolDir:        spool,
				obgit.EnvSessionDir:          sessionDir,
				devconfig.EnvHaltDir:         haltDir,
				devconfig.EnvEnforcementFile: enforcementFile,
			} {
				if !strings.Contains(string(body), key) || !strings.Contains(string(body), want) {
					t.Errorf("%s unit does not carry %s=%s, so the daemon resolves a different one:\n%s",
						tc.lane, key, want, body)
				}
			}
			// Unconditional, resolved (not copied) the same way as the three
			// above: a daemon has no $HOME to derive either from.
			for _, key := range []string{devconfig.EnvSpoolRoot, advisoryFileEnvKey} {
				if !strings.Contains(string(body), key) {
					t.Errorf("%s unit does not carry %s at all:\n%s", tc.lane, key, body)
				}
			}
		})
	}
}

// TestLaneUnitsCarryNoConditionalEnvironmentWhenTheInstallerHadNone. The
// default machine sets neither OPENBOX_HOME nor OPENBOX_SPOOL_DIR, and a unit
// that gained either there would rewrite every developer's unit file on the
// next install for no behavioural reason. OPENBOX_SESSION_DIR and
// OPENBOX_HALT_DIR are NOT conditional: each is always resolved and always
// carried, because a daemon has no $HOME to resolve it from otherwise -- see
// TestLaneUnitEnvCarriesOnlyCoordinates for what an install with real
// credentials set must still keep OUT of the unit.
func TestLaneUnitsCarryNoConditionalEnvironmentWhenTheInstallerHadNone(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	nothingIsListening(t)

	// The harness pins OPENBOX_HOME for its own isolation; this case is about an
	// installer that read nothing beyond that, which is what a.getenv reports
	// for the two CONDITIONAL keys.
	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	unitPath := laneservice.Transport("", "", false).UnitPath(runtime.GOOS, h.home)
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the written unit: %v", err)
	}
	if !strings.Contains(string(body), obgit.EnvSessionDir) {
		t.Errorf("the unit does not carry %s, which is unconditional (a daemon has no $HOME):\n%s", obgit.EnvSessionDir, body)
	}
	if !strings.Contains(string(body), devconfig.EnvHaltDir) {
		t.Errorf("the unit does not carry %s, which is unconditional (a daemon has no $HOME):\n%s", devconfig.EnvHaltDir, body)
	}
	if !strings.Contains(string(body), devconfig.EnvSpoolRoot) {
		t.Errorf("the unit does not carry %s, which is unconditional (a daemon has no $HOME):\n%s", devconfig.EnvSpoolRoot, body)
	}
	if !strings.Contains(string(body), advisoryFileEnvKey) {
		t.Errorf("the unit does not carry %s, which is unconditional (a daemon has no $HOME):\n%s", advisoryFileEnvKey, body)
	}
	if strings.Contains(string(body), devconfig.EnvSpoolDir) {
		t.Errorf("the unit carries %s though the installer set nothing:\n%s", devconfig.EnvSpoolDir, body)
	}
}

// TestLaneUnitEnvCarriesOnlyCoordinates. A unit file is world-readable, and
// ~/.openbox/.env holds the signing seed and the obx_ key. Only path
// coordinates may travel in a unit — the same one-store-per-field rule that
// keeps secrets out of dev.json.
func TestLaneUnitEnvCarriesOnlyCoordinates(t *testing.T) {
	dir := t.TempDir()
	haltDir := t.TempDir()
	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()
	t.Setenv(obgit.EnvSessionDir, dir)
	t.Setenv(devconfig.EnvHaltDir, haltDir)
	enforcementFile := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, enforcementFile)
	a, _, _ := testApp(map[string]string{
		devconfig.EnvHome:            "/h",
		devconfig.EnvSpoolDir:        "/s",
		devconfig.EnvAPIKeyDirect:    "obx_secret",
		devconfig.EnvAgentPrivateKey: "seed",
		devconfig.EnvControlToken:    "token",
	})
	env := a.laneUnitEnv()
	if len(env) != 8 || env[devconfig.EnvHome] != "/h" || env[devconfig.EnvSpoolDir] != "/s" ||
		env[obgit.EnvSessionDir] != dir || env[devconfig.EnvHaltDir] != haltDir ||
		env[devconfig.EnvEnforcementFile] != enforcementFile ||
		env[devconfig.EnvSpoolRoot] == "" || env[advisoryFileEnvKey] == "" ||
		env[trace.EnvDir] != traceDir {
		t.Fatalf("laneUnitEnv carried %v; want exactly the eight path coordinates", env)
	}
	for _, v := range env {
		for _, secret := range []string{"obx_secret", "seed", "token"} {
			if strings.Contains(v, secret) {
				t.Errorf("a credential reached a unit file through %q", v)
			}
		}
	}
}
