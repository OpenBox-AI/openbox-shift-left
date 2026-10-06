package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// settingsFixture writes a Claude Code user-scope settings file at
// <home>/.claude/settings.json holding whatever the caller wants under
// ThinkingSummariesKey (omitted when body is ""), and returns its path.
func settingsFixture(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// thinkingSummariesValue extracts what the settings file currently holds at
// ThinkingSummariesKey, "<absent>" when the key is not there at all.
func thinkingSummariesValue(t *testing.T, path string) string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mustReadFile(t, path)), &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	raw, ok := doc[ThinkingSummariesKey]
	if !ok {
		return "<absent>"
	}
	return string(raw)
}

// TestWriteThinkingSummariesFourRowStateMatrix is the top-level acceptance
// table: whatever showThinkingSummaries held before `init` -- absent, false,
// true, or a non-boolean value a developer typed -- `init` forces it to true,
// and a later `uninstall` (RestoreThinkingSummaries, called directly here to
// isolate this from the removal surfaces `openbox uninstall` also walks) puts
// exactly that back.
func TestWriteThinkingSummariesFourRowStateMatrix(t *testing.T) {
	cases := []struct {
		name           string
		body           string // the settings file's full content before writeThinkingSummaries
		wantAfterWrite string // value of the key after writeThinkingSummaries, JSON literal
		wantRestored   string // "<absent>" or a JSON literal, after RestoreThinkingSummaries
	}{
		{
			name:           "absent",
			body:           `{"other": 1}`,
			wantAfterWrite: "true",
			wantRestored:   "<absent>",
		},
		{
			name:           "false",
			body:           `{"showThinkingSummaries": false, "other": 1}`,
			wantAfterWrite: "true",
			wantRestored:   "false",
		},
		{
			name:           "true (developer-set)",
			body:           `{"showThinkingSummaries": true, "other": 1}`,
			wantAfterWrite: "true",
			wantRestored:   "true",
		},
		{
			name:           "non-boolean",
			body:           `{"showThinkingSummaries": "yes", "other": 1}`,
			wantAfterWrite: "true",
			wantRestored:   `"yes"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			settingsPath := settingsFixture(t, home, tc.body)

			if err := writeThinkingSummaries(settingsPath, home); err != nil {
				t.Fatalf("writeThinkingSummaries: %v", err)
			}
			if got := thinkingSummariesValue(t, settingsPath); got != tc.wantAfterWrite {
				t.Errorf("after write: %s = %s, want %s", ThinkingSummariesKey, got, tc.wantAfterWrite)
			}
			if !strings.Contains(mustReadFile(t, settingsPath), `"other": 1`) {
				t.Errorf("an unrelated settings key was lost:\n%s", mustReadFile(t, settingsPath))
			}

			res, err := RestoreThinkingSummaries(settingsPath, home)
			if err != nil {
				t.Fatalf("RestoreThinkingSummaries: %v", err)
			}
			if !res.Recorded {
				t.Fatal("Recorded = false; the write above must have left a record")
			}
			if res.Drifted {
				t.Fatalf("Drifted = true; nothing changed the value between write and restore: %+v", res)
			}
			if got := thinkingSummariesValue(t, settingsPath); got != tc.wantRestored {
				t.Errorf("after restore: %s = %s, want %s", ThinkingSummariesKey, got, tc.wantRestored)
			}
			if !strings.Contains(mustReadFile(t, settingsPath), `"other": 1`) {
				t.Errorf("an unrelated settings key was lost by restore:\n%s", mustReadFile(t, settingsPath))
			}
		})
	}
}

// TestWriteThinkingSummariesIsByteIdempotent is criterion 2's unit-level
// twin: a second call over its own output, with the record already captured,
// must not touch the settings file at all -- not just converge on the same
// content, but skip the write outright (for the whole file, since there is
// only one key).
func TestWriteThinkingSummariesIsByteIdempotent(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{"other": 1}`)

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := mustReadFile(t, settingsPath)
	firstInfo, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Force the mtime backwards so a real second write (even one producing
	// identical bytes) is detectable: writeFileAtomic renames unconditionally,
	// which would otherwise move mtime forward and hide a real second write.
	older := firstInfo.ModTime().Add(-time.Hour)
	if err := os.Chtimes(settingsPath, older, older); err != nil {
		t.Fatal(err)
	}

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second := mustReadFile(t, settingsPath)
	if first != second {
		t.Errorf("byte content changed on the second call:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	secondInfo, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondInfo.ModTime().Equal(older) {
		t.Errorf("mtime moved on the second call (%v -> %v); the settings write was not skipped",
			older, secondInfo.ModTime())
	}
}

// TestWriteThinkingSummariesNeverOverwritesTheRecordOnALaterInit is the
// Requirements-level pin: the prior state is recorded once and never
// overwritten by a later `init`, even when the value seen on a later call
// differs from both the original and from true (a developer setting it to
// something else in between two `init` runs, or a hand-rolled test standing
// in for one).
func TestWriteThinkingSummariesNeverOverwritesTheRecordOnALaterInit(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{}`) // absent

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("first write: %v", err)
	}
	rec, err := loadPriorSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	if pv := rec.Keys[ThinkingSummariesKey]; pv.Present {
		t.Fatalf("first capture: Present = true, want false (the key was absent): %+v", pv)
	}

	// Something other than OpenBox changes the value between two `init` runs.
	raw := mustReadFile(t, settingsPath)
	raw = strings.Replace(raw, "true", `"maybe"`, 1)
	if err := os.WriteFile(settingsPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := thinkingSummariesValue(t, settingsPath); got != "true" {
		t.Errorf("second write left %s = %s, want true (init forces it every run)", ThinkingSummariesKey, got)
	}
	rec2, err := loadPriorSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	if pv := rec2.Keys[ThinkingSummariesKey]; pv.Present {
		t.Errorf("the record was overwritten by the second `init` call: %+v, want still Present=false", pv)
	}
}

// TestWriteThinkingSummariesRefusesAnUnparsableFile mirrors
// activation.go:321-326 and writeHooks' own refusal: a settings file that
// exists but is not valid JSON is left untouched rather than rewritten, and
// no record is created for a value this call never actually read.
func TestWriteThinkingSummariesRefusesAnUnparsableFile(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{not json`)

	err := writeThinkingSummaries(settingsPath, home)
	if err == nil {
		t.Fatal("writeThinkingSummaries accepted an unparsable settings file")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error does not name the shape problem: %v", err)
	}
	if got := mustReadFile(t, settingsPath); got != `{not json` {
		t.Errorf("the refusal still rewrote the file: %q", got)
	}
	if _, err := os.Stat(PriorSettingsPath(home)); !os.IsNotExist(err) {
		t.Errorf("a record was written for a file that was never successfully read (err=%v)", err)
	}
}

// TestRestoreThinkingSummariesOnAMachineWithNoRecordIsANoOp is criterion 6:
// `openbox uninstall` runs unconditionally on every machine, including one
// that never ran `init` for this provider, and must report nothing and fail
// nothing.
func TestRestoreThinkingSummariesOnAMachineWithNoRecordIsANoOp(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{"other": 1}`)
	before := mustReadFile(t, settingsPath)

	res, err := RestoreThinkingSummaries(settingsPath, home)
	if err != nil {
		t.Fatalf("RestoreThinkingSummaries: %v", err)
	}
	if res.Recorded {
		t.Errorf("Recorded = true on a machine with no record: %+v", res)
	}
	if got := mustReadFile(t, settingsPath); got != before {
		t.Errorf("a no-record restore rewrote the settings file:\n%s", got)
	}
}

// TestRestoreThinkingSummariesDrift: a developer
// who changes the value after `init` keeps that change through `uninstall`,
// which reports the drift and touches nothing rather than failing.
func TestRestoreThinkingSummariesDrift(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{}`)
	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(string(out), "true", "false", 1)
	if err := os.WriteFile(settingsPath, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RestoreThinkingSummaries(settingsPath, home)
	if err != nil {
		t.Fatalf("RestoreThinkingSummaries must not fail on drift: %v", err)
	}
	if !res.Recorded {
		t.Fatal("Recorded = false; the write above must have left a record")
	}
	if !res.Drifted {
		t.Errorf("Drifted = false, want true: %+v", res)
	}
	if res.Current != "false" {
		t.Errorf("Current = %q, want \"false\"", res.Current)
	}
	if got := thinkingSummariesValue(t, settingsPath); got != "false" {
		t.Errorf("drift restore changed the value: %s, want false untouched", got)
	}
}

// TestWriteThinkingSummariesRecordsBeforeSettings is criterion 5: a settings
// write that fails after the record was already captured must leave the
// record and the settings file's ORIGINAL value both in place, and a later
// restore over that state is a clean no-op -- not an error, and not a
// corrupted file.
func TestWriteThinkingSummariesRecordsBeforeSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes")
	}
	home := t.TempDir()
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	const original = `{"other": 1}`
	if err := os.WriteFile(settingsPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settingsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(settingsDir, 0o755) })

	err := writeThinkingSummaries(settingsPath, home)
	if err == nil {
		t.Fatal("writeThinkingSummaries succeeded despite a read-only settings directory")
	}

	// The record must exist -- captured before the failed settings write --
	// and the settings file must still hold the ORIGINAL value: the failed
	// write must not have partially applied.
	rec, loadErr := loadPriorSettings(home)
	if loadErr != nil {
		t.Fatalf("load record after the failed write: %v", loadErr)
	}
	pv, ok := rec.Keys[ThinkingSummariesKey]
	if !ok {
		t.Fatal("no record was captured before the settings write failed")
	}
	if pv.Present {
		t.Errorf("recorded prior value: %+v, want Present=false (the key was absent)", pv)
	}
	if got := mustReadFile(t, settingsPath); got != original {
		t.Errorf("settings file changed despite the write failing: %q, want %q", got, original)
	}

	if err := os.Chmod(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := RestoreThinkingSummaries(settingsPath, home)
	if err != nil {
		t.Fatalf("subsequent restore must be a clean no-op, got error: %v", err)
	}
	if !res.Recorded {
		t.Fatal("Recorded = false after a record was captured")
	}
	if got := mustReadFile(t, settingsPath); got != original {
		t.Errorf("the clean no-op restore changed the settings file: %q, want %q", got, original)
	}
}

// TestWriteThinkingSummariesCreatesTheParentDirectory mirrors
// TestWriteHooksToAUserFileCreatesItsParent: a fresh machine has no
// ~/.claude directory yet, and the first `init` must not require one to
// pre-exist.
func TestWriteThinkingSummariesCreatesTheParentDirectory(t *testing.T) {
	home := t.TempDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json") // parent does not exist yet

	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("writeThinkingSummaries: %v", err)
	}
	if got := thinkingSummariesValue(t, settingsPath); got != "true" {
		t.Errorf("%s = %s, want true", ThinkingSummariesKey, got)
	}
}

// TestPriorSettingsPathIsUnderOpenboxHome pins the file's location to the
// gateway-prior-env.json precedent: its own sibling file under ~/.openbox,
// not .env (secrets only) and not dev.json (coordinates only).
func TestPriorSettingsPathIsUnderOpenboxHome(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, ".openbox", "claude-code-prior-settings.json")
	if got := PriorSettingsPath(home); got != want {
		t.Errorf("PriorSettingsPath(%q) = %q, want %q", home, got, want)
	}
}

// TestPriorSettingsRecordIsPermission0600 mirrors activation.json's posture
// (activation.go:288-298): the record is not a credential, but it is written
// with the same tightened permission discipline everything under ~/.openbox
// gets.
func TestPriorSettingsRecordIsPermission0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{}`)
	if err := writeThinkingSummaries(settingsPath, home); err != nil {
		t.Fatalf("writeThinkingSummaries: %v", err)
	}
	fi, err := os.Stat(PriorSettingsPath(home))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %v, want 0600", perm)
	}
	dirFi, err := os.Stat(filepath.Dir(PriorSettingsPath(home)))
	if err != nil {
		t.Fatalf("stat record dir: %v", err)
	}
	if perm := dirFi.Mode().Perm(); perm != 0o700 {
		t.Errorf("record dir mode = %v, want 0700", perm)
	}
}

// writePriorSettingsFixture hand-crafts a restore record directly, so a test
// can inject a shape writeThinkingSummaries itself would never produce --
// specifically, an envelope that is valid JSON (loadPriorSettings accepts it)
// but whose "raw" field holds something that is not.
func writePriorSettingsFixture(t *testing.T, home, body string) {
	t.Helper()
	path := PriorSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreThinkingSummariesRefusesACorruptedRawValue is the splice-safety
// gap: Raw is restored verbatim by design (RestoreThinkingSummaries:125 hands
// it straight to sjson.SetRawBytes so a developer's own non-boolean value
// round-trips exactly), but nothing checked that Raw itself still holds a
// well-formed JSON value before that splice. loadPriorSettings only validates
// the record's outer envelope -- "raw" is just a Go string field, so a
// truncated write, disk damage, or a hand-edit of the record can leave it
// holding bytes that unmarshal fine into that string but are not a complete
// JSON value on their own (here, "tru", a truncated boolean). SetRawBytes
// does not validate what it is given: splicing that straight in would corrupt
// the developer's real settings file silently. The fix refuses to write and
// reports the record as the cause instead -- and "yes" (a JSON string, the
// four-row matrix's non-boolean case) must still be accepted, because the
// rule is well-formed JSON, not boolean.
func TestRestoreThinkingSummariesRefusesACorruptedRawValue(t *testing.T) {
	home := t.TempDir()
	settingsPath := settingsFixture(t, home, `{"showThinkingSummaries": true, "other": 1}`)
	before := mustReadFile(t, settingsPath)
	writePriorSettingsFixture(t, home,
		`{"schema":"openbox.claude-code.prior-settings/v1",`+
			`"keys":{"showThinkingSummaries":{"present":true,"raw":"tru"}}}`)

	res, err := RestoreThinkingSummaries(settingsPath, home)
	if err == nil {
		t.Fatalf("RestoreThinkingSummaries accepted a corrupted raw value instead of refusing to write it: %+v", res)
	}
	if !strings.Contains(err.Error(), PriorSettingsPath(home)) {
		t.Errorf("error does not name the prior-settings record that holds the corrupted value: %v", err)
	}
	if got := mustReadFile(t, settingsPath); got != before {
		t.Errorf("the settings file changed despite the refusal:\nbefore: %s\nafter:  %s", before, got)
	}
}
