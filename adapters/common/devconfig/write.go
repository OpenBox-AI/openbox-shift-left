package devconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Update is what one `openbox init` run supplies for the dev config.
//
// A zero string or a nil pointer means "this run did not mention the setting".
// That distinction is the whole point: re-running `init` to repair hooks
// must not be able to turn enforcement off, and the only honest way to know it
// was not asked for is to record that the flag was never passed.
type Update struct {
	// Coordinates. Empty ⇒ not supplied by this run, so a re-init that
	// resolves credentials from the store without re-deriving the coordinates
	// keeps the ones already on disk.
	BaseURL    string
	DID        string
	AgentID    string
	BackendURL string

	// Posture and preferences. nil ⇒ leave whatever is on disk (or the product
	// default on a first install); non-nil ⇒ this run chose it explicitly,
	// including a deliberate downgrade.
	ContentCapture *bool
	Enforce        *bool
	Tier2          *bool
	Findings       *bool
	InstallGitHook *bool
}

// WriteConfig merges u over the config already at path and writes the result.
//
// One writer for every provider. Both adapters previously hand-rolled this,
// including the preserve-prior block, and the two copies had already drifted in
// what they preserved: the sync coordinates were carried forward under a comment
// insisting a re-init "must not silently drop" them, while the enforce posture
// right beside them was overwritten from the current run's flags. So
// `init --enforce` followed by a plain `init` dropped the developer from
// enforce to observe — exit 0, no message. The security-relevant fields got
// weaker treatment than the operational ones.
//
// The merge starts from what is on disk and overlays only what this run
// supplied, so a field nobody thought about here — including one added to
// DevConfig later — is carried forward rather than erased. Nothing is lost by
// omission.
func WriteConfig(path string, u Update) error {
	// A prior file that cannot be read or parsed leaves cfg at its zero value,
	// so this run's values are written as-is. That is the right direction for a
	// repair command: `init` must still work against a corrupted config.
	cfg, _ := Load(path)

	setString(&cfg.BaseURL, u.BaseURL)
	setString(&cfg.DID, u.DID)
	setString(&cfg.AgentID, u.AgentID)
	setString(&cfg.BackendURL, u.BackendURL)

	setBoolPtr(&cfg.Enforce, u.Enforce)
	setBool(&cfg.InstallGitHook, u.InstallGitHook)
	setBoolPtr(&cfg.ContentCapture, u.ContentCapture)
	setBoolPtr(&cfg.Tier2, u.Tier2)
	setBoolPtr(&cfg.Findings, u.Findings)

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("dev config: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("dev config: create dir: %w", err)
	}
	// 0600: coordinates are not secret, but keep them owner-only anyway.
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("dev config: write %s: %w", path, err)
	}
	return nil
}

// WouldDowngradeEnforce reports whether writing next to the config at path turns
// enforcement off, so the caller can say so out loud rather than let a posture
// change happen quietly. next is the tri-state Update.Enforce: nil never
// downgrades, which is the point.
//
// It compares RESOLVED postures, not the raw field. Under enforce-by-default
// (ADR-0016) the field is absent on exactly the configs most people have, so
// reading prior.Enforce directly would see "not enforcing" and report no
// downgrade — going silent in the common case, which is the one case this
// message exists for. Absent means enforcing, so turning it off IS a downgrade.
func WouldDowngradeEnforce(path string, next *bool) bool {
	if next == nil || *next {
		return false
	}
	prior, err := Load(path)
	if err != nil {
		// An unreadable prior config resolves to the default, which is on, so
		// writing false still weakens the effective posture.
		return true
	}
	return prior.Enforce == nil || *prior.Enforce
}

func setString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func setBoolPtr(dst **bool, v *bool) {
	if v != nil {
		*dst = v
	}
}
