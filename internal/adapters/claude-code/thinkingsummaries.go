package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ThinkingSummariesKey is a Claude Code settings key, not an OpenBox one: the
// tool sends display:"summarized" only when it is true, and a governance
// transcript that cannot show reasoning shows an empty thinking field
// instead. The name carries no gjson/sjson path syntax, so it addresses
// itself directly with no escaper.
const ThinkingSummariesKey = "showThinkingSummaries"

// writeThinkingSummaries forces ThinkingSummariesKey to true in the
// developer's Claude Code settings, recording whatever was there before --
// once -- so RestoreThinkingSummaries can put it back.
//
// Record first, then the settings write ("install ordering is a safety
// property"): record-then-fail leaves a stale record RestoreThinkingSummaries
// treats as drift and ignores; settings-then-fail leaves a forced key with no
// way home. writeThinkingSummaries also skips the settings write entirely
// once the key is already true and the record exists, so a re-`init` against
// a converged file is byte-identical.
func writeThinkingSummaries(settingsPath, homeDir string) error {
	before, err := readClaudeSettingsRaw(settingsPath)
	if err != nil {
		return err
	}
	raw := before
	if len(raw) == 0 {
		raw = []byte("{}")
	}

	rec, err := loadPriorSettings(homeDir)
	if err != nil {
		return err
	}
	current := gjson.GetBytes(raw, ThinkingSummariesKey)
	if _, captured := rec.Keys[ThinkingSummariesKey]; !captured {
		rec.Keys[ThinkingSummariesKey] = priorValue{Present: current.Exists(), Raw: current.Raw}
		rec.SettingsPath = settingsPath
		if err := savePriorSettings(homeDir, rec); err != nil {
			return err
		}
	}

	// Byte idempotency: nothing left to change. The record above now exists
	// either way -- captured just now, or already there from an earlier run.
	if current.Type == gjson.True {
		return nil
	}

	out, err := sjson.SetBytes(raw, ThinkingSummariesKey, true)
	if err != nil {
		return fmt.Errorf("claude-code: setting %s in %s: %w", ThinkingSummariesKey, settingsPath, err)
	}
	out = finishClaudeSettingsWrite(out, before)

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("claude-code: creating %s: %w", filepath.Dir(settingsPath), err)
	}
	if err := writeFileAtomic(settingsPath, out, 0o644); err != nil {
		return fmt.Errorf("claude-code: writing %s: %w", settingsPath, err)
	}
	return nil
}

// Restored reports what RestoreThinkingSummaries did to ThinkingSummariesKey,
// so `openbox uninstall` can print exactly one of: nothing recorded, restored
// to a recorded value, deleted (it was absent before `init`), or left alone
// because the developer changed it after `init` (drift).
type Restored struct {
	// Recorded is false when this machine holds no restore record: `init`
	// never ran for this provider here, or an earlier `uninstall` already
	// removed it. The ordinary case, and not a failure.
	Recorded bool
	// Drifted is true when the current value is not literally boolean true:
	// the developer (or something else) changed it after `init`, so nothing
	// was touched. Current is gjson's raw form of what is there now, or
	// "<absent>" when the key itself is gone.
	Drifted bool
	Current string
	// Present is the recorded prior value's own shape: true means the key is
	// restored to Value (a JSON value put back verbatim); false means the key
	// was absent before `init` and is now deleted.
	Present bool
	Value   string
}

// RestoreThinkingSummaries puts ThinkingSummariesKey back to whatever
// writeThinkingSummaries found before forcing it to true.
//
// It mirrors activation.Deactivate's drift refusal (activation.go:176-197)
// but non-fatally: `openbox uninstall` must complete regardless of what one
// settings key looks like now -- its hook-surface loop already records a
// failure instead of aborting (cmd/openbox/uninstall.go:335-345) -- so drift
// here is reported and left alone rather than erroring.
func RestoreThinkingSummaries(settingsPath, homeDir string) (Restored, error) {
	rec, err := loadPriorSettings(homeDir)
	if err != nil {
		return Restored{}, err
	}
	prior, ok := rec.Keys[ThinkingSummariesKey]
	if !ok {
		return Restored{}, nil
	}

	raw, err := readClaudeSettingsRaw(settingsPath)
	if err != nil {
		return Restored{}, err
	}
	current := gjson.GetBytes(raw, ThinkingSummariesKey)
	if current.Type != gjson.True {
		return Restored{Recorded: true, Drifted: true, Current: currentOrAbsent(current)}, nil
	}

	var out []byte
	if prior.Present {
		// loadPriorSettings only validates the record's outer envelope; Raw is
		// just a Go string field inside it, so a truncated write, disk damage,
		// or a hand-edit of the record can leave it holding bytes that
		// unmarshal fine into that string but are not, on their own, a
		// complete JSON value. SetRawBytes does not check what it is given --
		// splicing that straight into the developer's real settings file would
		// corrupt it silently, and this command would still report success.
		// Refuse instead, the same discipline readClaudeSettingsRaw already
		// applies to the settings file itself. "yes" (a JSON string) must
		// still pass: the rule is well-formed JSON, not boolean.
		if !gjson.Valid(prior.Raw) {
			recPath := PriorSettingsPath(homeDir)
			return Restored{}, fmt.Errorf(
				"claude-code: %s holds a corrupted value for %s (%q is not valid JSON); refusing to write "+
					"it into %s. Fix or delete the %q entry in %s, then run `openbox uninstall` again",
				recPath, ThinkingSummariesKey, prior.Raw, settingsPath, ThinkingSummariesKey, recPath)
		}
		out, err = sjson.SetRawBytes(raw, ThinkingSummariesKey, []byte(prior.Raw))
	} else {
		out, err = sjson.DeleteBytes(raw, ThinkingSummariesKey)
	}
	if err != nil {
		return Restored{}, fmt.Errorf("claude-code: restoring %s in %s: %w", ThinkingSummariesKey, settingsPath, err)
	}
	out = finishClaudeSettingsWrite(out, raw)
	if err := writeFileAtomic(settingsPath, out, 0o644); err != nil {
		return Restored{}, fmt.Errorf("claude-code: writing %s: %w", settingsPath, err)
	}
	return Restored{Recorded: true, Present: prior.Present, Value: prior.Raw}, nil
}

func currentOrAbsent(r gjson.Result) string {
	if !r.Exists() {
		return "<absent>"
	}
	return r.Raw
}

// readClaudeSettingsRaw reads a Claude Code settings file, refusing one that
// exists but is not valid JSON: sjson edits a malformed document without
// complaint, and a settings file that cannot be parsed stops every hook in
// it applying -- including the developer's own -- while reporting nothing.
// This is the same validity posture writeHooks already applies
// (localhooks.go:174-186) and activation.readSettings applies to its own
// file (activation.go:310-328); this package's own settings write needs a
// second, independent reader because localhooks.go stays untouched.
//
// Absent returns (nil, nil): nothing to preserve, and not a failure.
func readClaudeSettingsRaw(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("claude-code: read %s: %w", path, err)
	}
	if !gjson.ValidBytes(raw) {
		detail := json.Unmarshal(raw, new(any))
		return nil, fmt.Errorf("claude-code: %s exists but is not valid JSON; fix or remove it first: %w", path, detail)
	}
	return raw, nil
}

// finishClaudeSettingsWrite mirrors writeHooks' own newline and indentation
// discipline (localhooks.go:235-246), so this second writer of the same file
// cannot disagree with the first about what "unchanged" looks like: a file
// this call created gets readable indentation, and a file that already ended
// in a newline keeps one even though sjson's splice can consume it.
func finishClaudeSettingsWrite(out, before []byte) []byte {
	if len(before) == 0 {
		var doc any
		if json.Unmarshal(out, &doc) == nil {
			if pretty, err := json.MarshalIndent(doc, "", "  "); err == nil {
				return append(pretty, '\n')
			}
		}
		return out
	}
	if before[len(before)-1] == '\n' && (len(out) == 0 || out[len(out)-1] != '\n') {
		return append(out, '\n')
	}
	return out
}
