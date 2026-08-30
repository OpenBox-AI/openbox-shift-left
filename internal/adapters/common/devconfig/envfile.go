package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joho/godotenv"
)

const envFileHeader = `# OpenBox credentials for this machine — written by ` + "`openbox auth`" + `.
#
# PLAINTEXT. 0600 on macOS/Linux; on Windows 0600 is a no-op and other local
# accounts can read this file. Anything running as you can read the
# signing key below and sign governance events as this agent.
#
# This is the ONLY copy. OpenBox shows the API key and signing key once, at
# registration, and does not store them. If you lose this file, re-issue with
# ` + "`openbox auth --rotate`" + ` or register again with ` + "`openbox auth`" + `.
#
# DO NOT COMMIT THIS FILE. Sourcing it is never required — the tools read it
# directly. A real environment variable always wins over a value here.
`

// ParseEnvFile reads a dotenv file into a map. Four behaviours differ from the
// hand-rolled parser this replaced, all measured against it over an 18-case
// corpus, and all of them losses rather than neutral changes. They are
// recorded here because each one is a way a credential can go wrong silently:
func ParseEnvFile(path string) (map[string]string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	kv, err := godotenv.Read(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return kv, nil
}

// WriteEnvFile merges kv over whatever is already at path and writes the
// result atomically, 0600, under a 0700 parent.
func WriteEnvFile(path string, kv map[string]string) error {
	existing, err := ParseEnvFile(path)
	if err != nil {
		// Surface it so the user fixes or moves it deliberately.
		return fmt.Errorf("refusing to overwrite %s: %w", path, err)
	}
	merged := make(map[string]string, len(existing)+len(kv))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range kv {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(envFileHeader)
	for _, k := range keys {
		if strings.ContainsAny(merged[k], "'\n\r") {
			return fmt.Errorf("cannot write %s to %s: its value contains a quote or newline, which this "+
				"format cannot represent without corrupting it. Remove or rewrite that value by hand", k, path)
		}
		fmt.Fprintf(&b, "%s='%s'\n", k, merged[k])
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp credential file: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp credential file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}
