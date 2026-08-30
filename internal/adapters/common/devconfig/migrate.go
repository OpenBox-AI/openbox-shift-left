package devconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config migrates itself; credentials do not.

// MigrateLegacyConfig copies dev.json and approver.json from the legacy
// <os.UserConfigDir()>/openbox directory into Home(), when and only when the
// new file is absent and the legacy one exists.
func MigrateLegacyConfig() ([]string, error) {
	newHome, err := Home()
	if err != nil {
		return nil, err
	}
	legacy := legacyConfigDir()
	if filepath.Clean(legacy) == filepath.Clean(newHome) {
		return nil, nil
	}

	var migrated []string
	for _, name := range []string{"dev.json", "approver.json"} {
		switch name {
		case "dev.json":
			if os.Getenv(EnvConfigPath) != "" {
				continue
			}
		case "approver.json":
			if os.Getenv(EnvApproverConfigPath) != "" {
				continue
			}
		}

		dst := filepath.Join(newHome, name)
		if _, err := os.Stat(dst); err == nil {
			continue // already migrated, or written fresh by this binary
		} else if !errors.Is(err, os.ErrNotExist) {
			return migrated, fmt.Errorf("stat %s: %w", dst, err)
		}

		src := filepath.Join(legacy, name)
		raw, err := os.ReadFile(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // nothing to migrate for this file
			}
			return migrated, fmt.Errorf("read legacy config %s: %w", src, err)
		}
		if _, err := ensureHome(); err != nil {
			return migrated, err
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			return migrated, fmt.Errorf("write %s: %w", dst, err)
		}
		migrated = append(migrated, name)
	}
	return migrated, nil
}

// LegacyConfigPaths reports where the pre-that decision files live, for docs,
// the migration note and `openbox doctor`. Callers (and the migration note in
// docs/getting-started.md) enumerate legacy paths from here rather than from
// memory, so the two cannot drift.
func LegacyConfigPaths() (devJSON, approverJSON, secretsJSON string) {
	dir := legacyConfigDir()
	return filepath.Join(dir, "dev.json"),
		filepath.Join(dir, "approver.json"),
		filepath.Join(dir, "secrets.json")
}
