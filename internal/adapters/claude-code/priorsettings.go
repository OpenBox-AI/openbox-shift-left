package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// priorSettingsSchema versions the restore record, mirroring
// internal/cli/activation's recordSchema and gatewayservice's priorEnv.
const priorSettingsSchema = "openbox.claude-code.prior-settings/v1"

// priorValue is one settings key's value from before OpenBox first wrote it.
// Present distinguishes "the key was absent, so restore means delete" from
// "the key held some JSON value, restore verbatim" -- the same split
// activation.Original draws for an env key (activation.go:41-44). Raw is
// gjson.Result.Raw: the prior value may be false, true, "yes", null, or
// anything a developer typed, and restoring with sjson.SetRawBytes puts back
// exactly those bytes rather than a bool this package reinterpreted.
type priorValue struct {
	Present bool   `json:"present"`
	Raw     string `json:"raw,omitempty"`
}

// priorSettings is the whole restore record: which settings file it guards,
// and one entry per key captured there. It is not dev.json (coordinates
// only) and not .env (secrets only) -- one store per field -- so it gets its own sibling file under ~/.openbox, named after the
// existing gateway-prior-env.json precedent (internal/cli/gatewayservice).
type priorSettings struct {
	Schema       string                `json:"schema"`
	SettingsPath string                `json:"settings_path,omitempty"`
	Keys         map[string]priorValue `json:"keys"`
}

// PriorSettingsPath is where the Claude Code adapter records a settings
// key's value from before `openbox init` forced it, so `openbox uninstall`
// can put it back. Exported so internal/cli/providers and cmd/openbox can
// name and purge it without importing the rest of this adapter.
func PriorSettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".openbox", "claude-code-prior-settings.json")
}

// loadPriorSettings reads the record. Absent is the zero value: nothing
// recorded yet, the ordinary state on a machine that never ran `init` for
// this provider. An unparsable file is refused rather than silently
// replaced, mirroring activation.loadRecord (activation.go:274-278) --
// continuing without knowing what an earlier run already captured would let
// a later `init` mistake its own forced value for the developer's original.
func loadPriorSettings(homeDir string) (priorSettings, error) {
	path := PriorSettingsPath(homeDir)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return priorSettings{Schema: priorSettingsSchema, Keys: map[string]priorValue{}}, nil
	}
	if err != nil {
		return priorSettings{}, fmt.Errorf("claude-code: reading %s: %w", path, err)
	}
	var rec priorSettings
	if err := json.Unmarshal(raw, &rec); err != nil {
		return priorSettings{}, fmt.Errorf(
			"claude-code: %s is not valid JSON, refusing to continue without knowing what was on this "+
				"machine before OpenBox: %w", path, err)
	}
	if rec.Keys == nil {
		rec.Keys = map[string]priorValue{}
	}
	rec.Schema = priorSettingsSchema
	return rec, nil
}

// savePriorSettings writes the record atomically, 0600 in a 0700 directory
// -- the same posture as activation.json (activation.go:288-298). It uses
// hookflow.AtomicWriteFile (adapter -> adapters/common is the correct import
// direction; internal/cli/atomicfile would invert it, and this adapter must
// not import internal/cli) rather than this package's own writeFileAtomic:
// that one is the settings-document committer localhooks.go already owns,
// and this is an unrelated small JSON record with its own file.
func savePriorSettings(homeDir string, rec priorSettings) error {
	path := PriorSettingsPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("claude-code: creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("claude-code: encoding %s: %w", path, err)
	}
	if err := hookflow.AtomicWriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("claude-code: writing %s: %w", path, err)
	}
	return nil
}
