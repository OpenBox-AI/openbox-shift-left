package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvHome relocates the whole OpenBox config directory.
const EnvHome = "OPENBOX_HOME"

// Home is the OpenBox config directory: $OPENBOX_HOME when set, else
// $HOME/.openbox. It never creates the directory; a read path must be able to
// ask where a file would be without making anything on disk. EnsureHome does
// the creating, and only write paths call it.
func Home() (string, error) {
	if p := os.Getenv(EnvHome); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path (got %q)", EnvHome, p)
		}
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot locate a home directory for the OpenBox config dir: set %s to an absolute path", EnvHome)
	}
	return filepath.Join(home, ".openbox"), nil
}

func ensureHome() (string, error) {
	dir, err := Home()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// EnvFilePath is ~/.openbox/.env; the credential file.
func EnvFilePath() (string, error) {
	dir, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env"), nil
}

// DevConfigPath is where to read the dev config: $OPENBOX_CONFIG when set,
// else ~/.openbox/dev.json, falling back to the legacy location while an
// unmigrated file still lives there (see resolveConfigPath).
func DevConfigPath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	return resolveConfigPath("dev.json")
}

// DevConfigWritePath is where to write the dev config; always the new
// location, never the legacy one, so a write can never re-entrench the old
// path. Callers that write must run MigrateLegacyConfig first.
func DevConfigWritePath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	dir, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dev.json"), nil
}

// ApproverConfigPath is where to read the approver config, with the same
// legacy fallback as DevConfigPath.
func ApproverConfigPath() (string, error) {
	if p := os.Getenv(EnvApproverConfigPath); p != "" {
		return p, nil
	}
	return resolveConfigPath("approver.json")
}

// ApproverConfigWritePath is where to write the approver config; always the
// new location.
func ApproverConfigWritePath() (string, error) {
	if p := os.Getenv(EnvApproverConfigPath); p != "" {
		return p, nil
	}
	dir, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "approver.json"), nil
}

// resolveConfigPath picks between the new location and the legacy one for a
// read, and always returns the new one for a write. This exists because
// upgrading the binary must not silently ungovern a machine.
func resolveConfigPath(name string) (string, error) {
	dir, err := Home()
	if err != nil {
		return filepath.Join(legacyConfigDir(), name), nil
	}
	newPath := filepath.Join(dir, name)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, nil
	}
	legacy := filepath.Join(legacyConfigDir(), name)
	if legacy != newPath {
		if _, err := os.Stat(legacy); err == nil {
			return legacy, nil
		}
	}
	return newPath, nil
}

func legacyConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox")
}
