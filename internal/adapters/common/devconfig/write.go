package devconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Update is what one `openbox init` run supplies for the dev config. That
// distinction is the whole point: re-running `init` to repair hooks must not
// be able to turn enforcement off, and the only honest way to know it was not
// asked for is to record that the flag was never passed.
type Update struct {
	// BaseURL coordinates.
	BaseURL    string
	DID        string
	AgentID    string
	BackendURL string

	// ContentCapture posture and preferences. Nil ⇒ leave whatever is on disk (or
	// the product default on a first install); non-nil ⇒ this run chose it
	// explicitly, including a deliberate downgrade.
	ContentCapture *bool
	Enforce        *bool
	Tier2          *bool
	Findings       *bool
	InstallGitHook *bool
}

// WriteConfig merges u over the config already at path and writes the result.
func WriteConfig(path string, u Update) error {
	// That is the right direction for a repair command: `init` must still work
	// against a corrupted config.
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
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("dev config: write %s: %w", path, err)
	}
	return nil
}

// WouldDowngradeEnforce reports whether writing next to the config at path
// turns enforcement off, so the caller can say so out loud rather than let a
// posture change happen quietly. Next is the tri-state Update.Enforce: nil
// never downgrades, which is the point.
func WouldDowngradeEnforce(path string, next *bool) bool {
	if next == nil || *next {
		return false
	}
	prior, err := Load(path)
	if err != nil {
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
