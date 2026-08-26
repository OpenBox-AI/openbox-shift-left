package gatewaycheck

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dialWait = 400 * time.Millisecond

// writeSettings lays down a settings.json with an env block.
func writeSettings(t *testing.T, path, baseURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"env":{"ANTHROPIC_BASE_URL":"` + baseURL + `"},"someOtherKey":true}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// listener starts a throwaway loopback listener and returns its address.
func listener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// TestNoConfigurationIsReportedAsUngoverned is the case that matters most in
// practice: nothing configured means model calls never touch the gateway, and the
// report has to say so rather than look healthy because no check failed.
func TestNoConfigurationIsReportedAsUngoverned(t *testing.T) {
	r := Inspect(t.TempDir(), filepath.Join(t.TempDir(), "managed-settings.json"), dialWait)

	if r.Tier != TierNone {
		t.Errorf("Tier = %q want %q", r.Tier, TierNone)
	}
	if r.SettingsPath != "" || r.ConfiguredAddr != "" {
		t.Errorf("found configuration where none exists: path=%q addr=%q", r.SettingsPath, r.ConfiguredAddr)
	}
	if !r.BypassCapable {
		t.Error("an unconfigured machine is entirely ungoverned for model calls and must report bypass exposure")
	}
	if !strings.Contains(strings.Join(r.BypassNotes, " "), "gateway spans") {
		t.Errorf("the note does not tell a reader how to detect this in stored data: %v", r.BypassNotes)
	}
}

// TestUserOwnedConfigIsBaseTier — user-owned config is tamper-evident, not
// tamper-resistant, and the report must never imply otherwise.
func TestUserOwnedConfigIsBaseTier(t *testing.T) {
	home := t.TempDir()
	addr := listener(t)
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), "http://"+addr)

	r := Inspect(home, filepath.Join(t.TempDir(), "managed-settings.json"), dialWait)

	if r.Tier != TierBase {
		t.Errorf("Tier = %q want %q", r.Tier, TierBase)
	}
	if !r.Alive {
		t.Errorf("live listener reported unreachable: %s", r.AliveErr)
	}
	if !r.TargetsGateway {
		t.Errorf("loopback target not recognised: %q", r.ConfiguredAddr)
	}
	if !r.BypassCapable {
		t.Error("base tier must always report bypass as detectable, not prevented")
	}
	notes := strings.Join(r.BypassNotes, " ")
	if !strings.Contains(notes, "DETECTABLE") {
		t.Errorf("base-tier note does not frame this as detection: %v", r.BypassNotes)
	}
}

// TestManagedPathWithoutRootOwnershipIsNotTheMDMTier is the overstatement guard.
// An org can push the same bytes to the managed path; only ROOT ownership makes it
// the MDM tier, and reporting otherwise would claim assurance this build cannot
// observe.
func TestManagedPathWithoutRootOwnershipIsNotTheMDMTier(t *testing.T) {
	home := t.TempDir()
	managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
	addr := listener(t)
	writeSettings(t, managedPath, "http://"+addr)

	r := Inspect(home, managedPath, dialWait)

	if r.SettingsPath != managedPath {
		t.Fatalf("managed file not picked up: %q", r.SettingsPath)
	}
	// The test process is not root, so the file it just wrote is user-owned.
	if r.OwnerUID == 0 {
		t.Skip("running as root; this test needs a non-root owner to be meaningful")
	}
	if r.Tier != TierBase {
		t.Errorf("Tier = %q; a user-owned file at the managed path is the BASE tier", r.Tier)
	}
	notes := strings.Join(r.BypassNotes, " ")
	if !strings.Contains(notes, "not root") {
		t.Errorf("the downgrade is not explained: %v", r.BypassNotes)
	}
}

// TestManagedPrecedesUserSettings — a managed value cannot be overridden by the
// user file, so it is what the tool actually reads and what doctor must report.
func TestManagedPrecedesUserSettings(t *testing.T) {
	home := t.TempDir()
	managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
	writeSettings(t, managedPath, "http://127.0.0.1:9001")
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), "http://127.0.0.1:9002")

	r := Inspect(home, managedPath, dialWait)
	if r.ConfiguredAddr != "http://127.0.0.1:9001" {
		t.Errorf("ConfiguredAddr = %q; the managed value must win", r.ConfiguredAddr)
	}
	if r.SettingsPath != managedPath {
		t.Errorf("SettingsPath = %q want the managed file", r.SettingsPath)
	}
}

// TestNonLoopbackTargetIsFlagged catches the case where the gateway is healthy and
// completely unused because the tool points somewhere else. "Alive" and "actually
// used" are different claims.
func TestNonLoopbackTargetIsFlagged(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), "https://api.anthropic.com")

	r := Inspect(home, "", dialWait)
	if r.TargetsGateway {
		t.Error("a provider URL was treated as pointing at the local gateway")
	}
	if !strings.Contains(strings.Join(r.BypassNotes, " "), "not loopback") {
		t.Errorf("the note does not name the mismatch: %v", r.BypassNotes)
	}
	// The port-defaulting half. This URL has no explicit port and is perfectly
	// valid — a real client connects on 443. Requiring one made the dial fail, so
	// the report described a WORKING, ungoverned configuration as "model calls
	// will FAIL rather than escape, the safe direction": exactly backwards, and
	// this test previously pinned that wrong claim by not looking.
	if !r.Alive {
		t.Errorf("api.anthropic.com reported unreachable (%s) — the port was not defaulted from the scheme, so a working ungoverned config reads as failing safe", r.AliveErr)
	}
	if strings.Contains(strings.Join(r.BypassNotes, " "), "safe direction") {
		t.Errorf("a reachable provider URL is being described as failing safe: %v", r.BypassNotes)
	}
}

// TestDeadGatewayNamesTheSafeDirection — a dead gateway fails model calls rather
// than letting them escape, and the report should say that so a developer does not
// "fix" it by unsetting the variable.
func TestDeadGatewayNamesTheSafeDirection(t *testing.T) {
	home := t.TempDir()
	// Port 1 on loopback: nothing listens and connecting fails fast.
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), "http://127.0.0.1:1")

	r := Inspect(home, "", dialWait)
	if r.Alive {
		t.Error("reported a dead port as reachable")
	}
	if r.AliveErr == "" {
		t.Error("no reason given for unreachability")
	}
	if !strings.Contains(strings.Join(r.BypassNotes, " "), "safe direction") {
		t.Errorf("the note does not explain the fail direction: %v", r.BypassNotes)
	}
}

// TestReportNeverClaimsPrevention is the wording control, and it is the whole
// point of this package. The base assurance claim is DETECTION; any output saying
// bypass is impossible would be the overstatement this product exists to prevent.
func TestReportNeverClaimsPrevention(t *testing.T) {
	home := t.TempDir()
	managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
	addr := listener(t)

	for _, setup := range []func(){
		func() {},
		func() { writeSettings(t, filepath.Join(home, ".claude", "settings.json"), "http://"+addr) },
		func() { writeSettings(t, managedPath, "http://"+addr) },
	} {
		setup()
		r := Inspect(home, managedPath, dialWait)
		all := strings.ToLower(strings.Join(r.BypassNotes, " "))
		// Affirmative CLAIMS, not the vocabulary. "not prevented" and "Prevention
		// needs the org's MDM" are exactly the honest phrasings, so a check that
		// banned the word "prevented" would push the wording in the wrong
		// direction — which is how this assertion first failed.
		for _, forbidden := range []string{
			"cannot bypass", "cannot be bypassed", "is prevented", "bypass is prevented",
			"impossible", "tamper-proof", "tamper proof", "guaranteed", "fully prevented",
			"no way to bypass",
		} {
			if strings.Contains(all, forbidden) {
				t.Errorf("report claims prevention with %q: %v", forbidden, r.BypassNotes)
			}
		}
		// The honest framing must actually be present, not merely un-forbidden.
		if !strings.Contains(all, "detectable") && !strings.Contains(all, "detect") {
			t.Errorf("no note frames this as detection: %v", r.BypassNotes)
		}
		// And it must never go quiet: silence reads as prevention.
		if len(r.BypassNotes) == 0 {
			t.Error("no bypass note at all; silence trains a reader to assume prevention")
		}
	}
}

// TestUnparseableSettingsDegradesNotLies — doctor must lose information rather
// than make a wrong claim.
func TestUnparseableSettingsDegradesNotLies(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	r := Inspect(home, "", dialWait)
	if r.Tier != TierNone {
		t.Errorf("Tier = %q; an unreadable file is 'not configured here', not a tier", r.Tier)
	}
	if !r.BypassCapable {
		t.Error("an unparseable config must not read as governed")
	}
}

// TestPortIsDefaultedFromTheScheme is the unit-level half of the same finding,
// isolated so it does not depend on network reachability.
func TestPortIsDefaultedFromTheScheme(t *testing.T) {
	cases := map[string][2]string{
		"https://api.anthropic.com":      {"api.anthropic.com", "443"},
		"http://127.0.0.1:8788":          {"127.0.0.1", "8788"},
		"https://api.anthropic.com/v1":   {"api.anthropic.com", "443"},
		"http://localhost":               {"localhost", "80"},
		"https://gw.internal:8443/relay": {"gw.internal", "8443"},
	}
	for in, want := range cases {
		h, p := hostPort(in)
		if h != want[0] || p != want[1] {
			t.Errorf("hostPort(%q) = %q,%q want %q,%q", in, h, p, want[0], want[1])
		}
	}
}

// TestUnknownOwnerIsNotReportedAsNonRoot covers the Windows path, which has no uid
// to read. -1 means UNKNOWN; treating it as "not root" printed a confident false
// claim ("owned by uid -1, not root — the developer can rewrite it") about a file
// that may be properly locked down.
func TestUnknownOwnerIsNotReportedAsNonRoot(t *testing.T) {
	orig := statUID
	statUID = func(fs.FileInfo) int { return -1 }
	t.Cleanup(func() { statUID = orig })

	managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
	writeSettings(t, managedPath, "http://127.0.0.1:8788")

	r := Inspect(t.TempDir(), managedPath, dialWait)
	if r.OwnerUID != -1 {
		t.Fatalf("OwnerUID = %d, want -1 for this fixture", r.OwnerUID)
	}
	notes := strings.Join(r.BypassNotes, " ")
	if strings.Contains(notes, "not root") {
		t.Errorf("unknown ownership reported as confirmed non-root: %v", r.BypassNotes)
	}
	if !strings.Contains(notes, "cannot be CONFIRMED") {
		t.Errorf("the note does not admit it could not tell: %v", r.BypassNotes)
	}
	// It still must not claim the MDM tier it cannot observe.
	if r.Tier == TierMDM {
		t.Error("claimed the MDM tier without being able to check ownership")
	}
}

// TestLoopbackSpellingIsCaseInsensitive.
//
// DNS names are case-insensitive and the gateway's own validator treats them that
// way (gateway/config.go isLoopbackSpelling uses EqualFold), so `LOCALHOST:8788`
// passed validation and was written to the settings file — and doctor then
// reported "NOT loopback — this machine is pointed at something else" about a
// machine it had just configured, while also reporting it reachable. This
// package's rule is that doctor degrades to LESS information, never to a wrong
// claim.
func TestLoopbackSpellingIsCaseInsensitive(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "LocalHost", "127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			home := t.TempDir()
			target := "http://" + host + ":8788"
			if host == "::1" {
				target = "http://[::1]:8788"
			}
			writeSettings(t, filepath.Join(home, ".claude", "settings.json"), target)
			r := Inspect(home, "", dialWait)
			if !r.TargetsGateway {
				t.Errorf("%s reported as not loopback; doctor would tell a correctly "+
					"configured developer their machine is pointed elsewhere", target)
			}
		})
	}
}
