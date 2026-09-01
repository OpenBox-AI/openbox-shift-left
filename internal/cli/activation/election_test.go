package activation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedSettings(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestARelativeSettingsPathIsNamedRatherThanReportedAsUnrouted is defect 6a, and
// it is the diagnostic that would have caught it on day one.
//
// The launchd plist carries no HOME, so the daemon's homeDir() yielded "" and
// SettingsPath("") produced the RELATIVE ".claude/settings.json", which launchd
// resolves against /. The read failed, and the election reported "no lane is
// routed in the tool's settings" -- the one outcome that reads as a deliberate
// configuration choice rather than a process that cannot see the file.
func TestARelativeSettingsPathIsNamedRatherThanReportedAsUnrouted(t *testing.T) {
	e := ResolveElection(filepath.Join(".claude", "settings.json"))

	if e.Usable() {
		t.Fatal("a relative settings path was treated as a readable one")
	}
	if !strings.Contains(e.SettingsProblem, "not absolute") {
		t.Errorf("the problem does not name what is wrong: %q", e.SettingsProblem)
	}
	if !strings.Contains(e.SettingsProblem, ".claude") {
		t.Errorf("the problem does not name the path: %q", e.SettingsProblem)
	}
	if strings.Contains(e.Reason, "no lane is routed") {
		t.Errorf("the reason still reads as a configuration choice: %q", e.Reason)
	}
	if e.Elected != "" {
		t.Errorf("a lane was elected from a file that was never read: %q", e.Elected)
	}
}

// TestAnEmptySettingsPathIsNamedToo the same failure one step earlier: a daemon
// whose home could not be resolved at all.
func TestAnEmptySettingsPathIsNamedToo(t *testing.T) {
	e := ResolveElection("")
	if e.Usable() {
		t.Fatal("an empty settings path was treated as readable")
	}
	if !strings.Contains(e.SettingsProblem, "could not be resolved") {
		t.Errorf("problem = %q", e.SettingsProblem)
	}
}

// TestAnUnparseableSettingsFileIsDistinctFromAnAbsentOne a file that exists and
// cannot be understood is the loudest of these states and the least likely to be
// noticed, because everything downstream sees an empty env block either way.
func TestAnUnparseableSettingsFileIsDistinctFromAnAbsentOne(t *testing.T) {
	dir := t.TempDir()
	path := seedSettings(t, dir, `{"env": not json at all`)

	broken := ResolveElection(path)
	if broken.Usable() {
		t.Fatal("an unparseable settings file was treated as readable")
	}
	if !strings.Contains(broken.SettingsProblem, path) {
		t.Errorf("the problem does not name the file: %q", broken.SettingsProblem)
	}

	// An ABSENT file, by contrast, is the normal state of a machine with nothing
	// installed and must stay quiet: reporting it as a fault would train a reader
	// to ignore the report.
	absent := ResolveElection(filepath.Join(t.TempDir(), ".claude", "settings.json"))
	if !absent.Usable() {
		t.Errorf("an absent settings file was reported as a problem: %q", absent.SettingsProblem)
	}
	if !strings.Contains(absent.Reason, "no lane is routed") {
		t.Errorf("an absent file should read as nothing routed: %q", absent.Reason)
	}
}

// TestTheElectionResolvesTheSameFromADaemonAsFromAShell is the requirement in one
// assertion: the answer must not depend on the reader's environment. The path is
// absolute here precisely because that is what the unit now carries.
func TestTheElectionResolvesTheSameFromADaemonAsFromAShell(t *testing.T) {
	dir := t.TempDir()
	path := seedSettings(t, dir, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`)

	// A daemon has no HOME at all. The election must not consult one.
	t.Setenv("HOME", "")
	e := ResolveElection(path)

	if !e.Usable() {
		t.Fatalf("the election could not read an absolute path with no HOME set: %q", e.SettingsProblem)
	}
	if e.Elected != LaneTransport {
		t.Errorf("elected %q, want transport: HTTPS_PROXY at loopback routes it and nothing outranks it", e.Elected)
	}
}

// TestBothInPathLanesRoutedElectsExactlyOne is the case nothing enforced. Route
// both ANTHROPIC_BASE_URL and HTTPS_PROXY at loopback and both in-path lanes see
// the call; candidateLanes already reasoned about this and its conclusion was
// enforced nowhere.
func TestBothInPathLanesRoutedElectsExactlyOne(t *testing.T) {
	dir := t.TempDir()
	path := seedSettings(t, dir, `{"env":{
		"HTTPS_PROXY":"http://127.0.0.1:8790",
		"ANTHROPIC_BASE_URL":"http://127.0.0.1:8788"
	}}`)

	e := ResolveElection(path)
	if len(e.Routed) != 2 {
		t.Fatalf("routed = %v, want both in-path lanes", e.Routed)
	}
	// Transport is excluded from the candidates on purpose: with a base URL set the
	// call goes straight to the gateway, so transport observes nothing and naming
	// it would attribute every turn to a lane that never saw one.
	if e.Elected != LaneGateway {
		t.Errorf("elected %q, want gateway; a base URL takes the transport relay out of the path", e.Elected)
	}
	if len(e.Candidates) != 1 || e.Candidates[0] != LaneGateway {
		t.Errorf("candidates = %v, want exactly [gateway]", e.Candidates)
	}
}

// TestTheElectionIsReDerivedPerCall asserts the SECOND resolution, not the first.
//
// Install ordering starts a lane daemon before the env var is written -- unit,
// start, prove it listens, then env -- so the startup election legitimately sees
// no routed lane. An answer cached at startup would therefore silence the lane
// for its entire lifetime. This repo has paid for a first-invocation-only test
// once already: fifteen green tests missed a defect because each ran init exactly
// once.
func TestTheElectionIsReDerivedPerCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	// First resolution: the file does not exist yet, exactly as during install.
	if first := ResolveElection(path); first.Elected != "" {
		t.Fatalf("elected %q before any lane was routed", first.Elected)
	}

	seedSettings(t, dir, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`)

	if second := ResolveElection(path); second.Elected != LaneTransport {
		t.Errorf("the second resolution elected %q, want transport. The answer is cached, so a "+
			"lane started before its env var was written stays silent forever.", second.Elected)
	}

	// And back again, because un-routing is the observed failure mode: a
	// settings.json lost OpenBox's whole env block during one planning session
	// while the units stayed installed and listening.
	if err := os.WriteFile(path, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if third := ResolveElection(path); third.Elected != "" {
		t.Errorf("the third resolution still elects %q after the env block was removed", third.Elected)
	}
}
