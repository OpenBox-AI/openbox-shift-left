package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

func seed(t *testing.T, home, body string) {
	t.Helper()
	path := settingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("settings not valid JSON: %v\n%s", err, raw)
	}
	e, _ := m["env"].(map[string]any)
	return e
}

func activate(t *testing.T, home string, lane Lane, desired map[string]string) Applied {
	t.Helper()
	got, err := Activate(home, settingsPath(home), lane, desired)
	if err != nil {
		t.Fatalf("Activate(%s): %v", lane, err)
	}
	return got
}

// TestActivatePreservesEverythingItDoesNotOwn is the ownership rule this repo
// paid for once already: `init` decided what it owned by exact-match, preserved
// an entry written under a different HOME as foreign, and every governed tool
// call was stored twice. Anything outside the desired set is untouched.
func TestActivatePreservesEverythingItDoesNotOwn(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"permissions":{"allow":["Bash"]},"env":{"CORP_TOKEN_PATH":"/etc/corp/token"}}`)

	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})

	e := env(t, home)
	if e["CORP_TOKEN_PATH"] != "/etc/corp/token" {
		t.Errorf("a foreign env key was lost: %v", e["CORP_TOKEN_PATH"])
	}
	raw, _ := os.ReadFile(settingsPath(home))
	var all map[string]any
	_ = json.Unmarshal(raw, &all)
	if _, ok := all["permissions"]; !ok {
		t.Error("a top-level key outside env was dropped; the writer must round-trip what it was never taught about")
	}
}

// TestTheOriginalIsCapturedOnlyOnce is first-writer-wins, generalized.
//
// The gateway learned this the hard way: re-running init with a different
// --gateway-addr displaced OUR OWN previous URL, which was then recorded as
// "what the org had", so uninstall restored a loopback address whose daemon it
// had just unloaded — and a dead localhost fails closed, so the command meant to
// undo the relay left every model call failing while announcing a restore.
//
// In the record model that whole class disappears structurally: the original is
// whatever was there before the FIRST activation, and later activations of the
// same lane cannot overwrite it.
func TestTheOriginalIsCapturedOnlyOnce(t *testing.T) {
	const orgRelay = "https://llm-proxy.corp.internal"
	home := t.TempDir()
	seed(t, home, `{"env":{"ANTHROPIC_BASE_URL":"`+orgRelay+`"}}`)

	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:9999"})

	res, err := Deactivate(home, settingsPath(home), LaneGateway, false)
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if res.Restored["ANTHROPIC_BASE_URL"] != orgRelay {
		t.Fatalf("restored %q after two activations, want the pre-OpenBox %q", res.Restored["ANTHROPIC_BASE_URL"], orgRelay)
	}
	if env(t, home)["ANTHROPIC_BASE_URL"] != orgRelay {
		t.Error("the org's own relay was not put back into the settings file")
	}
}

// TestDeactivateDeletesAKeyThatWasNotThereBefore is the other half: with no
// original, removal means removal, not a restore of something invented.
func TestDeactivateDeletesAKeyThatWasNotThereBefore(t *testing.T) {
	home := t.TempDir()
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})

	res, err := Deactivate(home, settingsPath(home), LaneGateway, false)
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if len(res.Restored) != 0 {
		t.Errorf("restored %v out of nowhere", res.Restored)
	}
	if got := res.Removed; len(got) != 1 || got[0] != "ANTHROPIC_BASE_URL" {
		t.Errorf("removed = %v, want just the one key we wrote", got)
	}
	if _, present := env(t, home)["ANTHROPIC_BASE_URL"]; present {
		t.Error("the key we wrote survived removal")
	}
}

// TestOneLaneNeverDisturbsAnother is the reason the record is per-lane rather
// than a single managed map. `--remove-gateway` on a machine that also runs the
// transport lane must leave the transport lane working.
func TestOneLaneNeverDisturbsAnother(t *testing.T) {
	home := t.TempDir()
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	activate(t, home, LaneTransport, map[string]string{
		"HTTPS_PROXY":         "http://127.0.0.1:8790",
		"NODE_EXTRA_CA_CERTS": "/home/dev/.openbox/transport-ca.pem",
	})

	if _, err := Deactivate(home, settingsPath(home), LaneGateway, false); err != nil {
		t.Fatalf("Deactivate(gateway): %v", err)
	}
	e := env(t, home)
	if e["HTTPS_PROXY"] != "http://127.0.0.1:8790" {
		t.Errorf("removing the gateway lane took the transport lane's HTTPS_PROXY with it: %v", e["HTTPS_PROXY"])
	}
	if e["NODE_EXTRA_CA_CERTS"] != "/home/dev/.openbox/transport-ca.pem" {
		t.Errorf("removing the gateway lane disturbed the transport lane's CA path: %v", e["NODE_EXTRA_CA_CERTS"])
	}
	if _, present := e["ANTHROPIC_BASE_URL"]; present {
		t.Error("the gateway lane's own key survived its removal")
	}
}

// TestDeactivateRefusesWhenAManagedValueChangedUnderneath.
//
// Restoring over a value somebody deliberately edited destroys their edit, and
// the record cannot tell a deliberate edit from drift. Refusing names the key;
// forcing is a separate, explicit act.
func TestDeactivateRefusesWhenAManagedValueChangedUnderneath(t *testing.T) {
	home := t.TempDir()
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	seed(t, home, `{"env":{"ANTHROPIC_BASE_URL":"https://someone-elses-relay.example"}}`)

	res, err := Deactivate(home, settingsPath(home), LaneGateway, false)
	if err == nil {
		t.Fatal("Deactivate overwrote a value that had changed since activation")
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "ANTHROPIC_BASE_URL" {
		t.Errorf("conflicts = %v, want the one changed key named", res.Conflicts)
	}
	if env(t, home)["ANTHROPIC_BASE_URL"] != "https://someone-elses-relay.example" {
		t.Error("the refusal still rewrote the file")
	}

	// Forcing is the escape hatch, and it must actually restore.
	if _, err := Deactivate(home, settingsPath(home), LaneGateway, true); err != nil {
		t.Fatalf("forced Deactivate: %v", err)
	}
	if _, present := env(t, home)["ANTHROPIC_BASE_URL"]; present {
		t.Error("--force did not remove the key")
	}
}

// TestDeactivateWithNoRecordIsANoOp: removal must be safe on a machine that
// never installed the lane. `--remove-all` runs on partial state by design.
func TestDeactivateWithNoRecordIsANoOp(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"env":{"CORP_TOKEN_PATH":"/etc/corp/token"}}`)

	res, err := Deactivate(home, settingsPath(home), LaneTelemetry, false)
	if err != nil {
		t.Fatalf("Deactivate on an untouched machine: %v", err)
	}
	if len(res.Removed) != 0 || len(res.Restored) != 0 {
		t.Errorf("reported work on an untouched machine: %+v", res)
	}
	if env(t, home)["CORP_TOKEN_PATH"] != "/etc/corp/token" {
		t.Error("a no-op removal rewrote the settings file")
	}
}

// TestAnEmptyEnvBlockIsDropped keeps the gateway's shipped shape: a settings
// file that had no env block before must not gain an empty one.
func TestAnEmptyEnvBlockIsDropped(t *testing.T) {
	home := t.TempDir()
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	if _, err := Deactivate(home, settingsPath(home), LaneGateway, false); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	if _, present := all["env"]; present {
		t.Errorf("an empty env block was left behind: %s", raw)
	}
}

// TestUnparseableSettingsIsRefusedNotClobbered. A file we cannot parse is a file
// we cannot safely rewrite; overwriting destroys configuration nobody asked us
// to touch.
func TestUnparseableSettingsIsRefusedNotClobbered(t *testing.T) {
	home := t.TempDir()
	const broken = `{"env": {`
	seed(t, home, broken)

	if _, err := Activate(home, settingsPath(home), LaneGateway, map[string]string{"K": "v"}); err == nil {
		t.Fatal("Activate rewrote a settings file it could not parse")
	}
	raw, _ := os.ReadFile(settingsPath(home))
	if string(raw) != broken {
		t.Errorf("the unparseable file was modified: %s", raw)
	}
}

// TestActivateReportsWhatItReplaced. A developer or their org had that value
// pointed somewhere; a silent overwrite is how an org's own egress point
// disappears without anyone noticing.
func TestActivateReportsWhatItReplaced(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"env":{"ANTHROPIC_BASE_URL":"https://llm-proxy.corp.internal"}}`)
	res := activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	if len(res.Replaced) != 1 {
		t.Fatalf("Replaced = %v, want the one displaced key", res.Replaced)
	}
	if !contains(res.Replaced[0], "llm-proxy.corp.internal") || !contains(res.Replaced[0], "127.0.0.1:8788") {
		t.Errorf("the report does not name both values: %q", res.Replaced[0])
	}
	// Writing the same value again is not a replacement.
	res = activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	if len(res.Replaced) != 0 {
		t.Errorf("an idempotent re-write reported %v as replaced", res.Replaced)
	}
}

// TestTheRecordIsOwnerOnly — it lives beside credentials and names every key we
// touched. It is integrity evidence rather than a secret, but a displaced value
// can itself carry a credential (an org relay URL with an embedded token).
func TestTheRecordIsOwnerOnly(t *testing.T) {
	home := t.TempDir()
	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	fi, err := os.Stat(RecordPath(home))
	if err != nil {
		t.Fatalf("no activation record was written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("activation record mode = %o, want 600", perm)
	}
}

// TestActiveLanesReportsWhatIsInstalled — the election is computed from this, so
// a lane that was removed must stop counting immediately.
func TestActiveLanesReportsWhatIsInstalled(t *testing.T) {
	home := t.TempDir()
	if got := ActiveLanes(home); len(got) != 0 {
		t.Fatalf("ActiveLanes on an untouched machine = %v", got)
	}
	activate(t, home, LaneTelemetry, map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1"})
	activate(t, home, LaneTransport, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:8790"})
	if got := ActiveLanes(home); len(got) != 2 {
		t.Fatalf("ActiveLanes = %v, want both installed lanes", got)
	}
	if _, err := Deactivate(home, settingsPath(home), LaneTransport, false); err != nil {
		t.Fatal(err)
	}
	got := ActiveLanes(home)
	if len(got) != 1 || got[0] != LaneTelemetry {
		t.Fatalf("ActiveLanes after removing transport = %v, want just telemetry", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
