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
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

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
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv(obgit.EnvSessionDir, sessionDir)

	for _, tc := range []struct {
		lane  string
		setup func(*app) error
		spec  laneservice.Spec
	}{
		{
			lane:  "telemetry",
			setup: func(a *app) error { return a.setupTelemetry(h.home, telemetry.DefaultAddr, false) },
			spec:  laneservice.Telemetry("", "", false),
		},
		{
			lane:  "transport",
			setup: func(a *app) error { return a.setupTransport(h.home, transport.DefaultAddr, false) },
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
				devconfig.EnvHome:     openboxHome,
				devconfig.EnvSpoolDir: spool,
				obgit.EnvSessionDir:   sessionDir,
			} {
				if !strings.Contains(string(body), key) || !strings.Contains(string(body), want) {
					t.Errorf("%s unit does not carry %s=%s, so the daemon resolves a different one:\n%s",
						tc.lane, key, want, body)
				}
			}
		})
	}
}

// TestLaneUnitsCarryNoConditionalEnvironmentWhenTheInstallerHadNone. The
// default machine sets neither OPENBOX_HOME nor OPENBOX_SPOOL_DIR, and a unit
// that gained either there would rewrite every developer's unit file on the
// next install for no behavioural reason. OPENBOX_SESSION_DIR is NOT
// conditional (phase 08, insight 7): it is always resolved and always
// carried, because a daemon has no $HOME to resolve it from otherwise --
// see TestLaneUnitEnvCarriesOnlyCoordinates for what an install with real
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
	if err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
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
	t.Setenv(obgit.EnvSessionDir, dir)
	a, _, _ := testApp(map[string]string{
		devconfig.EnvHome:            "/h",
		devconfig.EnvSpoolDir:        "/s",
		devconfig.EnvAPIKeyDirect:    "obx_secret",
		devconfig.EnvAgentPrivateKey: "seed",
		devconfig.EnvControlToken:    "token",
	})
	env := a.laneUnitEnv()
	if len(env) != 3 || env[devconfig.EnvHome] != "/h" || env[devconfig.EnvSpoolDir] != "/s" || env[obgit.EnvSessionDir] != dir {
		t.Fatalf("laneUnitEnv carried %v; want exactly the three path coordinates", env)
	}
	for _, v := range env {
		for _, secret := range []string{"obx_secret", "seed", "token"} {
			if strings.Contains(v, secret) {
				t.Errorf("a credential reached a unit file through %q", v)
			}
		}
	}
}
