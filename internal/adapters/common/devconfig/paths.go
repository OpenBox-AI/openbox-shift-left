package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// paths.go — one config directory on every operating system.
//
// Configuration lives in ~/.openbox/ (ADR-0015). Before this, it lived under
// os.UserConfigDir(), which resolves to three different places —
// ~/Library/Application Support/openbox, ~/.config/openbox, %AppData%\openbox —
// for one product with one config file. ~/.aws, ~/.kube and ~/.docker all put a
// dot-directory in $HOME on all three OSes (including Windows, where it is
// C:\Users\<you>\.openbox\), so the convention is well-trodden.
//
// Scope of the move: `dev.json`, `approver.json` and the new `.env`. Runtime
// state — the spool, the policy bundle, the enforcement and advisory logs —
// still lives under os.UserConfigDir() and is not relocated here. Moving it too
// would touch every hot-path reader for no behavioural gain; docs state the
// split rather than implying one directory holds everything.
//
// No build tags. os.UserHomeDir() is platform-independent, which is the whole
// reason this replaced a keychain design that needed a _windows.go file.

// EnvHome relocates the whole OpenBox config directory. It exists for tests
// (t.TempDir()), for containers with an unusual HOME, and for anyone who wants
// their credentials somewhere other than $HOME.
const EnvHome = "OPENBOX_HOME"

// Home is the OpenBox config directory: $OPENBOX_HOME when set, else
// $HOME/.openbox.
//
// It never creates the directory — a read path must be able to ask where a file
// would be without making anything on disk. ensureHome does the creating, and
// only write paths call it.
//
// The error case is real rather than theoretical: os.UserHomeDir() fails on a CI
// runner or in a container with no HOME set, and the error says what to do about
// it instead of returning a path that would silently resolve to "/.openbox".
func Home() (string, error) {
	if p := os.Getenv(EnvHome); p != "" {
		// A relative OPENBOX_HOME would resolve against the process working
		// directory, which for a hook is whatever project the tool happens to
		// be in — so the same config would resolve to different files per
		// session, and a write could land inside a repo. Refuse it.
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

// ensureHome returns Home(), creating it 0700 if absent. Write paths only.
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

// EnvFilePath is ~/.openbox/.env — the credential file (ADR-0015). An error
// resolving Home() yields an empty path, so a caller that ignores the error
// gets "no file" rather than a wrong file.
func EnvFilePath() (string, error) {
	dir, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env"), nil
}

// DevConfigPath is where to READ the dev config: $OPENBOX_CONFIG when set, else
// ~/.openbox/dev.json, falling back to the legacy location while an unmigrated
// file still lives there (see resolveConfigPath).
//
// The env override keeps precedence because it names the file directly and
// predates this layout; every existing test and operator override keeps working.
func DevConfigPath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	return resolveConfigPath("dev.json")
}

// DevConfigWritePath is where to WRITE the dev config — always the new location,
// never the legacy one, so a write can never re-entrench the old path.
//
// Callers that write must run MigrateLegacyConfig first. WriteConfig merges over
// whatever is at the path it is given, so writing to a fresh new-location file
// while an unmigrated legacy file holds the user's posture would silently reset
// that posture to defaults. Migrating first makes the merge base correct.
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

// ApproverConfigPath is where to READ the approver config, with the same
// legacy fallback as DevConfigPath.
func ApproverConfigPath() (string, error) {
	if p := os.Getenv(EnvApproverConfigPath); p != "" {
		return p, nil
	}
	return resolveConfigPath("approver.json")
}

// ApproverConfigWritePath is where to WRITE the approver config — always the new
// location. Same migrate-first contract as DevConfigWritePath.
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
// READ, and always returns the new one for a write.
//
// This exists because upgrading the binary must not silently ungovern a machine.
// MigrateLegacyConfig runs from `auth` and `init` — write commands — so between
// installing a new binary and running one of them, every hook would resolve
// ~/.openbox/dev.json, find nothing, and fail open into an unconfigured state:
// no DID, no credentials, no events, exit 0. The install would look fine and
// produce nothing, which is the exact failure mode ADR-0016 objects to elsewhere.
//
// So a read prefers the new file, falls back to an existing legacy file, and
// otherwise returns the new path (a missing file is not an error anywhere in this
// package). Writes are unaffected: with neither file present the new path is
// returned, so nothing new is ever written to the legacy directory.
//
// Cost is one os.Stat per config resolution on the hot path, next to the file
// read it precedes. It disappears for anyone whose config has been migrated,
// because the first Stat hits.
func resolveConfigPath(name string) (string, error) {
	dir, err := Home()
	if err != nil {
		// No resolvable home: fall back to the legacy location rather than
		// returning an error the hook would have to fail open around. An
		// operator in this state gets the error from Home() via the write path.
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

// legacyConfigDir is where dev.json and approver.json lived before ADR-0015:
// <os.UserConfigDir()>/openbox. Kept for migration (migrate.go) and for the
// runtime-state files that still live there.
//
// The HOME/.config fallback mirrors what DefaultConfigPath did, so a machine
// where os.UserConfigDir() fails still finds a file written by an older binary.
func legacyConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox")
}
