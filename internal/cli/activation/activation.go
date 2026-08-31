// Package activation owns the developer's tool-settings environment block:
// which keys OpenBox writes there, what was there before, and how to put it
// back.
//
//	the lane's own set is the developer's or their org's and is never touched -
//	not on install, not on removal, not when the whole env block is rewritten.
//	Removing one lane must leave the others working.
package activation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Lane names an OpenBox component that owns a set of env keys.
type Lane string

const (
	// LaneGateway is the loopback base-URL relay.
	LaneGateway Lane = "gateway"
	// LaneTelemetry is the local OTLP receiver (that decision `:otel:`).
	LaneTelemetry Lane = "telemetry"
	// LaneTransport is the in-path CONNECT/TLS relay (that decision `:proxy:`).
	LaneTransport Lane = "transport"
)

const recordSchema = "openbox.dev-runtime.activation/v1"

// Original is what a key looked like before OpenBox first wrote it. Present
// distinguishes "absent, so removal means delete" from "empty string, which we
// must put back verbatim".
type Original struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

// Entry is one lane's activation.
type Entry struct {
	Managed      map[string]string   `json:"managed"`
	Original     map[string]Original `json:"original"`
	SettingsPath string              `json:"settings_path"`
	ActivatedAt  string              `json:"activated_at"`
	BeforeSHA256 string              `json:"before_sha256"`
	AfterSHA256  string              `json:"after_sha256"`
}

// Record is the whole file: one entry per activated lane.
type Record struct {
	Schema string          `json:"schema"`
	Lanes  map[Lane]*Entry `json:"lanes"`
}

// Applied reports what an activation changed.
type Applied struct {
	// Replaced holds "KEY: old -> new" for every key whose value we displaced.
	// Never silent: something had that value pointed somewhere.
	Replaced []string
}

// Reverted reports what a removal did.
type Reverted struct {
	Removed   []string
	Restored  map[string]string
	Conflicts []string
}

// RecordPath is where the activation record lives.
func RecordPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "activation.json")
}

// Activate writes desired into the settings env block on behalf of lane.
func Activate(homeDir, settingsPath string, lane Lane, desired map[string]string) (Applied, error) {
	var applied Applied

	beforeRaw, err := readSettings(settingsPath)
	if err != nil {
		return applied, err
	}
	if err := checkEnvShape(beforeRaw, settingsPath); err != nil {
		return applied, err
	}

	record, err := loadRecord(homeDir)
	if err != nil {
		return applied, err
	}
	entry := record.Lanes[lane]
	if entry == nil {
		entry = &Entry{Original: map[string]Original{}}
		record.Lanes[lane] = entry
	}
	if entry.Original == nil {
		entry.Original = map[string]Original{}
	}

	afterRaw := beforeRaw
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		want := desired[key]
		existing := gjson.GetBytes(afterRaw, envPath(key))
		if existing.Exists() {
			if s := existing.String(); s != want {
				applied.Replaced = append(applied.Replaced, fmt.Sprintf("%s: %s -> %s", key, s, want))
			}
		}
		if _, captured := entry.Original[key]; !captured {
			entry.Original[key] = Original{Present: existing.Exists(), Value: existing.String()}
		}
		afterRaw, err = sjson.SetBytes(afterRaw, envPath(key), want)
		if err != nil {
			return applied, fmt.Errorf("activation: setting %s in %s: %w", key, settingsPath, err)
		}
	}
	afterRaw = finishSettings(indentIfNew(afterRaw, beforeRaw), beforeRaw)

	entry.Managed = copyOf(desired)
	entry.SettingsPath = settingsPath
	entry.ActivatedAt = time.Now().UTC().Format(time.RFC3339)
	entry.BeforeSHA256 = sha256Of(beforeRaw)
	entry.AfterSHA256 = sha256Of(afterRaw)

	if err := saveRecord(homeDir, record); err != nil {
		return applied, err
	}
	if err := writeSettings(settingsPath, afterRaw); err != nil {
		return applied, err
	}
	return applied, nil
}

// Deactivate puts lane's keys back and forgets the lane. It exists because
// without it a machine whose managed value drifted could never complete a
// removal at all; and removal must not require the thing being removed to
// still be in the state we left it in.
func Deactivate(homeDir, settingsPath string, lane Lane, force bool) (Reverted, error) {
	out := Reverted{Restored: map[string]string{}}

	record, err := loadRecord(homeDir)
	if err != nil {
		return out, err
	}
	entry := record.Lanes[lane]
	if entry == nil {
		return out, nil
	}

	beforeRaw, err := readSettings(settingsPath)
	if err != nil {
		return out, err
	}
	if err := checkEnvShape(beforeRaw, settingsPath); err != nil {
		return out, err
	}

	type action struct {
		key      string
		original Original
	}
	var todo []action
	for _, key := range slices.Sorted(maps.Keys(entry.Managed)) {
		original := entry.Original[key]
		current := gjson.GetBytes(beforeRaw, envPath(key))
		switch {
		case !current.Exists():
			continue
		case current.String() == entry.Managed[key]:
		case original.Present && current.String() == original.Value:
			continue
		default:
			out.Conflicts = append(out.Conflicts, key)
			if !force {
				continue
			}
		}
		todo = append(todo, action{key: key, original: original})
	}
	if len(out.Conflicts) > 0 && !force {
		return out, fmt.Errorf("activation: %s changed since OpenBox set %s; refusing to overwrite. "+
			"Review the value, or re-run with --force-restore to restore what was there before",
			strings.Join(out.Conflicts, ", "), plural(len(out.Conflicts), "it", "them"))
	}

	raw := beforeRaw
	for _, a := range todo {
		if a.original.Present {
			if raw, err = sjson.SetBytes(raw, envPath(a.key), a.original.Value); err != nil {
				return out, fmt.Errorf("activation: restoring %s in %s: %w", a.key, settingsPath, err)
			}
			out.Restored[a.key] = a.original.Value
			continue
		}
		if raw, err = sjson.DeleteBytes(raw, envPath(a.key)); err != nil {
			return out, fmt.Errorf("activation: removing %s from %s: %w", a.key, settingsPath, err)
		}
		out.Removed = append(out.Removed, a.key)
	}

	// A file that had no env block before must not gain an empty one.
	if envKeyCount(raw) == 0 {
		if raw, err = sjson.DeleteBytes(raw, "env"); err != nil {
			return out, fmt.Errorf("activation: removing the empty env block from %s: %w", settingsPath, err)
		}
	}
	raw = finishSettings(raw, beforeRaw)
	if err := writeSettings(settingsPath, raw); err != nil {
		return out, err
	}

	delete(record.Lanes, lane)
	if err := saveRecord(homeDir, record); err != nil {
		return out, err
	}
	return out, nil
}

// ActiveLanes reports which lanes this tool has activated and not removed, in
// precedence order.
func ActiveLanes(homeDir string) []Lane {
	record, err := loadRecord(homeDir)
	if err != nil {
		return nil
	}
	var lanes []Lane
	for _, lane := range lanePrecedence {
		if record.Lanes[lane] != nil {
			lanes = append(lanes, lane)
		}
	}
	return lanes
}

// CurrentEnv returns the settings file's env block as strings, so a caller can
// compute a desired value that merges with what is there; NO_PROXY being the
// case that matters, where overwriting an org's list breaks their egress while
// the lane is active even though removal would put it back.
func CurrentEnv(settingsPath string) map[string]string {
	raw, err := readSettings(settingsPath)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	gjson.GetBytes(raw, "env").ForEach(func(k, v gjson.Result) bool {
		out[k.String()] = v.String()
		return true
	})
	return out
}

func loadRecord(homeDir string) (Record, error) {
	path := RecordPath(homeDir)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Record{Schema: recordSchema, Lanes: map[Lane]*Entry{}}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("activation: reading %s: %w", path, err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("activation: %s is not valid JSON, refusing to continue without knowing "+
			"what was on this machine before OpenBox: %w", path, err)
	}
	if rec.Lanes == nil {
		rec.Lanes = map[Lane]*Entry{}
	}
	rec.Schema = recordSchema
	return rec, nil
}

func saveRecord(homeDir string, rec Record) error {
	path := RecordPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("activation: creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("activation: encoding the record: %w", err)
	}
	if err := atomicfile.Write(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("activation: writing %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

// readSettings returns the file's bytes, and nothing else, because every byte
// in it that this tool did not write belongs to the developer. Decoding into a
// typed struct is how a writer silently deletes configuration it was never
// taught about; decoding into map[string]any is how it silently reorders and
// reindents the whole document, which Go does because it marshals a map in
// sorted key order.
//
// The validity check has to be spelled out now. It used to be a side effect of
// json.Unmarshal failing; sjson will edit a malformed document without
// complaint, and this file is one every reader here refuses to rewrite when it
// cannot parse it -- so a malformed file written back would block its own
// repair. It is gjson's validator on purpose: the thing that decides a document
// is safe to edit has to be the thing that edits it. encoding/json is used only
// to explain a rejection, where its byte offset is worth having.
func readSettings(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("activation: reading %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	if !gjson.ValidBytes(raw) {
		if detail := json.Unmarshal(raw, new(any)); detail != nil {
			return raw, fmt.Errorf("activation: %s is not valid JSON, refusing to rewrite it: %w", path, detail)
		}
		return raw, fmt.Errorf("activation: %s is not valid JSON, refusing to rewrite it", path)
	}
	return raw, nil
}

// envPath is the gjson/sjson path for one key inside the env block. The escape
// matters: gjson reads `.` as a separator and `*?#|@` as query syntax, so a
// developer's key holding any of them would address something that is not
// there -- reading empty, and writing a nested object beside the real key.
func envPath(key string) string {
	var b strings.Builder
	b.WriteString("env.")
	for _, r := range key {
		switch r {
		case '.', '*', '?', '#', '|', '@', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// checkEnvShape refuses a settings file whose `env` is not an object. Absent
// and null are both fine -- the writer creates it -- but replacing a string or
// an array with an object would be destroying a shape somebody chose.
func checkEnvShape(raw []byte, path string) error {
	env := gjson.GetBytes(raw, "env")
	if !env.Exists() || env.Type == gjson.Null {
		return nil
	}
	if !env.IsObject() {
		return fmt.Errorf("activation: `env` in %s is not a JSON object; refusing to rewrite it", path)
	}
	return nil
}

// envKeyCount is how the writer knows whether removing its own keys emptied the
// block, which it then deletes rather than leaving `"env": {}` in a file that
// never had one.
func envKeyCount(raw []byte) int {
	n := 0
	gjson.GetBytes(raw, "env").ForEach(func(gjson.Result, gjson.Result) bool {
		n++
		return true
	})
	return n
}

// finishSettings makes the bytes about to be written end the way the ones read
// did. sjson's splice can consume a trailing newline, and a settings file
// silently losing its last byte on every activation is the same class of
// unasked-for edit this phase exists to stop.
func finishSettings(raw, before []byte) []byte {
	endedWithNewline := len(before) > 0 && before[len(before)-1] == '\n'
	if len(before) == 0 {
		endedWithNewline = true // a file this tool creates gets one
	}
	has := len(raw) > 0 && raw[len(raw)-1] == '\n'
	switch {
	case endedWithNewline && !has:
		return append(raw, '\n')
	case !endedWithNewline && has:
		return raw[:len(raw)-1]
	}
	return raw
}

// indentIfNew reindents a document this tool created from nothing. There are no
// developer bytes in it to preserve at that point, and sjson splices compactly,
// so without this a fresh machine gets its settings.json as a single line.
func indentIfNew(raw, before []byte) []byte {
	if len(before) != 0 {
		return raw
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return raw
	}
	return append(out, '\n')
}

func writeSettings(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("activation: creating %s: %w", filepath.Dir(path), err)
	}
	if err := atomicfile.Write(path, raw, 0o644); err != nil {
		return fmt.Errorf("activation: writing %s: %w", path, err)
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func copyOf(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sha256Of(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
