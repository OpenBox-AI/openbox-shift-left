package devconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// migrate.go — carry dev.json and approver.json from the pre-that decision
// location.
//
// Config migrates itself; credentials do not. That asymmetry is deliberate and
// worth naming here, because it is the first thing a reader wonders: copying a
// JSON file needs no platform-specific code, whereas reading the OS keychain
// would mean shipping the very `security`/`secret-tool` paths that decision
// deletes, for one run per machine. So keychain credentials stay stranded by
// decision (recovery is `auth --rotate`, a manual keychain read, or
// re-registration) while the config a user spent time tuning follows them
// automatically.
//
// Non-destructive on purpose: the legacy file is READ and left in place. Deleting
// it would make a rollback to an older binary lossy — the old binary reads the
// old path — and buys nothing but tidiness.

// MigrateLegacyConfig copies dev.json and approver.json from the legacy
// <os.UserConfigDir()>/openbox directory into Home(), when and only when the new
// file is absent and the legacy one exists.
//
// Returns the basenames it migrated, so the caller can log one line naming both
// paths. Idempotent: a second run finds the new file present and does nothing.
//
// Presence of the new file always wins. A user who has already run the new
// binary has a current config, and re-copying a stale legacy file over it would
// revert whatever they last set.
func MigrateLegacyConfig() ([]string, error) {
	newHome, err := Home()
	if err != nil {
		return nil, err
	}
	legacy := legacyConfigDir()
	// Same directory (an operator pointing OPENBOX_HOME at the old location)
	// means there is nothing to move and copying would be a self-overwrite.
	if filepath.Clean(legacy) == filepath.Clean(newHome) {
		return nil, nil
	}

	var migrated []string
	for _, name := range []string{"dev.json", "approver.json"} {
		// Honour an explicit path override: someone who set OPENBOX_CONFIG has
		// named the file they want and is not asking to be migrated.
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
			// An unreadable legacy file is surfaced rather than swallowed: the
			// alternative is a silent fresh start that loses the user's posture
			// (enforce, capture settings) with no indication it happened.
			return migrated, fmt.Errorf("read legacy config %s: %w", src, err)
		}
		if _, err := ensureHome(); err != nil {
			return migrated, err
		}
		// 0600 to match what both writers use; these files name credential
		// coordinates even though they hold no credential.
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			return migrated, fmt.Errorf("write %s: %w", dst, err)
		}
		migrated = append(migrated, name)
	}
	return migrated, nil
}

// LegacyConfigPaths reports where the pre-that decision files live, for docs,
// the migration note and `openbox doctor`. It states locations; it reads
// nothing.
//
// Callers (and the migration note in docs/getting-started.md) enumerate legacy
// paths from here rather than from memory, so the two cannot drift.
func LegacyConfigPaths() (devJSON, approverJSON, secretsJSON string) {
	dir := legacyConfigDir()
	return filepath.Join(dir, "dev.json"),
		filepath.Join(dir, "approver.json"),
		// The opt-in file secret backend deleted with the keychain. Nothing
		// reads it any more, but it is a live plaintext copy of credentials
		// until someone deletes it, which is why it is named here.
		filepath.Join(dir, "secrets.json")
}
