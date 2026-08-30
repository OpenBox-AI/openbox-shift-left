package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joho/godotenv"
)

// envfile.go — the dotenv codec for ~/.openbox/.env (ADR-0015).
//
// The READ side is joho/godotenv at its defaults (D-OSS-7). That reverses
// ADR-0015's "hand-rolled rather than a dependency", deliberately and with the
// consequences measured rather than assumed — see ParseEnvFile's comment for
// exactly what changed. The WRITE side is still ours: it has to emit the header
// below and merge unknown keys, neither of which the library does.
//
// The codec is deliberately a plain map[string]string with no knowledge of which
// keys are credentials. Precedence — real env var beats file — belongs to
// ResolveCredentials, and restricting the written key set to secrets belongs to
// `openbox auth`. Keeping policy out of the codec is what makes it testable
// without touching the environment.

// envFileHeader is written above the keys on every WriteEnvFile.
//
// It is a security control, not decoration. This file is the ONLY copy of
// credentials the backend shows exactly once, and it is plaintext — a human who
// opens it should learn both facts here rather than from an ADR they will not
// read.
const envFileHeader = `# OpenBox credentials for this machine — written by ` + "`openbox auth`" + `.
#
# PLAINTEXT. 0600 on macOS/Linux; on Windows 0600 is a no-op and other local
# accounts can read this file (ADR-0015). Anything running as you can read the
# signing key below and sign governance events as this agent.
#
# This is the ONLY copy. OpenBox shows the API key and signing key once, at
# registration, and does not store them. If you lose this file, re-issue with
# ` + "`openbox auth --rotate`" + ` or register again with ` + "`openbox auth`" + `.
#
# DO NOT COMMIT THIS FILE. Sourcing it is never required — the tools read it
# directly. A real environment variable always wins over a value here.
`

// ParseEnvFile reads a dotenv file into a map.
//
// A missing file is an empty map and a nil error: no credentials configured is a
// legitimate state that the caller reports in its own words, not an I/O failure.
// WriteEnvFile also relies on it for the first write to a fresh path.
//
// Parsing is godotenv's, at its defaults. Four behaviours differ from the
// hand-rolled parser this replaced, all measured against it over an 18-case
// corpus, and all of them LOSSES rather than neutral changes. They are recorded
// here because each one is a way a credential can go wrong silently:
//
//   - a DUPLICATE key is last-wins, not an error. The old parser refused, on the
//     reasoning that two lines setting one credential means the user believes
//     something the file does not say — and the failure surfaces much later as an
//     unexplained 401 from the key that lost. That refusal is gone;
//   - `$VAR` and `${VAR}` are EXPANDED in unquoted and double-quoted values, and
//     `\n`-style escapes are expanded in double-quoted ones. A credential
//     containing a dollar sign is silently rewritten. Base64 and `obx_` keys use
//     no `$`, so the two secrets this file is for are unaffected — a hand-added
//     value is not;
//   - a `#` after a value starts a comment, so `KEY=abc # note` yields `abc`. The
//     old parser treated it as data;
//   - `=value` (empty key) is accepted rather than refused.
//
// And one that is a disclosure rather than a parse difference: godotenv's error
// for a malformed line ECHOES THAT LINE. On a file whose whole purpose is
// credentials, a line that is a bare secret puts the secret in the error string,
// and from there into whatever logged it. The old parser named the file and line
// number and never the content, and its test asserted exactly that.
//
// All five are accepted: the owner's ruling is to take the package's default
// behaviour and not work around it. Nothing here compensates for them, on
// purpose — a wrapper that restored the old semantics would mean the dependency
// bought nothing while hiding where the behaviour actually comes from.
func ParseEnvFile(path string) (map[string]string, error) {
	// The missing-file contract is this function's own, not something to delegate:
	// godotenv.Read surfaces the open error, and callers here treat "no file" as
	// "no credentials".
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

// WriteEnvFile merges kv over whatever is already at path and writes the result
// atomically, 0600, under a 0700 parent.
//
// Merge, not replace, and that matters more under the secrets-only split than it
// would otherwise (ADR-0015). `openbox auth` writes exactly three keys, so any
// other key in the file was put there by a human — someone who hand-added
// OPENBOX_AGENT_DID to override a coordinate has authored a key this writer must
// keep, even though nothing here will ever write it. Dropping it would silently
// undo a deliberate override.
//
// Comments are not preserved: the header is rewritten from the constant above
// and a user's own comments are lost. That is the documented limit of "preserve
// unknown keys" — keys survive, annotations do not.
func WriteEnvFile(path string, kv map[string]string) error {
	existing, err := ParseEnvFile(path)
	if err != nil {
		// A file we cannot parse must not be silently overwritten — it holds
		// the only copy of credentials. Surface it so the user fixes or moves
		// it deliberately.
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
	// Stable order so a rewrite that changes one value produces a one-line diff.
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(envFileHeader)
	for _, k := range keys {
		// Single quotes: no shell expansion, and a credential never contains one.
		//
		// A value that DOES contain a single quote is refused rather than escaped.
		// The earlier version wrote shell-style `'"'"'`, which this parser — which
		// strips one outer quote pair and unescapes nothing — read back verbatim,
		// silently corrupting the value with no error. Worse, every subsequent write
		// re-serializes EVERY key, so one hand-added apostrophe would be mangled
		// further on each `openbox auth` run.
		//
		// Refusing is the right trade for a file that holds the only copy of
		// credentials: neither of the two secrets can contain a quote, so this only
		// ever fires on a key a human added, and a clear error beats silent
		// corruption. Supporting it properly means an escaping scheme on both sides,
		// which is more machinery than the format needs.
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
	// Atomic: a concurrent reader (a hook firing mid-write) sees either the old
	// file or the new one, never a truncated credential. Same temp+rename shape
	// the deleted file secret backend used, which was proven in production.
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	// Chmod BEFORE writing, so the secret never exists at a wider mode, not
	// even for the microseconds between write and chmod.
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
