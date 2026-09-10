package devconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config migrates itself; credentials do not.

// MigrateLegacyConfig copies dev.json from the legacy
// <os.UserConfigDir()>/openbox directory into Home(), when and only when the
// new file is absent and the legacy one exists. It used to carry approver.json
// too; that went with the approver persona.
func MigrateLegacyConfig() ([]string, error) {
	newHome, err := Home()
	if err != nil {
		return nil, err
	}
	legacy := legacyConfigDir()
	if filepath.Clean(legacy) == filepath.Clean(newHome) {
		return nil, nil
	}

	if os.Getenv(EnvConfigPath) != "" {
		return nil, nil
	}
	const name = "dev.json"

	dst := filepath.Join(newHome, name)
	if _, err := os.Stat(dst); err == nil {
		return nil, nil // already migrated, or written fresh by this binary
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", dst, err)
	}

	src := filepath.Join(legacy, name)
	raw, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing to migrate
		}
		return nil, fmt.Errorf("read legacy config %s: %w", src, err)
	}
	if _, err := ensureHome(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", dst, err)
	}
	return []string{name}, nil
}

// LegacyConfigPaths reports where the pre-that decision files live, for docs,
// the migration note and `openbox doctor`. Callers (and the migration note in
// docs/getting-started.md) enumerate legacy paths from here rather than from
// memory, so the two cannot drift.
//
// approver.json is no longer among them: nothing reads it, so naming it here
// would tell a reader to go looking for state this tool has no opinion about.
func LegacyConfigPaths() (devJSON, secretsJSON string) {
	dir := legacyConfigDir()
	return filepath.Join(dir, "dev.json"), filepath.Join(dir, "secrets.json")
}
