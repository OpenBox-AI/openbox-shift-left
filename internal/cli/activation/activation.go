// Package activation owns the developer's tool-settings environment block: which
// keys OpenBox writes there, what was there before, and how to put it back.
//
// It replaces a single-key mechanism that served one lane. Three lanes now write
// that block — the gateway's base URL, the telemetry receiver's ~13 OTel keys,
// the transport relay's proxy and CA keys — and copying the one-key writer three
// times is the drift this repo already paid for once, on the enforcement path.
//
// ── THE TWO RULES THAT SHAPE EVERY FUNCTION HERE ─────────────────────────────
//
//  1. **Ownership is per lane, and everything else is preserved.** A key outside
//     the lane's own set is the developer's or their org's and is never touched —
//     not on install, not on removal, not when the whole env block is rewritten.
//     Removing one lane must leave the others working.
//
//  2. **First writer wins, per lane.** The value remembered as "what was there
//     before OpenBox" is captured on the FIRST activation and can never be
//     overwritten by a later one. The gateway learned this the expensive way:
//     re-running init on a different port displaced OUR OWN previous URL, which
//     was then recorded as the org's, so uninstall "restored" a loopback address
//     whose daemon it had just unloaded — and a dead localhost fails closed, so
//     the command meant to undo the relay left every model call on the machine
//     failing while announcing a successful restore.
//
// ── WHERE THE REFUSAL LIVES, AND WHY IT IS NOT ON INSTALL ────────────────────
//
// Activate REPLACES and REPORTS; Deactivate REFUSES on a value that changed
// underneath it. That asymmetry is deliberate. Redirecting the developer's tool
// is the whole purpose of installing a lane, so refusing because the key we
// exist to change is already set would refuse on precisely the machines that
// need governing — and the displaced value is not lost, it is recorded. Putting
// a value BACK over something a human has since edited is the destructive
// direction, and that is where the refusal belongs.
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
	// LaneGateway is the loopback base-URL relay (ADR-0021).
	//
	// Its env writes still go through cli/internal/gatewayservice, which shipped
	// and is socket-verified. The lane identity exists here so the election and
	// `--remove-all` can reason about all three lanes uniformly.
	LaneGateway Lane = "gateway"
	// LaneTelemetry is the local OTLP receiver (ADR-0022 `:otel:`).
	LaneTelemetry Lane = "telemetry"
	// LaneTransport is the in-path CONNECT/TLS relay (ADR-0022 `:proxy:`).
	LaneTransport Lane = "transport"
)

// recordSchema versions the on-disk record. Present so a future shape change is
// a decision rather than a silent reinterpretation of someone's originals.
const recordSchema = "openbox.dev-runtime.activation/v1"

// Original is what a key looked like before OpenBox first wrote it.
//
// Present distinguishes "absent, so removal means delete" from "empty string,
// which we must put back verbatim". Collapsing the two would turn a deliberate
// empty value into a deletion.
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
	// Never silent: something had that value pointed somewhere. On a FIRST
	// activation that something is the developer or their org; on a re-install at
	// a different address it is our own previous write, which is why this reports
	// what changed and the record — not this list — is what remembers the
	// original.
	Replaced []string
}

// Reverted reports what a removal did. Removed and Restored are different
// outcomes and are reported separately on purpose — a machine put back to what
// the org configured is not an unconfigured machine, and telling an operator
// "removed" about a value that was actually restored sends them looking for the
// wrong thing.
type Reverted struct {
	Removed   []string
	Restored  map[string]string
	Conflicts []string
}

// RecordPath is where the activation record lives.
//
// Under ~/.openbox beside the credential file, 0600. It is integrity evidence
// rather than a secret — it names every key we touched — but a displaced value
// can itself carry a credential (an org relay URL with an embedded token), so it
// gets the same mode regardless.
func RecordPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "activation.json")
}

// Activate writes desired into the settings env block on behalf of lane.
//
// Order of writes is a safety property: the RECORD goes down before the
// SETTINGS. A record written for an activation that then failed over-reports a
// lane, which costs turn events and is announced; settings written with no
// record loses the org's displaced value permanently. Both bodies are computed
// before either write, so the after-hash is known without a second record pass.
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
		// FIRST WRITER WINS. A key already in this lane's originals was captured
		// by an earlier activation; recapturing it now would record the value WE
		// wrote as the developer's own.
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

// Deactivate puts lane's keys back and forgets the lane.
//
// force overwrites a value that changed since activation. It exists because
// without it a machine whose managed value drifted could never complete a
// removal at all — and removal must not require the thing being removed to still
// be in the state we left it in.
func Deactivate(homeDir, settingsPath string, lane Lane, force bool) (Reverted, error) {
	out := Reverted{Restored: map[string]string{}}

	record, err := loadRecord(homeDir)
	if err != nil {
		return out, err
	}
	entry := record.Lanes[lane]
	if entry == nil {
		// Never installed, or already removed. Not an error: `--remove-all` runs
		// on partial state by design.
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
			// Already gone. Nothing to restore into, nothing to remove.
			continue
		case asString(current) == entry.Managed[key]:
			// Still ours.
		case original.Present && asString(current) == original.Value:
			// A previous removal got this far and then failed. Treating that as a
			// conflict would make a half-finished removal un-repeatable without
			// --force, which is the opposite of what partial-state tolerance means.
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

	// An env block we emptied is removed entirely: a settings file that had none
	// before must not gain one because OpenBox was installed and uninstalled.
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

	// Only after the settings write succeeded. Dropping the entry first would
	// lose the org's original value if the write then failed.
	delete(record.Lanes, lane)
	if err := saveRecord(homeDir, record); err != nil {
		return out, err
	}
	return out, nil
}

// ActiveLanes reports which lanes this tool has activated and not removed, in
// precedence order.
//
// It answers "what did we install", which is what `--remove-all` needs. It is
// NOT the election input — that asks a different question (where is the client
// actually routed) and is answered in producer.go from the settings file itself.
func ActiveLanes(homeDir string) []Lane {
	record, err := loadRecord(homeDir)
	if err != nil {
		// A record we cannot read means nothing we can claim. Reporting no lanes
		// makes `--remove-all` fall back to its unconditional per-lane cleanup,
		// which is idempotent, rather than acting on a guess.
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
// compute a desired value that MERGES with what is there — NO_PROXY being the
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

// ── record I/O ───────────────────────────────────────────────────────────────

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
		// REFUSE, do not start fresh. An empty record would recapture originals
		// from an env block we ourselves wrote, which is precisely the "our own
		// value remembered as the org's" bug this file exists to make impossible.
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
	// ENFORCED, not requested: the atomic writer preserves an EXISTING file's
	// mode, which is right for the developer's settings file and wrong here. A
	// record that became readable once would stay readable through every rewrite.
	return os.Chmod(path, 0o600)
}

// ── settings I/O ─────────────────────────────────────────────────────────────

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
		// A settings file we cannot parse is a file we cannot safely rewrite, and
		// clobbering it would destroy configuration nobody asked us to touch.
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

// writeSettings persists the settings file.
//
// 0644 because the tool reads it and it is the DEVELOPER's config; its
// permissions are not an assurance boundary. The write is atomic — see
// cli/internal/atomicfile for why a truncated settings file would block its own
// repair.
func writeSettings(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("activation: creating %s: %w", filepath.Dir(path), err)
	}
	if err := atomicfile.Write(path, raw, 0o644); err != nil {
		return fmt.Errorf("activation: writing %s: %w", path, err)
	}
	return nil
}

// ── small helpers ────────────────────────────────────────────────────────────

// asString renders a settings value as the string the tool would read. Settings
// JSON is the developer's, so a number or bool where a string was expected is
// their file's shape, not an error worth failing an install over.
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
