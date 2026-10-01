package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const testEndpoint = "http://127.0.0.1:8789"

func telemetrySandbox(t *testing.T, settings string) (settingsPath, home string) {
	t.Helper()
	dir := t.TempDir()
	settingsPath = filepath.Join(dir, "config", "muse", "settings.json")
	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settingsPath, []byte(settings), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return settingsPath, home
}

const foreignSettings = "{\n    \"schema_version\": 1,\n    \"theme\":   \"dark\",\n    \"hooks\": {\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"echo mine\", \"timeout\": 3}]}]}\n}\n"

func TestWriteTelemetryCreatesTheFileAndRecordsAbsence(t *testing.T) {
	path, home := telemetrySandbox(t, "")
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if gjson.Get(got, "schema_version").Int() != 1 {
		t.Errorf("a created file has no schema_version: %s", got)
	}
	tel := gjson.Get(got, "telemetry")
	if !tel.Get("enabled").Bool() || tel.Get("destination").String() != "external" || tel.Get("endpoint").String() != testEndpoint {
		t.Errorf("telemetry = %s", tel.Raw)
	}
	rec := readFile(t, PriorSettingsPath(home))
	if gjson.Get(rec, "keys.telemetry.present").Bool() {
		t.Errorf("the record says a value was present before: %s", rec)
	}
	if !HasOwnedTelemetry(path, home) {
		t.Error("HasOwnedTelemetry is false right after the write")
	}
}

// TestWriteTelemetryKeepsEveryForeignByte: the key is merged by path, so the
// developer's own keys, hooks, spacing and trailing newline survive, and the
// restore is byte-exact when there was no key before.
func TestWriteTelemetryKeepsEveryForeignByte(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	for _, key := range []string{"schema_version", "theme", "hooks"} {
		if want := gjson.Get(foreignSettings, key).Raw; gjson.Get(got, key).Raw != want {
			t.Errorf("%s changed: %s -> %s", key, want, gjson.Get(got, key).Raw)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("the trailing newline was lost")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the file's own 0640", info.Mode().Perm())
	}

	res, err := RestoreTelemetry(path, home)
	if err != nil || !res.Recorded || res.Drifted || res.Present {
		t.Fatalf("restore = %+v, %v; want recorded, not drifted, deleted", res, err)
	}
	if after := readFile(t, path); after != foreignSettings {
		t.Errorf("settings after restore differ from before init:\n%q\n%q", after, foreignSettings)
	}
	if _, err := os.Stat(PriorSettingsPath(home)); !os.IsNotExist(err) {
		t.Error("the restore record survives a completed restore")
	}
}

func TestWriteTelemetryRestoresAPriorValueVerbatim(t *testing.T) {
	prior := `{"enabled": false,  "destination":"meta"}`
	path, home := telemetrySandbox(t, "{\n  \"schema_version\": 1,\n  \"telemetry\": "+prior+"\n}\n")
	replaced, err := WriteTelemetry(path, home, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != prior {
		t.Errorf("replaced = %q, want the displaced value %q", replaced, prior)
	}
	if got := gjson.Get(readFile(t, path), "telemetry.destination").String(); got != "external" {
		t.Fatalf("destination = %q", got)
	}
	res, err := RestoreTelemetry(path, home)
	if err != nil || !res.Present || res.Value != prior {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	if got := gjson.Get(readFile(t, path), "telemetry").Raw; got != prior {
		t.Errorf("telemetry = %s, want the prior bytes %s", got, prior)
	}
}

// TestSecondWriteChangesNothing: the converged state is byte-identical in both
// files, and the record still holds the ORIGINAL prior value, not OpenBox's own.
func TestSecondWriteChangesNothing(t *testing.T) {
	path, home := telemetrySandbox(t, "{\n  \"schema_version\": 1,\n  \"telemetry\": {\"enabled\": false}\n}\n")
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	settings1, rec1 := readFile(t, path), readFile(t, PriorSettingsPath(home))
	for i := 0; i < 2; i++ {
		if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
			t.Fatal(err)
		}
	}
	if readFile(t, path) != settings1 {
		t.Error("a second init rewrote settings.json")
	}
	if readFile(t, PriorSettingsPath(home)) != rec1 {
		t.Error("a second init rewrote the record, or replaced the original prior value with OpenBox's own")
	}
	if got := gjson.Get(rec1, "keys.telemetry.raw").String(); got != `{"enabled": false}` {
		t.Errorf("record raw = %q", got)
	}
}

func TestWriteTelemetryFollowsAnEndpointChangeKeepingTheOriginalPrior(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTelemetry(path, home, "http://127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(readFile(t, path), "telemetry.endpoint").String(); got != "http://127.0.0.1:9999" {
		t.Errorf("endpoint = %q", got)
	}
	if res, err := RestoreTelemetry(path, home); err != nil || res.Present || res.Drifted {
		t.Fatalf("restore = %+v, %v; the original absence must survive an endpoint change", res, err)
	}
	if readFile(t, path) != foreignSettings {
		t.Error("not byte-exact after restore")
	}
}

// TestADeveloperEditAfterInitIsNeitherOverwrittenNorRestoredOver: drift in both
// directions. init refuses to overwrite it; uninstall reports it and leaves it.
func TestADeveloperEditAfterInitIsNeitherOverwrittenNorRestoredOver(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	mine := strings.Replace(readFile(t, path), testEndpoint, "https://otel.corp.example", 1)
	if err := os.WriteFile(path, []byte(mine), 0o640); err != nil {
		t.Fatal(err)
	}
	if HasOwnedTelemetry(path, home) {
		t.Error("a developer's value reads as OpenBox's")
	}
	if _, err := WriteTelemetry(path, home, testEndpoint); err == nil || !strings.Contains(err.Error(), "changed after OpenBox set it") {
		t.Fatalf("init over a developer's edit: %v, want a refusal", err)
	}
	if readFile(t, path) != mine {
		t.Error("init overwrote the developer's value")
	}
	res, err := RestoreTelemetry(path, home)
	if err != nil || !res.Recorded || !res.Drifted || !strings.Contains(res.Current, "corp.example") {
		t.Fatalf("restore = %+v, %v; want drift naming the current value", res, err)
	}
	if readFile(t, path) != mine {
		t.Error("uninstall touched a drifted value")
	}
	// Drift drops the record, so a later init captures the developer's value as
	// the new prior instead of refusing forever.
	if _, err := WriteTelemetry(path, home, testEndpoint); err == nil {
		// the value is the developer's and there is now no record: captured, and overwritten
		if res, err := RestoreTelemetry(path, home); err != nil || !res.Present || !strings.Contains(res.Value, "corp.example") {
			t.Errorf("after drift a fresh init must record the developer's value; restore = %+v, %v", res, err)
		}
	} else {
		t.Errorf("init after a drifted uninstall: %v", err)
	}
}

func TestRestoreTelemetryWhenTheKeyWasDeleted(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(foreignSettings), 0o640); err != nil {
		t.Fatal(err)
	}
	res, err := RestoreTelemetry(path, home)
	if err != nil || !res.Drifted || res.Current != "<absent>" {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	if readFile(t, path) != foreignSettings {
		t.Error("the file changed")
	}
}

func TestRestoreTelemetryWithNothingRecorded(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	res, err := RestoreTelemetry(path, home)
	if err != nil || res.Recorded {
		t.Fatalf("restore = %+v, %v; want nothing recorded", res, err)
	}
	if readFile(t, path) != foreignSettings {
		t.Error("the file changed")
	}
}

func TestRestoreTelemetryRefusesACorruptedRecordAndKeepsIt(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	recPath := PriorSettingsPath(home)
	rec := strings.Replace(readFile(t, recPath), `"present": false`, `"present": true, "raw": "{\"enabled\": "`, 1)
	if err := os.WriteFile(recPath, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if _, err := RestoreTelemetry(path, home); err == nil || !strings.Contains(err.Error(), recPath) {
		t.Fatalf("restore = %v, want an error naming the record", err)
	}
	if readFile(t, path) != before {
		t.Error("a corrupted record was spliced into the settings file")
	}
	if _, err := os.Stat(recPath); err != nil {
		t.Error("the record was dropped although the restore did not complete")
	}
}

func TestWriteTelemetryRefusesAFileMuseCannotRead(t *testing.T) {
	bad := `{"schema_version": 1, "telemetry": `
	path, home := telemetrySandbox(t, bad)
	if _, err := WriteTelemetry(path, home, testEndpoint); err == nil {
		t.Fatal("wrote into an unparseable settings file")
	}
	if readFile(t, path) != bad {
		t.Error("the file was modified")
	}
	if _, err := os.Stat(PriorSettingsPath(home)); !os.IsNotExist(err) {
		t.Error("a record was written for a write that was refused")
	}
}

// TestSettingsAreNotTouchedWhenTheRecordCannotBeWritten: record first, then
// settings -- so a failure to record leaves Muse's file as it was.
func TestSettingsAreNotTouchedWhenTheRecordCannotBeWritten(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	if err := os.WriteFile(filepath.Join(home, ".openbox"), []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTelemetry(path, home, testEndpoint); err == nil {
		t.Fatal("wrote although the record could not be saved")
	}
	if readFile(t, path) != foreignSettings {
		t.Error("settings.json changed though its restore record could not be written")
	}
}

func TestWriteTelemetryWritesThroughASymlink(t *testing.T) {
	path, home := telemetrySandbox(t, "")
	target := filepath.Join(t.TempDir(), "dotfiles-settings.json")
	if err := os.WriteFile(target, []byte(foreignSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if gjson.Get(readFile(t, target), "telemetry.destination").String() != "external" {
		t.Error("the link's target was not written")
	}
}

// TestTelemetryObjectEscapesTheEndpoint: the value is built by a JSON encoder,
// never by string concatenation into a document.
func TestTelemetryObjectEscapesTheEndpoint(t *testing.T) {
	if !gjson.Valid(telemetryObject(`http://x/"},"evil":{"`)) {
		t.Error("an endpoint with quotes produced invalid JSON")
	}
}

// TestAFailedWriteLeavesTheRecordAsItWas: the record is saved before the
// settings, so a settings write that then fails must take the record back with
// it. Otherwise it keeps OpenBox's intended value as "owned" and the next init
// refuses the file as changed after OpenBox set it.
func TestAFailedWriteLeavesTheRecordAsItWas(t *testing.T) {
	path, home := telemetrySandbox(t, foreignSettings)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced here")
	}

	if _, err := WriteTelemetry(path, home, testEndpoint); err == nil {
		t.Fatal("the write succeeded into a read-only directory")
	}
	if _, err := os.Stat(PriorSettingsPath(home)); !os.IsNotExist(err) {
		t.Error("a record outlived a failed first write")
	}
	if readFile(t, path) != foreignSettings {
		t.Error("settings changed")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatalf("the retry after a failed write: %v", err)
	}

	// And a failed endpoint change keeps the converged record, not the new owned value.
	rec := readFile(t, PriorSettingsPath(home))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTelemetry(path, home, "http://127.0.0.1:9999"); err == nil {
		t.Fatal("the endpoint change succeeded into a read-only directory")
	}
	if readFile(t, PriorSettingsPath(home)) != rec {
		t.Error("a failed endpoint change left the record naming a value that never landed")
	}
	_ = os.Chmod(dir, 0o700)
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Errorf("the next init refuses after a failed change: %v", err)
	}
}

// TestCheckTelemetryRefusesWhatWriteWouldAndTouchesNothing is the dry run the
// installer runs before it touches the shared unit.
func TestCheckTelemetryRefusesWhatWriteWouldAndTouchesNothing(t *testing.T) {
	bad := `{"schema_version": 1,`
	path, home := telemetrySandbox(t, bad)
	if err := CheckTelemetry(path, home, testEndpoint); err == nil {
		t.Error("an unreadable settings file passed the check")
	}

	path, home = telemetrySandbox(t, foreignSettings)
	if err := CheckTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != foreignSettings {
		t.Error("the check wrote settings")
	}
	if _, err := os.Stat(PriorSettingsPath(home)); !os.IsNotExist(err) {
		t.Error("the check wrote a record")
	}
	if _, err := WriteTelemetry(path, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	mine := strings.Replace(readFile(t, path), testEndpoint, "https://otel.corp.example", 1)
	if err := os.WriteFile(path, []byte(mine), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CheckTelemetry(path, home, testEndpoint); err == nil || !strings.Contains(err.Error(), "changed after OpenBox set it") {
		t.Errorf("a drifted value passed the check: %v", err)
	}
}

// With no record, a key that already holds exactly the lane's value is
// OpenBox's (a lost record, a synced settings file), never the developer's:
// uninstall deletes it rather than restoring a pointer at a receiver it is
// about to remove.
func TestWriteTelemetryWithNoRecordDoesNotAdoptItsOwnValueAsPrior(t *testing.T) {
	ours := "{\n  \"schema_version\": 1,\n  \"telemetry\": " + telemetryObject(testEndpoint) + "\n}\n"
	path, home := telemetrySandbox(t, ours)
	replaced, err := WriteTelemetry(path, home, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != "" {
		t.Errorf("init reports replacing the developer's value %q; it was OpenBox's own", replaced)
	}
	res, err := RestoreTelemetry(path, home)
	if err != nil || !res.Recorded || res.Present {
		t.Fatalf("restore = %+v, %v; want the key deleted as absent before init", res, err)
	}
	if gjson.Get(readFile(t, path), "telemetry").Exists() {
		t.Errorf("uninstall left the lane's pointer behind: %s", readFile(t, path))
	}
}

// TestARecordForAnotherSettingsFileIsNotAppliedHere: the record names the file
// it was made for. Applied to a different one (HOME or the user changed), its
// owned value would be compared with a file OpenBox never wrote and the run
// would blame the developer for a change nobody made. Write, check and restore
// all stop, name both files, and leave the record and both files alone.
func TestARecordForAnotherSettingsFileIsNotAppliedHere(t *testing.T) {
	first, home := telemetrySandbox(t, foreignSettings)
	if _, err := WriteTelemetry(first, home, testEndpoint); err != nil {
		t.Fatal(err)
	}
	recBefore := readFile(t, PriorSettingsPath(home))
	other := filepath.Join(t.TempDir(), "other", "settings.json")
	writeFile(t, other, foreignSettings)

	for name, call := range map[string]func() error{
		"write":   func() error { _, err := WriteTelemetry(other, home, testEndpoint); return err },
		"check":   func() error { return CheckTelemetry(other, home, testEndpoint) },
		"restore": func() error { _, err := RestoreTelemetry(other, home); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s: a record for another settings file was applied", name)
		}
		if strings.Contains(err.Error(), "changed after OpenBox set it") {
			t.Errorf("%s: blamed the developer for a change nobody made: %v", name, err)
		}
		firstResolved, _ := filepath.EvalSymlinks(first)
		otherResolved, _ := filepath.EvalSymlinks(other)
		if !strings.Contains(err.Error(), firstResolved) || !strings.Contains(err.Error(), otherResolved) {
			t.Errorf("%s: the error does not name both files: %v", name, err)
		}
	}
	if got := readFile(t, other); got != foreignSettings {
		t.Errorf("the other file was touched:\n%s", got)
	}
	if got := readFile(t, PriorSettingsPath(home)); got != recBefore {
		t.Errorf("the record was changed:\n%s", got)
	}
	if HasOwnedTelemetry(other, home) {
		t.Error("HasOwnedTelemetry claimed a file the record is not for")
	}
	// The file it was made for still restores.
	if r, err := RestoreTelemetry(first, home); err != nil || !r.Recorded {
		t.Fatalf("restore of the original file = %+v, %v", r, err)
	}
}
