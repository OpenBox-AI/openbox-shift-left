package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	t.Setenv(devconfig.EnvSpoolDir, spool)

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
			} {
				if !strings.Contains(string(body), key) || !strings.Contains(string(body), want) {
					t.Errorf("%s unit does not carry %s=%s, so the daemon resolves a different one:\n%s",
						tc.lane, key, want, body)
				}
			}
		})
	}
}

// TestLaneUnitsCarryNoEnvironmentWhenTheInstallerHadNone. The default machine
// sets neither variable, and a unit that gained a block there would rewrite
// every developer's unit file on the next install for no behavioural reason.
func TestLaneUnitsCarryNoEnvironmentWhenTheInstallerHadNone(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	h.seedCA(t)
	nothingIsListening(t)

	// The harness pins OPENBOX_HOME for its own isolation; this case is about an
	// installer that read nothing, which is what a.getenv reports here.
	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if err := a.setupTransport(h.home, transport.DefaultAddr, false); err != nil {
		t.Fatalf("setupTransport: %v", err)
	}
	unitPath := laneservice.Transport("", "", false).UnitPath(runtime.GOOS, h.home)
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the written unit: %v", err)
	}
	for _, marker := range []string{"EnvironmentVariables", "Environment="} {
		if strings.Contains(string(body), marker) {
			t.Errorf("the unit renders %s for an installer that set nothing:\n%s", marker, body)
		}
	}
}

// TestLaneUnitEnvCarriesOnlyCoordinates. A unit file is world-readable, and
// ~/.openbox/.env holds the signing seed and the obx_ key. Only path
// coordinates may travel in a unit — the same one-store-per-field rule that
// keeps secrets out of dev.json.
func TestLaneUnitEnvCarriesOnlyCoordinates(t *testing.T) {
	a, _, _ := testApp(map[string]string{
		devconfig.EnvHome:            "/h",
		devconfig.EnvSpoolDir:        "/s",
		devconfig.EnvAPIKeyDirect:    "obx_secret",
		devconfig.EnvAgentPrivateKey: "seed",
		devconfig.EnvControlToken:    "token",
	})
	env := a.laneUnitEnv()
	if len(env) != 2 || env[devconfig.EnvHome] != "/h" || env[devconfig.EnvSpoolDir] != "/s" {
		t.Fatalf("laneUnitEnv carried %v; want exactly the two path coordinates", env)
	}
	for _, v := range env {
		for _, secret := range []string{"obx_secret", "seed", "token"} {
			if strings.Contains(v, secret) {
				t.Errorf("a credential reached a unit file through %q", v)
			}
		}
	}
}
