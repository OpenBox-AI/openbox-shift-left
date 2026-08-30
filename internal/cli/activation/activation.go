// Package activation owns the developer's tool-settings environment block:
// which keys OpenBox writes there, what was there before, and how to put it
// back.
//
//	the lane's own set is the developer's or their org's and is never touched -
//	not on install, not on removal, not when the whole env block is rewritten.
//	Removing one lane must leave the others working.
//	before OpenBox" is captured on the FIRST activation and can never be
package activation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
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

	settings, beforeRaw, err := readSettings(settingsPath)
	if err != nil {
		return applied, err
	}
	env, err := envBlock(settings, settingsPath)
	if err != nil {
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

	for _, key := range sortedKeys(desired) {
		want := desired[key]
		if existing, present := env[key]; present {
			if s := asString(existing); s != want {
				applied.Replaced = append(applied.Replaced, fmt.Sprintf("%s: %s -> %s", key, s, want))
			}
		}
		if _, captured := entry.Original[key]; !captured {
			existing, present := env[key]
			entry.Original[key] = Original{Present: present, Value: asString(existing)}
		}
		env[key] = want
	}

	settings["env"] = env
	afterRaw, err := marshalSettings(settings)
	if err != nil {
		return applied, err
	}

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

	settings, _, err := readSettings(settingsPath)
	if err != nil {
		return out, err
	}
	env, err := envBlock(settings, settingsPath)
	if err != nil {
		return out, err
	}

	type action struct {
		key      string
		original Original
	}
	var todo []action
	for _, key := range sortedKeys(entry.Managed) {
		original := entry.Original[key]
		current, present := env[key]
		switch {
		case !present:
			continue
		case asString(current) == entry.Managed[key]:
		case original.Present && asString(current) == original.Value:
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
		return out, fmt.Errorf("activation: %s changed since OpenBox set %s — refusing to overwrite. "+
			"Review the value, or re-run with --force-restore to restore what was there before",
			strings.Join(out.Conflicts, ", "), plural(len(out.Conflicts), "it", "them"))
	}

	for _, a := range todo {
		if a.original.Present {
			env[a.key] = a.original.Value
			out.Restored[a.key] = a.original.Value
			continue
		}
		delete(env, a.key)
		out.Removed = append(out.Removed, a.key)
	}

	if len(env) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = env
	}
	raw, err := marshalSettings(settings)
	if err != nil {
		return out, err
	}
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
	settings, _, err := readSettings(settingsPath)
	if err != nil {
		return nil
	}
	raw, _ := settings["env"].(map[string]any)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = asString(v)
	}
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

// readSettings loads the file as a generic map so unknown keys survive a
// round-trip. Decoding into a typed struct is how a writer silently deletes
// configuration it was never taught about.
func readSettings(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("activation: reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		return map[string]any{}, raw, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, raw, fmt.Errorf("activation: %s is not valid JSON, refusing to rewrite it: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, raw, nil
}

func envBlock(settings map[string]any, path string) (map[string]any, error) {
	value, present := settings["env"]
	if !present || value == nil {
		return map[string]any{}, nil
	}
	env, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("activation: `env` in %s is not a JSON object; refusing to rewrite it", path)
	}
	return env, nil
}

func marshalSettings(settings map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("activation: encoding settings: %w", err)
	}
	return append(out, '\n'), nil
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
