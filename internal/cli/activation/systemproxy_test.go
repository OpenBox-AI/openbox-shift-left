package activation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordLoadsAnOldFileWithNoSystemField is the back-compat requirement:
// a record written before the system field existed carries no "system" key at all, and
// loading it must leave Record.System nil rather than erroring -- no schema
// bump, no migration step.
func TestRecordLoadsAnOldFileWithNoSystemField(t *testing.T) {
	home := t.TempDir()
	path := RecordPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := `{"schema":"openbox.dev-runtime.activation/v1","lanes":{"transport":{"managed":{"HTTP_PROXY":"http://127.0.0.1:8790"},"original":{},"settings_path":"/tmp/settings.json","activated_at":"2026-01-01T00:00:00Z","before_sha256":"a","after_sha256":"b"}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entry, err := LoadSystemEntry(home)
	if err != nil {
		t.Fatalf("LoadSystemEntry on a pre-existing record with no system field: %v", err)
	}
	if entry != nil {
		t.Errorf("System = %+v, want nil for a record that never had one", entry)
	}

	// The lane entry that was already there must survive untouched.
	record, err := loadRecord(home)
	if err != nil {
		t.Fatalf("loadRecord: %v", err)
	}
	if record.Lanes[LaneTransport] == nil || record.Lanes[LaneTransport].SettingsPath != "/tmp/settings.json" {
		t.Errorf("loading an old record for its System field lost or altered an existing lane entry: %+v", record.Lanes)
	}
}

// TestPersistAndLoadAndClearSystemEntryRoundTrip covers the write/read/clear
// cycle a caller (Activate, doctor, a full uninstall) depends on, and that a
// lane entry activated separately is preserved through all three.
func TestPersistAndLoadAndClearSystemEntryRoundTrip(t *testing.T) {
	home := t.TempDir()
	if _, err := Activate(home, filepath.Join(home, "settings.json"), LaneGateway,
		map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	entry := &SystemEntry{Schema: systemEntrySchema, PACActivated: true, PACURL: "http://127.0.0.1:8790/proxy.pac"}
	if err := persistSystemEntry(home, entry); err != nil {
		t.Fatalf("persistSystemEntry: %v", err)
	}

	got, err := LoadSystemEntry(home)
	if err != nil {
		t.Fatalf("LoadSystemEntry: %v", err)
	}
	if got == nil || got.PACURL != entry.PACURL || !got.PACActivated {
		t.Fatalf("got %+v, want %+v", got, entry)
	}

	raw, err := os.ReadFile(RecordPath(home))
	if err != nil {
		t.Fatalf("reading the record file: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if _, ok := onDisk["schema"].(string); !ok {
		t.Error("Record.Schema was not persisted")
	}

	if err := ClearSystemEntry(home); err != nil {
		t.Fatalf("ClearSystemEntry: %v", err)
	}
	got, err = LoadSystemEntry(home)
	if err != nil {
		t.Fatalf("LoadSystemEntry after clear: %v", err)
	}
	if got != nil {
		t.Errorf("System entry survived ClearSystemEntry: %+v", got)
	}

	// The lane entry activated at the top of this test must still be there.
	if ActiveLanes(home)[0] != LaneGateway {
		t.Error("ClearSystemEntry disturbed an unrelated lane entry")
	}

	// Clearing an already-clear record is a no-op, not an error.
	if err := ClearSystemEntry(home); err != nil {
		t.Fatalf("ClearSystemEntry on an already-nil System: %v", err)
	}
}

// TestExecRunnerRunsRealArgvWithNoShellLayer proves ExecRunner is argv-only:
// a shell metacharacter in an argument must reach the child process
// literally, never be interpreted, and this never touches a privileged
// binary -- `echo` is not sudo, networksetup or security.
func TestExecRunnerRunsRealArgvWithNoShellLayer(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "a;b|c$(d)")
	if err != nil {
		t.Fatalf("ExecRunner(echo): %v", err)
	}
	got := string(out)
	if got != "a;b|c$(d)\n" {
		t.Errorf("got %q, want the argument echoed back literally with no shell expansion", got)
	}
}

// TestCATrustPresentMatchesOnlyItsOwnSHA1 is the read-only half of the
// CA-reissue-vs-activation check: it must recognise a SHA-1 that is in the
// find-certificate output and reject one that is not, and it must never
// request elevation (the fake Runner here answers with no sudo wrapper at
// all).
func TestCATrustPresentMatchesOnlyItsOwnSHA1(t *testing.T) {
	const sha1 = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "security" || len(args) == 0 || args[0] != "find-certificate" {
			t.Fatalf("unexpected call: %s %v", name, args)
		}
		return []byte("SHA-1 hash: " + sha1 + "\n"), nil
	}

	present, err := CATrustPresent(context.Background(), run, sha1)
	if err != nil {
		t.Fatalf("CATrustPresent: %v", err)
	}
	if !present {
		t.Error("CATrustPresent = false, want true for a SHA-1 that is in the output")
	}

	present, err = CATrustPresent(context.Background(), run, "0000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("CATrustPresent: %v", err)
	}
	if present {
		t.Error("CATrustPresent = true for a SHA-1 that was never in the output")
	}
}
