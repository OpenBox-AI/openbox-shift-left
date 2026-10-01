package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Muse's telemetry lane is one key of its own settings.json. Setting
// `telemetry` to {enabled, destination:"external", endpoint} makes Muse export
// OTLP/HTTP to that endpoint instead of Meta's own destinations; nothing in the
// environment does it (OTEL_EXPORTER_OTLP_ENDPOINT alone is ignored, measured
// on Muse 1.4.1). So the lane is a write to a file the developer owns, and
// this file is the whole of it: record what was there first, merge one key by
// path so every other byte survives, read it back, and on uninstall put back
// exactly what was recorded -- or leave a value the developer has since
// changed.

// TelemetryKey is the settings.json key the lane owns.
const TelemetryKey = "telemetry"

// priorSettingsSchema versions the restore record.
const priorSettingsSchema = "openbox.muse.prior-settings/v1"

// priorValue is the key's value from before OpenBox first wrote it, plus what
// OpenBox wrote. Present distinguishes "the key was absent, so restore means
// delete" from "the key held some JSON value, restore verbatim"; Raw is
// gjson's raw form, so a prior `false` or `null` or a whole object goes back
// as the bytes the developer typed. Owned is the value OpenBox set: a current
// value that is not it is the developer's, and is never overwritten or
// restored over.
type priorValue struct {
	Present bool   `json:"present"`
	Raw     string `json:"raw,omitempty"`
	Owned   string `json:"owned"`
}

// priorSettings is the whole restore record: one file of its own under
// ~/.openbox (not dev.json, which holds coordinates only, and not .env, which
// holds secrets only).
type priorSettings struct {
	Schema       string                `json:"schema"`
	SettingsPath string                `json:"settings_path,omitempty"`
	Keys         map[string]priorValue `json:"keys"`
}

// PriorSettingsPath is where the restore record lives, exported so uninstall
// can name and purge it without importing the rest of this adapter.
func PriorSettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "muse-prior-settings.json")
}

func loadPriorSettings(homeDir string) (priorSettings, error) {
	path := PriorSettingsPath(homeDir)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return priorSettings{Schema: priorSettingsSchema, Keys: map[string]priorValue{}}, nil
	}
	if err != nil {
		return priorSettings{}, fmt.Errorf("muse: reading %s: %w", path, err)
	}
	var rec priorSettings
	if err := json.Unmarshal(raw, &rec); err != nil {
		// Refused rather than replaced: continuing without knowing what an earlier
		// run captured would let a later init take its own forced value for the
		// developer's original.
		return priorSettings{}, fmt.Errorf("muse: %s is not valid JSON, refusing to continue without knowing "+
			"what was in Muse's settings before OpenBox: %w", path, err)
	}
	if rec.Keys == nil {
		rec.Keys = map[string]priorValue{}
	}
	rec.Schema = priorSettingsSchema
	return rec, nil
}

func savePriorSettings(homeDir string, rec priorSettings) error {
	path := PriorSettingsPath(homeDir)
	if len(rec.Keys) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("muse: removing %s: %w", path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("muse: creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("muse: encoding %s: %w", path, err)
	}
	if err := hookflow.AtomicWriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("muse: writing %s: %w", path, err)
	}
	return nil
}

// telemetryObject is the value the lane owns. The endpoint is the receiver's
// base URL: Muse appends /muse-code/telemetry/{logs,traces} itself.
func telemetryObject(endpoint string) string {
	raw, _ := json.Marshal(struct {
		Enabled     bool   `json:"enabled"`
		Destination string `json:"destination"`
		Endpoint    string `json:"endpoint"`
	}{true, "external", endpoint})
	return string(raw)
}

// resolveSettingsPath is the real path behind a settings file, or the path as
// given when it does not resolve (the file may not exist yet).
func resolveSettingsPath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// samePath reports whether two settings paths name one file: the same cleaned
// string, or two paths that stat to the same file.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

// recordElsewhere is the refusal for a restore record written for another
// settings file (HOME or the user changed, a sudo run). Applying its entry here
// would compare this file's value with one OpenBox set in a different file and
// report a change nobody made; discarding it would strand a restore that may
// still be owed there. So the run stops and names both files.
func recordElsewhere(rec priorSettings, current string) error {
	return fmt.Errorf("muse: the OpenBox restore record is for %s, but this run is working on %s; "+
		"leaving both alone. Run again as the user (and HOME) that ran `openbox init` for %s, "+
		"or delete the record if that file no longer exists", rec.SettingsPath, current, rec.SettingsPath)
}

// isElsewhere reports whether the record holds the telemetry entry for a
// settings file other than current.
func (rec priorSettings) isElsewhere(current string) bool {
	if _, ok := rec.Keys[TelemetryKey]; !ok || rec.SettingsPath == "" {
		return false
	}
	return !samePath(rec.SettingsPath, current)
}

// resolvedSettings returns the real path behind a settings file (a dotfiles
// manager often makes it a symlink, and renaming over the link would replace
// it with a regular file), its bytes, whether it exists, and its permissions.
// A file Muse could not read is refused, because Muse drops every hook in it.
func resolvedSettings(path string) (resolved string, before []byte, existed bool, perm os.FileMode, err error) {
	resolved, perm = resolveSettingsPath(path), 0o600
	before, err = os.ReadFile(resolved)
	switch {
	case err == nil:
		existed = true
		if info, statErr := os.Stat(resolved); statErr == nil {
			perm = info.Mode().Perm()
		}
		if _, vErr := ValidateSettings(before); vErr != nil {
			return "", nil, false, 0, fmt.Errorf("muse: refusing to modify %s: %w; Muse drops every setting in a file it "+
				"cannot read, so fix or remove it and run init again. Nothing was written", resolved, vErr)
		}
	case os.IsNotExist(err):
		before, err = nil, nil
	default:
		return "", nil, false, 0, fmt.Errorf("muse: read %s: %w", resolved, err)
	}
	return resolved, before, existed, perm, nil
}

// telemetryPlan is a write worked out but not yet made: everything a refusal
// depends on has been read and decided, nothing has been touched.
type telemetryPlan struct {
	path      string
	before    []byte
	out       []byte
	existed   bool
	perm      os.FileMode
	desired   string
	replaced  string
	current   gjson.Result
	rec       priorSettings // the record as it should be once the write lands
	prevKeys  map[string]priorValue
	prevPath  string
	unchanged bool // settings already say it
}

func (p *telemetryPlan) previousRecord(homeDir string) priorSettings {
	keys := make(map[string]priorValue, len(p.prevKeys))
	for k, v := range p.prevKeys {
		keys[k] = v
	}
	return priorSettings{Schema: priorSettingsSchema, SettingsPath: p.prevPath, Keys: keys}
}

func planTelemetry(settingsPath, homeDir, endpoint string) (*telemetryPlan, error) {
	path, before, existed, perm, err := resolvedSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	rec, err := loadPriorSettings(homeDir)
	if err != nil {
		return nil, err
	}
	if rec.isElsewhere(path) {
		return nil, recordElsewhere(rec, path)
	}
	p := &telemetryPlan{path: path, before: before, existed: existed, perm: perm, prevKeys: map[string]priorValue{}, prevPath: rec.SettingsPath}
	for k, v := range rec.Keys {
		p.prevKeys[k] = v
	}
	p.rec = rec
	doc := before
	if !existed {
		if doc, err = sjson.SetBytes([]byte("{}"), "schema_version", settingsSchemaVersion); err != nil {
			return nil, fmt.Errorf("muse: schema_version: %w", err)
		}
	}
	p.desired = telemetryObject(endpoint)
	p.current = gjson.GetBytes(doc, TelemetryKey)
	entry, captured := rec.Keys[TelemetryKey]

	switch {
	case captured && p.current.Exists() && !canonicalJSONEqual([]byte(p.current.Raw), []byte(entry.Owned)):
		return nil, fmt.Errorf("muse: %s holds a %q value that changed after OpenBox set it (%s); leaving it alone. "+
			"Remove the key, or restore OpenBox's value, and run init again", path, TelemetryKey, p.current.Raw)
	case captured && p.current.Exists():
		// Ours. Only the endpoint can differ, and the original prior value stays.
		entry.Owned = p.desired
	case p.current.Exists() && canonicalJSONEqual([]byte(p.current.Raw), []byte(p.desired)):
		// No record, yet the key already holds exactly the value this lane
		// writes: a lost record, or a settings file synced from a machine where
		// OpenBox had set it. That value is OpenBox's, not the developer's, so it
		// is recorded as absent; recording it as the prior value would have
		// uninstall "restore" it and leave Muse exporting to a closed port.
		entry = priorValue{Present: false, Owned: p.desired}
	default:
		entry = priorValue{Present: p.current.Exists(), Raw: p.current.Raw, Owned: p.desired}
		if p.current.Exists() {
			p.replaced = p.current.Raw
		}
	}
	keys := make(map[string]priorValue, len(rec.Keys)+1)
	for k, v := range rec.Keys {
		keys[k] = v
	}
	keys[TelemetryKey] = entry
	p.rec = priorSettings{Schema: priorSettingsSchema, SettingsPath: path, Keys: keys}
	if p.current.Exists() && canonicalJSONEqual([]byte(p.current.Raw), []byte(p.desired)) {
		p.unchanged = true
		return p, nil
	}
	out, err := sjson.SetRawBytes(doc, TelemetryKey, []byte(p.desired))
	if err != nil {
		return nil, fmt.Errorf("muse: setting %s in %s: %w", TelemetryKey, path, err)
	}
	p.out = finishSettingsWrite(out, before, existed)
	if _, err := ValidateSettings(p.out); err != nil {
		return nil, fmt.Errorf("muse: refusing to write %s: %w", path, err)
	}
	return p, nil
}

// CheckTelemetry is WriteTelemetry's refusal logic with nothing written: an
// unreadable settings file, a record that cannot be used, or a `telemetry` value
// changed since OpenBox set it all fail here exactly as they would there. The
// installer runs it before it touches the shared telemetry unit, so a refusal
// never disturbs a lane that already works.
func CheckTelemetry(settingsPath, homeDir, endpoint string) error {
	_, err := planTelemetry(settingsPath, homeDir, endpoint)
	return err
}

// WriteTelemetry points Muse's own telemetry export at endpoint. homeDir is the
// developer's home, where the restore record lives.
//
// The record is written BEFORE the settings ("install ordering is a safety
// property"): a record with no settings behind it is treated by uninstall as
// drift, while settings with no record would have no way home. If anything
// after the record fails, the record goes back to what it was, so a failed run
// leaves neither file changed and the next init does not mistake OpenBox's own
// intended value for the developer's. A re-run on a converged file writes
// nothing, to either file. A `telemetry` value the developer changed after
// OpenBox set it is theirs: it is refused, untouched, rather than overwritten.
//
// replaced is the developer's previous value when this write displaced one that
// was not already OpenBox's own, raw, for the install report; "" otherwise.
func WriteTelemetry(settingsPath, homeDir, endpoint string) (replaced string, err error) {
	p, err := planTelemetry(settingsPath, homeDir, endpoint)
	if err != nil {
		return "", err
	}
	if err := savePriorSettings(homeDir, p.rec); err != nil {
		return "", err
	}
	if p.unchanged {
		return "", nil
	}
	fail := func(format string, args ...any) (string, error) {
		_ = savePriorSettings(homeDir, p.previousRecord(homeDir))
		return "", fmt.Errorf(format, args...)
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return fail("muse: settings dir: %w", err)
	}
	if err := hookflow.AtomicWriteFile(p.path, p.out, p.perm); err != nil {
		return fail("muse: commit %s: %w", p.path, err)
	}
	// Re-read what landed: a short or altered write is a machine that reads as
	// exporting to OpenBox and does not.
	written, rerr := os.ReadFile(p.path)
	if rerr == nil {
		if _, rerr = ValidateSettings(written); rerr == nil {
			if got := gjson.GetBytes(written, TelemetryKey); !got.Exists() || !canonicalJSONEqual([]byte(got.Raw), []byte(p.desired)) {
				rerr = errors.New("the telemetry value did not land as written")
			}
		}
	}
	if rerr != nil {
		restoreSettingsFile(p.path, p.before, p.existed, p.perm)
		return fail("muse: %s did not read back as written (%w); the previous file was restored", p.path, rerr)
	}
	return p.replaced, nil
}

// finishSettingsWrite keeps a file's final whitespace, and gives a file this
// write created readable indentation.
func finishSettingsWrite(out, before []byte, existed bool) []byte {
	if existed {
		return keepTrailingSpace(out, before)
	}
	var indented bytes.Buffer
	if json.Indent(&indented, out, "", "  ") == nil {
		return append(indented.Bytes(), '\n')
	}
	return out
}

func restoreSettingsFile(path string, before []byte, existed bool, perm os.FileMode) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	_ = hookflow.AtomicWriteFile(path, before, perm)
}

// HasOwnedTelemetry reports whether settingsPath still carries the value
// OpenBox set: the record exists and the current value is it. False for a
// value the developer wrote or changed, which is not OpenBox's to sweep.
func HasOwnedTelemetry(settingsPath, homeDir string) bool {
	rec, err := loadPriorSettings(homeDir)
	if err != nil {
		return false
	}
	entry, ok := rec.Keys[TelemetryKey]
	if !ok || rec.isElsewhere(resolveSettingsPath(settingsPath)) {
		return false
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	current := gjson.GetBytes(raw, TelemetryKey)
	return current.Exists() && canonicalJSONEqual([]byte(current.Raw), []byte(entry.Owned))
}

// TelemetryRestored reports what RestoreTelemetry did, so uninstall can say
// exactly one of: nothing recorded, restored to a recorded value, deleted (it
// was absent before init), or left alone because the developer changed it.
type TelemetryRestored struct {
	// Recorded is false when no record exists: init never wrote the key here, or
	// an earlier uninstall already restored it. The ordinary case.
	Recorded bool
	// Drifted is true when the current value is not the one OpenBox wrote; Current
	// is its raw form ("<absent>" when the key is gone). Nothing was touched.
	Drifted bool
	Current string
	// Present is the recorded prior value's shape: true means the key went back to
	// Value, false means it was absent before init and is now deleted.
	Present bool
	Value   string
}

// RestoreTelemetry puts `telemetry` back to what WriteTelemetry found, or
// deletes it when there was none, and drops its record. A value that is no
// longer the one OpenBox wrote is reported and left alone: drift is not an
// error, because uninstall must finish whatever one key looks like now. A
// record that cannot be used is an error, and is kept.
func RestoreTelemetry(settingsPath, homeDir string) (TelemetryRestored, error) {
	rec, err := loadPriorSettings(homeDir)
	if err != nil {
		return TelemetryRestored{}, err
	}
	entry, ok := rec.Keys[TelemetryKey]
	if !ok {
		return TelemetryRestored{}, nil
	}
	if rec.isElsewhere(resolveSettingsPath(settingsPath)) {
		return TelemetryRestored{}, recordElsewhere(rec, resolveSettingsPath(settingsPath))
	}
	forget := func() error {
		delete(rec.Keys, TelemetryKey)
		return savePriorSettings(homeDir, rec)
	}

	path, before, existed, perm, err := resolvedSettings(settingsPath)
	if err != nil {
		return TelemetryRestored{}, err
	}
	current := gjson.GetBytes(before, TelemetryKey)
	if !existed || !current.Exists() || !canonicalJSONEqual([]byte(current.Raw), []byte(entry.Owned)) {
		out := TelemetryRestored{Recorded: true, Drifted: true, Current: "<absent>"}
		if current.Exists() {
			out.Current = current.Raw
		}
		return out, forget()
	}

	var next []byte
	if entry.Present {
		// The record's Raw is a string field; a truncated or hand-edited one can
		// hold bytes that are not a complete JSON value, and splicing them into the
		// developer's settings would corrupt it while reporting success.
		if !gjson.Valid(entry.Raw) {
			recPath := PriorSettingsPath(homeDir)
			return TelemetryRestored{}, fmt.Errorf("muse: %s holds a corrupted value for %s (%q is not valid JSON); "+
				"refusing to write it into %s. Fix or delete the %q entry in %s, then run `openbox uninstall` again",
				recPath, TelemetryKey, entry.Raw, path, TelemetryKey, recPath)
		}
		next, err = sjson.SetRawBytes(before, TelemetryKey, []byte(entry.Raw))
	} else {
		next, err = sjson.DeleteBytes(before, TelemetryKey)
	}
	if err != nil {
		return TelemetryRestored{}, fmt.Errorf("muse: restoring %s in %s: %w", TelemetryKey, path, err)
	}
	next = keepTrailingSpace(next, before)
	if _, err := ValidateSettings(next); err != nil {
		return TelemetryRestored{}, fmt.Errorf("muse: refusing to write %s: %w", path, err)
	}
	if err := hookflow.AtomicWriteFile(path, next, perm); err != nil {
		return TelemetryRestored{}, fmt.Errorf("muse: writing %s: %w", path, err)
	}
	if err := forget(); err != nil {
		return TelemetryRestored{}, err
	}
	return TelemetryRestored{Recorded: true, Present: entry.Present, Value: entry.Raw}, nil
}
